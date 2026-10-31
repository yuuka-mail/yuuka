package triage

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yuuka-mail/yuuka/internal/triage/score"
)

var update = flag.Bool("update", false, "rewrite the golden request")

// The fixtures under testdata/systemone are the response shapes the
// backends document: kev.json follows Kev's README example, laya.json
// Laya's serve.py outside its strict mode, with the abstention fields
// it adds when asked for a min_confidence. request.json is what this
// client sends; it changes only with Questions, and then
// QuestionsVersion changes with it.

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/systemone/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// backend serves body for every request and records the last one.
func backend(t *testing.T, status int, body []byte) (*httptest.Server, *[]byte, *http.Header) {
	t.Helper()
	var got []byte
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != Path {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		got, _ = io.ReadAll(r.Body)
		hdr = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &hdr
}

func client(srv *httptest.Server) *Client {
	return &Client{URL: srv.URL + Path, HTTP: NewHTTPClient(false)}
}

func TestClientRequest(t *testing.T) {
	srv, got, hdr := backend(t, 200, fixture(t, "kev.json"))
	c := client(srv)
	c.Token, c.Model = "test-token", "multilingual"
	if _, err := c.Score(t.Context(), Input{State: "the state"}); err != nil {
		t.Fatal(err)
	}
	if h := hdr.Get("Authorization"); h != "Bearer test-token" {
		t.Errorf("Authorization %q", h)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, *got, "", "  "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')
	if *update {
		if err := os.WriteFile("testdata/systemone/request.json", pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if want := fixture(t, "request.json"); !bytes.Equal(pretty.Bytes(), want) {
		t.Errorf("request changed; if Questions changed, bump QuestionsVersion and run with -update:\n%s", pretty.Bytes())
	}
}

func TestClientResponses(t *testing.T) {
	for _, c := range []struct {
		file    string
		model   string
		answers map[string]score.Answer
	}{
		{"kev.json", "kev-latest", map[string]score.Answer{
			Kind:       {P: map[string]float64{"notification": 0.86, "conversation": 0.03, "marketing": 0.06, "spam": 0.05}},
			Phishing:   {P: map[string]float64{"yes": 0.04, "no": 0.96}},
			NeedsReply: {P: map[string]float64{"yes": 0.02, "no": 0.98}},
		}},
		{"laya.json", "multilingual", map[string]score.Answer{
			Kind:       {P: map[string]float64{"notification": 0.05, "conversation": 0.02, "marketing": 0.13, "spam": 0.8}},
			Phishing:   {P: map[string]float64{"yes": 0.93, "no": 0.07}},
			NeedsReply: {Abstain: true},
		}},
	} {
		srv, _, _ := backend(t, 200, fixture(t, c.file))
		res, err := client(srv).Score(t.Context(), Input{State: "s"})
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if res.Model != c.model {
			t.Errorf("%s: model %q", c.file, res.Model)
		}
		for q, want := range c.answers {
			got := res.Answers[q]
			if got.Abstain != want.Abstain || len(got.P) != len(want.P) {
				t.Errorf("%s %s: %+v, want %+v", c.file, q, got, want)
			}
			for o, p := range want.P {
				if d := got.P[o] - p; d > 1e-9 || d < -1e-9 {
					t.Errorf("%s %s %s: %v, want %v", c.file, q, o, got.P[o], p)
				}
			}
		}
	}
}

// The response is checked, not trusted: a backend that answers something
// else than what was asked is a broken backend.
func TestClientRejectsInvalidAnswers(t *testing.T) {
	var base map[string]any
	if err := json.Unmarshal(fixture(t, "kev.json"), &base); err != nil {
		t.Fatal(err)
	}
	edit := func(f func(answers map[string]any)) []byte {
		var m map[string]any
		b, _ := json.Marshal(base)
		_ = json.Unmarshal(b, &m)
		f(m["answers"].(map[string]any))
		out, _ := json.Marshal(m)
		return out
	}
	kind := func(a map[string]any) map[string]any {
		return a["kind"].(map[string]any)["probabilities"].(map[string]any)
	}
	for name, body := range map[string][]byte{
		"missing answer":    edit(func(a map[string]any) { delete(a, "phishing") }),
		"noul above 1":      edit(func(a map[string]any) { a["phishing"].(map[string]any)["noul"] = 1.2 }),
		"noul negative":     edit(func(a map[string]any) { a["needs_reply"].(map[string]any)["noul"] = -0.1 }),
		"noul missing":      edit(func(a map[string]any) { delete(a["needs_reply"].(map[string]any), "noul") }),
		"wrong type":        edit(func(a map[string]any) { a["phishing"].(map[string]any)["type"] = "choice" }),
		"sum off":           edit(func(a map[string]any) { kind(a)["spam"] = 0.5 }),
		"option not asked":  edit(func(a map[string]any) { delete(kind(a), "spam"); kind(a)["phish"] = 0.05 }),
		"option missing":    edit(func(a map[string]any) { delete(kind(a), "spam"); kind(a)["notification"] = 0.91 }),
		"probability above": edit(func(a map[string]any) { kind(a)["spam"] = 1.05; kind(a)["notification"] = -0.14 }),
		"not json":          []byte("<html>gateway</html>"),
	} {
		srv, _, _ := backend(t, 200, body)
		if _, err := client(srv).Score(t.Context(), Input{State: "s"}); !errors.Is(err, ErrBackend) {
			t.Errorf("%s: %v, want ErrBackend", name, err)
		}
	}
}

// A model name only under routing is Laya outside strict mode with an
// older server; none at all is recorded as unreported.
func TestClientModelName(t *testing.T) {
	var m map[string]any
	_ = json.Unmarshal(fixture(t, "laya.json"), &m)
	delete(m, "model")
	routed, _ := json.Marshal(m)
	delete(m, "routing")
	none, _ := json.Marshal(m)
	for body, want := range map[string]string{string(routed): "multilingual", string(none): "unreported"} {
		srv, _, _ := backend(t, 200, []byte(body))
		res, err := client(srv).Score(t.Context(), Input{State: "s"})
		if err != nil || res.Model != want {
			t.Errorf("model %q, %v; want %q", res.Model, err, want)
		}
	}
	if got := modelName("a\x1b[31mb\n"); got != "a[31mb" {
		t.Errorf("control characters kept: %q", got)
	}
}

// Statuses: refusing one input is not the scorer being down, and the
// error never carries the body, which may quote the mail.
func TestClientStatus(t *testing.T) {
	for status, want := range map[int]error{
		413: ErrRejected, 422: ErrRejected,
		400: ErrBackend, 401: ErrBackend, 500: ErrBackend, 503: ErrBackend,
	} {
		srv, _, _ := backend(t, status, []byte(`{"detail":"Your verification code is 482913"}`))
		_, err := client(srv).Score(t.Context(), Input{State: "s"})
		if !errors.Is(err, want) {
			t.Errorf("HTTP %d: %v, want %v", status, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "482913") {
			t.Errorf("HTTP %d: the body reached the error: %v", status, err)
		}
	}
}

// A redirect is not followed: it would post the mail somewhere else.
func TestClientNoRedirect(t *testing.T) {
	var hit atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Store(true) }))
	defer elsewhere.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	if _, err := client(srv).Score(t.Context(), Input{State: "s"}); !errors.Is(err, ErrBackend) {
		t.Errorf("redirect: %v", err)
	}
	if hit.Load() {
		t.Error("the redirect was followed")
	}
}

// Unless hosted, the client dials only loopback and private addresses,
// whatever a name resolves to. 192.0.2.1 is a documentation address:
// the dial is refused before any packet is sent.
func TestClientDialGuard(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	c := &Client{URL: "http://192.0.2.1:31490" + Path, HTTP: NewHTTPClient(false)}
	_, err := c.Score(t.Context(), Input{State: "s"})
	if !errors.Is(err, ErrBackend) || !strings.Contains(err.Error(), "not a loopback or private address") {
		t.Errorf("public address: %v", err)
	}
}
