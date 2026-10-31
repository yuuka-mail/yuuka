package main

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/outbox"
	"github.com/yuuka-mail/yuuka/internal/transport"
)

// Transports are process configuration, not a table (§4, §9.3): a name,
// as identities.transport stores it, and the exit it stands for. The
// environment lists the names and gives each its own variables:
//
//	YUUKA_TRANSPORTS=cloudflare,work
//	YUUKA_TRANSPORT_CLOUDFLARE_HOST=smtp.mx.cloudflare.net
//	YUUKA_TRANSPORT_CLOUDFLARE_USERNAME=...
//	YUUKA_TRANSPORT_CLOUDFLARE_PASSWORD_FILE=/run/secrets/cloudflare-smtp
//	YUUKA_TRANSPORT_CLOUDFLARE_CAPS=cloudflare
//
// The variable infix is the name uppercased with '.' and '-' turned into
// '_'. The password is read from a file, never from the environment, so
// it is not in `docker inspect`, in /proc/<pid>/environ or in a dump of
// the process's configuration; it is read once at startup.

// transportName is the shape identities.transport accepts.
var transportName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// transportConfig is one exit as configured.
type transportConfig struct {
	Name         string
	Host         string
	Port         int
	Username     string
	PasswordFile string
	Caps         transport.Caps
}

// outboxConfig is the sending side of serve.
type outboxConfig struct {
	Transports   []transportConfig
	Workers      int
	PollInterval time.Duration
}

// transportVar is the variable of transport name with suffix.
func transportVar(name, suffix string) string {
	infix := strings.NewReplacer(".", "_", "-", "_").Replace(strings.ToUpper(name))
	return "YUUKA_TRANSPORT_" + infix + "_" + suffix
}

func loadOutboxConfig(getenv func(string) string) (outboxConfig, error) {
	var errs []error
	c := outboxConfig{Workers: 2, PollInterval: 30 * time.Second}
	if v := getenv("YUUKA_OUTBOX_WORKERS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 32 {
			errs = append(errs, errors.New("YUUKA_OUTBOX_WORKERS: want 1 to 32"))
		}
		c.Workers = n
	}
	if v := getenv("YUUKA_OUTBOX_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			errs = append(errs, errors.New("YUUKA_OUTBOX_POLL_INTERVAL: want a positive duration like 30s"))
		}
		c.PollInterval = d
	}

	vars := map[string]string{} // variable infix -> name, to catch collisions
	for _, name := range strings.Split(getenv("YUUKA_TRANSPORTS"), ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !transportName.MatchString(name) {
			errs = append(errs, fmt.Errorf("YUUKA_TRANSPORTS: %q is not a transport name (letters, digits, '.', '_', '-')", name))
			continue
		}
		key := transportVar(name, "")
		if other, ok := vars[key]; ok {
			errs = append(errs, fmt.Errorf("YUUKA_TRANSPORTS: %q and %q share the variables %s*", other, name, key))
			continue
		}
		vars[key] = name
		t, err := loadTransport(getenv, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		c.Transports = append(c.Transports, t)
	}
	return c, errors.Join(errs...)
}

func loadTransport(getenv func(string) string, name string) (transportConfig, error) {
	var errs []error
	get := func(suffix string) (string, string) {
		k := transportVar(name, suffix)
		return k, getenv(k)
	}
	t := transportConfig{Name: name, Port: 465}
	hostVar, host := get("HOST")
	if host == "" {
		errs = append(errs, fmt.Errorf("%s is required", hostVar))
	}
	t.Host = host
	if k, v := get("PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			errs = append(errs, fmt.Errorf("%s: want a port number", k))
		}
		t.Port = p
	}
	userVar, user := get("USERNAME")
	passVar, pass := get("PASSWORD_FILE")
	switch {
	case user != "" && pass == "":
		errs = append(errs, fmt.Errorf("%s is required with %s", passVar, userVar))
	case user == "" && pass != "":
		errs = append(errs, fmt.Errorf("%s needs %s", passVar, userVar))
	}
	t.Username, t.PasswordFile = user, pass
	if k, v := get("PASSWORD"); v != "" {
		errs = append(errs, fmt.Errorf("%s: put the password in a file and name it in %s", k, passVar))
	}
	capsVar, caps := get("CAPS")
	c, err := parseCaps(caps)
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", capsVar, err))
	}
	t.Caps = c
	return t, errors.Join(errs...)
}

// parseCaps reads a caps preset or an explicit list. It is required: the
// caps decide which header fields are stripped from every message sent
// through the exit (transport.StripHeaders), so a default would silently
// change the bytes of someone's mail.
func parseCaps(s string) (transport.Caps, error) {
	switch s {
	case "":
		return transport.Caps{}, errors.New("required: cloudflare, standard, none, or a list of message-id, return-path, dkim, status-query")
	case "cloudflare":
		return transport.CloudflareCaps(), nil
	case "standard":
		// A submission server that delivers what it is given: our
		// Message-ID survives and our reverse-path is where bounces go.
		// It signs with its own DKIM key; ours is not assumed to survive.
		return transport.Caps{CallerMessageID: true, CallerReturnPath: true}, nil
	case "none":
		return transport.Caps{}, nil
	}
	var c transport.Caps
	for _, f := range strings.Split(s, ",") {
		switch strings.TrimSpace(f) {
		case "message-id":
			c.CallerMessageID = true
		case "return-path":
			c.CallerReturnPath = true
		case "dkim":
			c.CallerDKIM = true
		case "status-query":
			c.StatusQuery = true
		default:
			return c, fmt.Errorf("unknown cap %q", f)
		}
	}
	return c, nil
}

// openTransports reads each password file and builds the exits.
func openTransports(ts []transportConfig, readFile func(string) ([]byte, error)) (map[string]outbox.Transport, error) {
	out := make(map[string]outbox.Transport, len(ts))
	var errs []error
	for _, t := range ts {
		var password string
		if t.PasswordFile != "" {
			b, err := readFile(t.PasswordFile)
			if err != nil {
				errs = append(errs, fmt.Errorf("transport %s: password file: %w", t.Name, err))
				continue
			}
			// One trailing newline is what an editor or echo leaves.
			b = bytes.TrimSuffix(bytes.TrimSuffix(b, []byte("\n")), []byte("\r"))
			if len(b) == 0 {
				errs = append(errs, fmt.Errorf("transport %s: password file %s is empty", t.Name, t.PasswordFile))
				continue
			}
			password = string(b)
		}
		smtp, err := transport.NewSMTP(transport.Config{
			Name:     t.Name,
			Host:     t.Host,
			Port:     t.Port,
			Username: t.Username,
			Password: password,
			Caps:     t.Caps,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("transport %s: %w", t.Name, err))
			continue
		}
		out[t.Name] = smtp
	}
	return out, errors.Join(errs...)
}
