package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/transport"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

const transportUsage = "usage: yuuka transport add NAME --host H [--port 465] [--auth plain|none] [--username U] " +
	"[--password-file PATH|-] --caps cloudflare|standard|none|<list> | " +
	"yuuka transport set NAME [--host H] [--port P] [--auth plain|none] [--username U] [--password-file PATH|-] [--caps C] | " +
	"yuuka transport ls [--all] | yuuka transport rm NAME | yuuka transport test NAME"

// transportCmd is `yuuka transport`: the outgoing transports in the
// database and their sealed passwords (§17, §10.2). Configuring one needs
// the KEK (YUUKA_KEK_FILE), which seals the password; listing and
// removing do not.
func (h *host) transportCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(transportUsage)
	}
	if err := refuseSecretArgs(args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "add", "set":
		fs := flag.NewFlagSet("transport "+args[0], flag.ContinueOnError)
		host := fs.String("host", "", "SMTP server; implicit TLS only")
		port := fs.Int("port", 465, "port")
		auth := fs.String("auth", "", "plain (AUTH PLAIN with --username and a password) or none (default: plain with a username)")
		username := fs.String("username", "", "AUTH PLAIN user name")
		passwordFile := fs.String("password-file", "", "file holding the password, or - for standard input; never the password itself")
		caps := fs.String("caps", "", "what the exit lets yuuka set: cloudflare, standard, none, or a list of message-id, return-path, dkim, status-query")
		if err := parseInterspersed(fs, args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New(transportUsage)
		}
		f := transportFlags{host: *host, port: *port, auth: *auth, username: *username, passwordFile: *passwordFile, caps: *caps}
		f.given = map[string]bool{}
		fs.Visit(func(fl *flag.Flag) { f.given[fl.Name] = true })
		return h.transportWrite(ctx, args[0] == "add", fs.Arg(0), f)
	case "ls":
		fs := flag.NewFlagSet("transport ls", flag.ContinueOnError)
		all := fs.Bool("all", false, "include removed transports")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New(transportUsage)
		}
		return h.transportList(ctx, *all)
	case "rm", "test":
		fs := flag.NewFlagSet("transport "+args[0], flag.ContinueOnError)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New(transportUsage)
		}
		if args[0] == "rm" {
			return h.transportRemove(ctx, fs.Arg(0))
		}
		return h.transportTest(ctx, fs.Arg(0))
	}
	return errors.New(transportUsage)
}

// refuseSecretArgs stops a secret given as an argument value, before
// anything else: arguments land in shell history and in ps (§17.6). The
// value is not repeated in the error.
func refuseSecretArgs(args []string) error {
	for _, a := range args {
		if a == "--" {
			return nil
		}
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if strings.HasPrefix(a, "-") && (name == "password" || name == "token" || name == "secret") {
			return fmt.Errorf("-%s: a secret is never an argument, which shell history and ps would keep; "+
				"put it in a file and give --password-file PATH, or pipe it to --password-file -", name)
		}
	}
	return nil
}

type transportFlags struct {
	host, auth, username, passwordFile, caps string
	port                                     int
	// given are the flags on the command line; set changes only those.
	given map[string]bool
}

