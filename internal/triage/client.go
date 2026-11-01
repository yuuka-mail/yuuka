package triage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/yuuka-mail/yuuka/internal/triage/score"
)

// The System One wire format, as the backends of §15.6 document it
// (Kev's README, Laya's serve.py; Jev is the hosted original both
// copy). Pinned by the fixtures under testdata/systemone; what the
// fixtures could not be checked against is listed in §15.6.
//
// Request:  {"state": "<text>", "model": "<optional>",
//            "questions": {"<key>": {"type": "choice"|"noul",
//                          "instructions": "...", "criteria": {...}}}}
// Response: {"model": "...", "answers": {"<key>": {"type": "noul",
//            "noul": <P(true)>} | {"type": "choice", "choice": "...",
//            "confidence": c, "probabilities": {"<option>": p, ...}}},
//            "usage": {...}}
//
// Fields a backend adds (Laya's routing, confidence on noul, its
// abstention when asked for a min_confidence) are ignored but for the
// checkpoint routing names and the two that say a backend abstained.

// Path is the endpoint under the configured base URL.
const Path = "/v1/systemone"

type wireQuestion struct {
	Type         QuestionType      `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
}

type wireRequest struct {
	State     string                  `json:"state"`
	Model     string                  `json:"model,omitempty"`
	Questions map[string]wireQuestion `json:"questions"`
}

type wireAnswer struct {
	Type          QuestionType       `json:"type"`
	Noul          *float64           `json:"noul"`
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	// Laya, given min_confidence: "passed", "abstained" or
	// "unevaluated", and low_confidence when its gate fired.
	Abstention    string `json:"abstention"`
	LowConfidence bool   `json:"low_confidence"`
}

type wireResponse struct {
	Model   string                `json:"model"`
	Answers map[string]wireAnswer `json:"answers"`
	// Laya's top-level model is always "laya-rl-agent", whichever
	// checkpoint answered; outside its strict mode the checkpoint is
	// named here, and that is the model a score has to record.
	Routing struct {
		Model string `json:"model"`
	} `json:"routing"`
}

// Input is what one scoring sends: the state text (State builds it).
// The questions are always Questions.
type Input struct {
	State string
}

// Result is a validated answer to every question of Questions.
type Result struct {
	// Model is what the backend said it ran.
	Model   string
	Answers map[string]score.Answer
}

var (
	// ErrBackend: the scorer is down, refused us, or answered something
	// that is not the protocol. It says nothing about the message, and
	// counts towards the circuit breaker.
	ErrBackend = errors.New("triage: scorer unavailable")
	// ErrRejected: the scorer refused this one input (413, 422).
	// Retrying it soon is pointless; others may still go through.
	ErrRejected = errors.New("triage: scorer rejected the input")
)

// Client speaks System One over HTTP.
type Client struct {
	// URL is the full endpoint, base URL plus Path.
	URL string
	// Token is sent as a bearer token when set.
	Token string
	// Model is asked for when set; otherwise the backend picks.
	Model string
	HTTP  *http.Client
}

// maxResponse bounds a response body; four answers fit in far less.
const maxResponse = 1 << 20

// Score asks Questions about in.State.
func (c *Client) Score(ctx context.Context, in Input) (Result, error) {
	body, err := json.Marshal(request(in.State, c.Model))
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// The error names the URL, never the body.
		return Result{}, fmt.Errorf("%w: %w", ErrBackend, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return Result{}, fmt.Errorf("%w: read response: %w", ErrBackend, err)
	}
	// An error body may quote the state, which is mail content: only
	// the status goes into the error, and so into the log.
	switch {
	case resp.StatusCode == http.StatusRequestEntityTooLarge || resp.StatusCode == http.StatusUnprocessableEntity:
		return Result{}, fmt.Errorf("%w: HTTP %d", ErrRejected, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return Result{}, fmt.Errorf("%w: HTTP %d", ErrBackend, resp.StatusCode)
	case len(data) > maxResponse:
		return Result{}, fmt.Errorf("%w: response over %d bytes", ErrBackend, maxResponse)
	}
	var wr wireResponse
	if err := json.Unmarshal(data, &wr); err != nil {
		return Result{}, fmt.Errorf("%w: response is not System One JSON: %w", ErrBackend, err)
	}
	res, err := validate(wr)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrBackend, err)
	}
	return res, nil
}

func request(state, model string) wireRequest {
	r := wireRequest{State: state, Model: model, Questions: map[string]wireQuestion{}}
	for _, q := range Questions {
		r.Questions[q.Key] = wireQuestion{Type: q.Type, Instructions: q.Instructions, Criteria: q.Criteria}
	}
	return r
}

// sumTolerance is how far the probabilities of a choice may sum from 1:
// backends round, and softmax in reduced precision drifts.
const sumTolerance = 0.01

// validate checks that every question got an answer of its type with
// probabilities that are probabilities, and converts them.
func validate(wr wireResponse) (Result, error) {
	model := wr.Routing.Model
	if model == "" {
		model = wr.Model
	}
	res := Result{Model: modelName(model), Answers: map[string]score.Answer{}}
	for _, q := range Questions {
		w, ok := wr.Answers[q.Key]
		if !ok {
			return Result{}, fmt.Errorf("no answer to %s", q.Key)
		}
		if w.Type != "" && w.Type != q.Type {
			return Result{}, fmt.Errorf("%s: answered as %q, asked as %q", q.Key, w.Type, q.Type)
		}
		var a score.Answer
		switch q.Type {
		case YesNo:
			if w.Noul == nil || !isProb(*w.Noul) {
				return Result{}, fmt.Errorf("%s: noul is not a probability", q.Key)
			}
			p := *w.Noul
			a.P = map[string]float64{Yes: p, No: 1 - p}
		case Choice:
			if len(w.Probabilities) != len(q.Criteria) {
				return Result{}, fmt.Errorf("%s: %d probabilities for %d options", q.Key, len(w.Probabilities), len(q.Criteria))
			}
			var sum float64
			for o, p := range w.Probabilities {
				if _, ok := q.Criteria[o]; !ok {
					return Result{}, fmt.Errorf("%s: probability for an option not asked", q.Key)
				}
				if !isProb(p) {
					return Result{}, fmt.Errorf("%s: %s is not a probability", q.Key, o)
				}
				sum += p
			}
			if math.Abs(sum-1) > sumTolerance {
				return Result{}, fmt.Errorf("%s: probabilities sum to %.4f", q.Key, sum)
			}
			a.P = w.Probabilities
		}
		// An abstention is checked like any answer, so that a broken
		// backend cannot hide behind it, but keeps no probabilities: the
		// backend itself said not to use them.
		if w.Abstention == "abstained" || w.LowConfidence {
			a = score.Answer{Abstain: true}
		}
		res.Answers[q.Key] = a
	}
	return res, nil
}

func isProb(p float64) bool { return !math.IsNaN(p) && p >= 0 && p <= 1 }

// modelName makes the backend's model name fit to store and show: it
// comes from another process, so it is bounded and stripped of control
// characters.
func modelName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if s == "" {
		return "unreported"
	}
	if utf8.RuneCountInString(s) > 200 {
		s = string([]rune(s)[:200])
	}
	return s
}

// requestTimeout bounds one scoring. A CPU encoder answers in a second
// or two; a cold start loading a checkpoint takes longer.
const requestTimeout = 60 * time.Second

// NewHTTPClient is the HTTP client for the scorer. Unless hosted, it
// connects only to loopback and private addresses, checked on the
// address actually dialled, so neither a DNS answer nor a later change
// of it can send mail off the deployment. It never uses a proxy from the
// environment, which would dial for us, and never follows a redirect,
// which would resend the mail to wherever the scorer points.
func NewHTTPClient(hosted bool) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second}
	if !hosted {
		d.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || !localIP(ip) {
				return fmt.Errorf("triage: %s is not a loopback or private address; set YUUKA_TRIAGE_HOSTED=1 to send mail to a third party", host)
			}
			return nil
		}
	}
	return &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         d.DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// localIP: loopback, or a private range (RFC 1918, RFC 4193).
func localIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate()
}
