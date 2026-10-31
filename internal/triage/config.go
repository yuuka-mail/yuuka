package triage

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/triage/score"
)

// Config is the scorer configuration, from the environment. Global, not
// per principal (§15.8). The zero value, without an Endpoint, is
// scoring off.
type Config struct {
	// Endpoint is the full URL scored mail is posted to.
	Endpoint string
	// Name is the scorer's name in every score; the operator renames it
	// when the model behind it changes, so that old and new scores are
	// not taken for one another.
	Name  string
	Model string
	// Lookback is how far back the sweep scores unscored mail.
	Lookback time.Duration
	Hosted   bool
	Token    string
}

// DefaultLookback bounds the sweep: on first start, a month of mail is
// scored, older mail only by an explicit rescore.
const DefaultLookback = 30 * 24 * time.Hour

var scorerName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,31}$`)

// LoadConfig reads the YUUKA_TRIAGE_* variables; readFile reads the
// token file. Without YUUKA_TRIAGE_URL scoring is off, and any other of
// them is an error, so that a half-configured scorer is not mistaken
// for a running one.
func LoadConfig(getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	var errs []error
	c := Config{Lookback: DefaultLookback}
	raw := getenv("YUUKA_TRIAGE_URL")
	if raw == "" {
		for _, name := range []string{"NAME", "MODEL", "LOOKBACK", "HOSTED", "TOKEN_FILE"} {
			if getenv("YUUKA_TRIAGE_"+name) != "" {
				errs = append(errs, fmt.Errorf("YUUKA_TRIAGE_%s needs YUUKA_TRIAGE_URL", name))
			}
		}
		return c, errors.Join(errs...)
	}

	switch h := getenv("YUUKA_TRIAGE_HOSTED"); h {
	case "", "0":
	case "1":
		c.Hosted = true
	default:
		errs = append(errs, fmt.Errorf("YUUKA_TRIAGE_HOSTED: %q: want 1 or unset", h))
	}
	endpoint, err := endpointURL(raw, c.Hosted)
	if err != nil {
		errs = append(errs, fmt.Errorf("YUUKA_TRIAGE_URL: %w", err))
	}
	c.Endpoint = endpoint

	c.Name = getenv("YUUKA_TRIAGE_NAME")
	if !scorerName.MatchString(c.Name) {
		errs = append(errs, fmt.Errorf("YUUKA_TRIAGE_NAME: %q: want a name like laya-ml-1 (a-z, 0-9, '.', '-', at most 32), changed whenever the model changes", c.Name))
	}
	c.Model = getenv("YUUKA_TRIAGE_MODEL")
	if len(c.Model) > 200 {
		errs = append(errs, errors.New("YUUKA_TRIAGE_MODEL: longer than 200 bytes"))
	}
	if v := getenv("YUUKA_TRIAGE_LOOKBACK"); v != "" {
		d, err := parseDays(v)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("YUUKA_TRIAGE_LOOKBACK: %q: want a positive duration like 30d or 720h", v))
		}
		c.Lookback = d
	}

	// The token comes from a file, never the environment itself, like
	// the transports' passwords.
	path := getenv("YUUKA_TRIAGE_TOKEN_FILE")
	if path != "" {
		b, err := readFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("YUUKA_TRIAGE_TOKEN_FILE: %w", err))
		} else if c.Token = strings.TrimSpace(string(b)); c.Token == "" {
			errs = append(errs, errors.New("YUUKA_TRIAGE_TOKEN_FILE: the file is empty"))
		}
	}
	if c.Hosted && path == "" {
		errs = append(errs, errors.New("YUUKA_TRIAGE_HOSTED=1 needs YUUKA_TRIAGE_TOKEN_FILE"))
	}
	return c, errors.Join(errs...)
}

// On says whether a scorer is configured.
func (c Config) On() bool { return c.Endpoint != "" }

// endpointURL checks the base URL and returns the endpoint under it.
// Unless hosted, a host given as an address must be loopback or
// private; a host given by name is checked when dialled
// (NewHTTPClient), since what it resolves to can change.
func endpointURL(raw string, hosted bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("not a URL")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "", fmt.Errorf("%q: want an http or https base URL", raw)
	case u.Hostname() == "":
		return "", fmt.Errorf("%q: no host", raw)
	case u.User != nil:
		return "", errors.New("credentials in the URL: put the token in YUUKA_TRIAGE_TOKEN_FILE")
	case u.RawQuery != "" || u.Fragment != "":
		return "", fmt.Errorf("%q: want a base URL without query or fragment", raw)
	case hosted && u.Scheme != "https":
		return "", errors.New("a hosted scorer receives the mail: https only")
	}
	if !hosted {
		if ip := net.ParseIP(u.Hostname()); ip != nil && !localIP(ip) {
			return "", fmt.Errorf("%s is not a loopback or private address; set YUUKA_TRIAGE_HOSTED=1 to send mail to a third party", u.Hostname())
		}
	}
	return strings.TrimRight(u.String(), "/") + Path, nil
}

// parseDays is time.ParseDuration plus a whole number of days, "30d".
func parseDays(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil {
			return 0, err
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// Profile is what makes a score current: this scorer and this question
// set.
func (c Config) Profile() score.Profile {
	return score.Profile{Scorer: c.Name, Questions: QuestionsVersion}
}

// EndpointKind says where scoring sends mail, as each score records it.
func (c Config) EndpointKind() string {
	if c.Hosted {
		return score.EndpointHosted
	}
	return score.EndpointLocal
}

// Client is the System One client for c.
func (c Config) Client() *Client {
	return &Client{URL: c.Endpoint, Token: c.Token, Model: c.Model, HTTP: NewHTTPClient(c.Hosted)}
}