// transportWrite is `transport add` and `transport set`. add configures a
// new name, a placeholder or a removed transport, and refuses one that is
// configured; set changes the given fields of an existing one. Either way
// the row is read, then written at the revision read, so a change made in
// between is an error rather than overwritten. A new password, and any
// change of destination, seals a new secret under a new id (§17.4).
func (h *host) transportWrite(ctx context.Context, add bool, name string, f transportFlags) error {
	cur, err := h.db.TransportByName(ctx, name)
	exists := err == nil
	switch {
	case errors.Is(err, store.ErrNotFound):
		if !add {
			return fmt.Errorf("no transport %s; `yuuka transport add` it", name)
		}
	case err != nil:
		return err
	case add && cur.ConfiguredAt != nil && cur.DisabledAt == nil:
		return fmt.Errorf("transport %s is already configured; change it with `yuuka transport set`", name)
	}
	configured := exists && cur.ConfiguredAt != nil
	// add always takes the whole configuration; set starts from what is
	// there, when something is.
	base := configured && !add

	c := store.TransportConfig{Name: name, Port: 465}
	if base {
		c = store.TransportConfig{Name: name, Host: cur.Host, Port: cur.Port, Auth: cur.Auth, Username: cur.Username, Caps: cur.Caps}
	}
	if f.given["host"] || !base {
		if f.host == "" {
			return errors.New("--host is required")
		}
		c.Host, err = secret.CanonicalHost(f.host)
		if err != nil {
			return fmt.Errorf("--host: %w", err)
		}
	}
	if f.given["port"] || !base {
		c.Port = f.port
	}
	if f.given["username"] {
		c.Username = f.username
	}
	switch {
	case f.given["auth"]:
		c.Auth = f.auth
	case f.given["username"] || !base:
		c.Auth = store.TransportAuthNone
		if c.Username != "" {
			c.Auth = store.TransportAuthPlain
		}
	}
	if f.given["caps"] || !base {
		caps, err := parseCaps(f.caps)
		if err != nil {
			return fmt.Errorf("--caps: %w", err)
		}
		c.Caps = capNames(caps)
	}

	moved := !configured || cur.Host != c.Host || cur.Port != c.Port || cur.Auth != c.Auth || cur.Username != c.Username
	switch {
	case c.Auth != store.TransportAuthPlain && f.passwordFile != "":
		return errors.New("--password-file needs auth plain and a --username")
	case c.Auth == store.TransportAuthPlain && f.passwordFile == "" && (moved || cur.SecretID == ""):
		if configured && cur.SecretID != "" {
			return fmt.Errorf("this moves transport %s to %s: a password is only ever sent where it was set for, "+
				"so give it again with --password-file PATH or -", name, destination(c))
		}
		return errors.New("auth plain needs the password: --password-file PATH, or - to read it from standard input")
	}

	var sealed *secret.Record
	if f.passwordFile != "" {
		s, err := h.sealer()
		if err != nil {
			return err
		}
		if s == nil {
			return errors.New("YUUKA_KEK_FILE is not set, so the password cannot be sealed; see docs/operate/secrets.md")
		}
		pw, err := h.readSecret(f.passwordFile)
		if err != nil {
			return err
		}
		sealed, err = sealSMTPPassword(ctx, s, c, pw, h.now())
		clear(pw)
		if err != nil {
			return err
		}
	}

	tool := "transport.set"
	if add {
		tool = "transport.add"
	}
	args := transportAuditArgs(c, sealed)
	if exists {
		args["transport_id"] = cur.ID
		args["revision"] = cur.Revision
	}
	if configured {
		args["was"] = map[string]any{"host": cur.Host, "port": cur.Port, "auth": cur.Auth, "username": cur.Username, "caps": cur.Caps}
	}
	var rev int64
	if exists {
		rev = cur.Revision
	}
	t, err := h.db.ConfigureTransport(ctx, c, rev, sealed, h.action(tool, args))
	switch {
	case errors.Is(err, store.ErrStale), errors.Is(err, store.ErrDuplicate):
		return fmt.Errorf("transport %s changed while this command ran, and nothing was written; "+
			"look at `yuuka transport ls` and run it again", name)
	case errors.Is(err, store.ErrNeedsSecret):
		return fmt.Errorf("transport %s: %w; give it with --password-file PATH or -", name, store.ErrNeedsSecret)
	case err != nil:
		return err
	}
	if sealed != nil {
		h.note("Password sealed for %s under %s key %s.", sealed.Audience, sealed.Wrapper, sealed.KeyRef)
	}
	h.note("Check it without sending anything: yuuka transport test %s", name)
	verb := "configured"
	if exists && cur.DisabledAt != nil {
		verb = "configured and enabled again"
	}
	return h.say("%s transport %s (%s): %s, caps %s", verb, t.Name, t.ID, destination(c), capsText(t.Caps))
}

// destination describes where a transport connects, as the audience
// would, without the scheme.
func destination(c store.TransportConfig) string {
	d := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	if c.Username != "" {
		d = c.Username + "@" + d
	}
	return shown(d)
}

// transportAuditArgs are what an action that widens a transport records
// (§17.6): where it goes and which sealed secret, never the secret, its
// hash or anything else derived from the plaintext.
func transportAuditArgs(c store.TransportConfig, sealed *secret.Record) map[string]any {
	a := map[string]any{"name": c.Name, "host": c.Host, "port": c.Port, "auth": c.Auth, "username": c.Username, "caps": c.Caps}
	if sealed != nil {
		a["secret_id"] = sealed.ID
		a["kind"] = sealed.Kind
		a["audience"] = sealed.Audience
		a["wrapper"] = sealed.Wrapper
		a["key_ref"] = sealed.KeyRef
	}
	return a
}

// sealSMTPPassword seals pw for c's destination under a fresh id. It does
// not keep pw; the caller clears it.
func sealSMTPPassword(ctx context.Context, s *secret.Sealer, c store.TransportConfig, pw []byte, now time.Time) (*secret.Record, error) {
	aud, err := secret.SMTPS(c.Username, c.Host, c.Port)
	if err != nil {
		return nil, fmt.Errorf("transport %s: %w", c.Name, err)
	}
	r, err := s.Seal(ctx, ulid.New(now), store.SecretSMTPPassword, aud, pw)
	if err != nil {
		return nil, fmt.Errorf("transport %s: seal the password: %w", c.Name, err)
	}
	return &r, nil
}

// capNames spells caps the way transports.caps stores them.
func capNames(c transport.Caps) []string {
	out := []string{}
	for _, x := range []struct {
		on   bool
		name string
	}{
		{c.CallerMessageID, "message-id"},
		{c.CallerReturnPath, "return-path"},
		{c.CallerDKIM, "dkim"},
		{c.StatusQuery, "status-query"},
	} {
		if x.on {
			out = append(out, x.name)
		}
	}
	return out
}

func capsText(caps []string) string {
	if len(caps) == 0 {
		return "none"
	}
	return strings.Join(caps, ",")
}

// sealer loads the KEK files named in the environment (loadSealer): nil
// without YUUKA_KEK_FILE.
func (h *host) sealer() (*secret.Sealer, error) {
	getenv := h.getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	return loadSealer(getenv)
}

// readSecret reads a third-party secret from the file at path, or from
// standard input for "-" (§17.6). One trailing newline, which an editor
// or echo leaves, is dropped; nothing else is changed. A path that cannot
// be read is not repeated in the error, in case it was the secret itself.
func (h *host) readSecret(path string) ([]byte, error) {
	var r io.Reader
	if path == "-" {
		r = h.in
		if r == nil {
			r = os.Stdin
			h.note("Reading the password from standard input; end it with Ctrl-D.")
		}
	} else {
		f, err := os.Open(path)
		if err != nil {
			if pe, ok := errors.AsType[*fs.PathError](err); ok {
				err = pe.Err
			}
			return nil, fmt.Errorf("--password-file: cannot open the file: %w", err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, secret.MaxPlaintext+3))
	if err != nil {
		clear(b)
		return nil, fmt.Errorf("--password-file: read: %w", err)
	}
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
		if n := len(b); n > 0 && b[n-1] == '\r' {
			b = b[:n-1]
		}
	}
	switch {
	case len(b) == 0:
		return nil, errors.New("--password-file: the password is empty")
	case len(b) > secret.MaxPlaintext:
		clear(b)
		return nil, fmt.Errorf("--password-file: more than %d bytes", secret.MaxPlaintext)
	}
	return b, nil
}

// transportList is `transport ls`. The secret column says whether one is
// set, when it was sealed and under which key: never anything derived
// from the plaintext.
func (h *host) transportList(ctx context.Context, all bool) error {
	trs, err := h.db.Transports(ctx, all)
	if err != nil {
		return err
	}
	secs, err := h.db.Secrets(ctx)
	if err != nil {
		return err
	}
	byID := map[string]store.StoredSecret{}
	for _, s := range secs {
		byID[s.ID] = s
	}
	tw := tabwriter.NewWriter(h.out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tSTATE\tHOST\tPORT\tAUTH\tUSERNAME\tCAPS\tSECRET\tSEALED\tKEY\tID")
	for _, t := range trs {
		dash := func(s string) string {
			if s == "" {
				return "-"
			}
			return shown(s)
		}
		state, port, caps := "configured", strconv.Itoa(t.Port), capsText(t.Caps)
		switch {
		case t.DisabledAt != nil:
			state = "disabled"
		case t.ConfiguredAt == nil:
			state, port, caps = "placeholder", "-", "-"
		}
		sec, sealed, key := "-", "-", "-"
		if t.SecretID != "" {
			sec = "set"
			if s, ok := byID[t.SecretID]; ok {
				sealed = s.SealedAt.UTC().Format(time.RFC3339)
				key = s.Wrapper + ":" + s.KeyRef
			}
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.Name, state, dash(t.Host), port,
			dash(t.Auth), dash(t.Username), caps, sec, sealed, key, t.ID)
	}
	return tw.Flush()
}

// transportRemove is `transport rm`: disable and erase the secret
// (DisableTransport). Identities that name it keep naming it; the note
// says which, since their mail now needs another route.
func (h *host) transportRemove(ctx context.Context, name string) error {
	cur, err := h.db.TransportByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) || err == nil && cur.DisabledAt != nil {
		return fmt.Errorf("no enabled transport %s", name)
	}
	if err != nil {
		return err
	}
	args := map[string]any{
		"transport_id": cur.ID, "name": cur.Name, "host": cur.Host, "port": cur.Port,
		"username": cur.Username, "revision": cur.Revision,
	}
	if cur.SecretID != "" {
		args["secret_id"] = cur.SecretID
	}
	err = h.db.DisableTransport(ctx, name, cur.Revision, h.action("transport.rm", args))
	if errors.Is(err, store.ErrStale) || errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("transport %s changed while this command ran, and nothing was written; run it again", name)
	}
	if err != nil {
		return err
	}
	ids, err := h.db.Identities(ctx, false)
	if err != nil {
		return err
	}
	for _, i := range ids {
		if slices.Contains(i.Transports, name) {
			h.note("%s still names it in its chain %s; `yuuka identity set` changes that.",
				shown(i.Address()), strings.Join(i.Transports, ","))
		}
	}
	return h.say("removed transport %s (%s); its secret is erased, its name stays in the sending history", name, cur.ID)
}

// transportTest is `transport test`: open the secret for the destination
// recomputed from the row, as sending would, and probe the server
// without sending mail.
func (h *host) transportTest(ctx context.Context, name string) error {
	cur, err := h.db.TransportByName(ctx, name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("no transport %s", name)
	case err != nil:
		return err
	case cur.DisabledAt != nil:
		return fmt.Errorf("transport %s is removed", name)
	case cur.ConfiguredAt == nil:
		return fmt.Errorf("transport %s is a placeholder; configure it with `yuuka transport add`", name)
	}
	cfg := transport.Config{Name: cur.Name, Host: cur.Host, Port: cur.Port, Username: cur.Username}
	if cur.Auth == store.TransportAuthPlain {
		if cur.SecretID == "" {
			return fmt.Errorf("transport %s has no password; set it with `yuuka transport set %s --password-file PATH`", name, name)
		}
		s, err := h.sealer()
		if err != nil {
			return err
		}
		st, err := h.db.Secret(ctx, cur.SecretID)
		if err != nil {
			return err
		}
		aud, err := secret.SMTPS(cur.Username, cur.Host, cur.Port)
		if err != nil {
			return err
		}
		v, err := s.Open(ctx, st.Record, aud)
		if err != nil {
			return fmt.Errorf("transport %s: %s", name, openFailure(err))
		}
		defer v.Close()
		cfg.Password = string(v.Reveal())
	}
	smtp, err := transport.NewSMTP(cfg)
	if err != nil {
		return err
	}
	h.note("Connecting to %s:%d (implicit TLS); no mail is sent.", cur.Host, cur.Port)
	if err := smtp.Probe(ctx); err != nil {
		return fmt.Errorf("transport %s: %w", name, err)
	}
	if cur.Username != "" {
		return h.say("ok: %s answered EHLO and accepted AUTH PLAIN for %s", name, shown(cur.Username))
	}
	return h.say("ok: %s answered EHLO", name)
}

// openFailure explains why a secret did not open, in the terms of §17.3.
func openFailure(err error) string {
	switch {
	case errors.Is(err, secret.ErrLocked):
		return "locked: " + err.Error() + " (YUUKA_KEK_FILE, YUUKA_KEK_PREVIOUS_FILES)"
	case errors.Is(err, secret.ErrAudienceMismatch):
		return "audience mismatch: the destination changed after the password was sealed; set the password again"
	case errors.Is(err, secret.ErrTampered):
		return "tampered: the sealed value does not authenticate (altered, corrupted or moved from another row)"
	case errors.Is(err, secret.ErrUnsupportedVersion):
		return "sealed by a newer yuuka: " + err.Error()
	}
	return err.Error()
}
