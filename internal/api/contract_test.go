package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authn"
	"github.com/yuuka-mail/yuuka/internal/openapi"
)

var update = flag.Bool("update", false, "rewrite openapi.json")

// openapi.json is committed so that a change to the contract shows in
// review; it must be what the code generates.
func TestOpenAPI(t *testing.T) {
	t.Parallel()
	doc, err := OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile("openapi.json", doc, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if file, err := os.ReadFile("openapi.json"); err != nil || !bytes.Equal(file, doc) {
		t.Fatalf("openapi.json is out of date (%v): run mise run generate", err)
	}

	var parsed struct {
		Paths map[string]map[string]struct {
			Parameters []struct{ Name, In string }
		}
	}
	if err := json.Unmarshal(doc, &parsed); err != nil {
		t.Fatal(err)
	}
	ops := authn.Ops()
	for _, rt := range routes {
		ops = append(ops, rt.Op)
	}
	var described int
	for _, op := range ops {
		o, ok := parsed.Paths[op.Path][strings.ToLower(op.Method)]
		if !ok {
			t.Errorf("%s %s is not in openapi.json", op.Method, op.Path)
			continue
		}
		// Every {name} of the path is a path parameter, and only those.
		var want, got []string
		for _, m := range regexp.MustCompile(`\{(\w+)\}`).FindAllStringSubmatch(op.Path, -1) {
			want = append(want, m[1])
		}
		for _, p := range o.Parameters {
			if p.In == "path" {
				got = append(got, p.Name)
			}
		}
		if !slices.Equal(want, got) {
			t.Errorf("%s %s: path parameters %v, want %v", op.Method, op.Path, got, want)
		}
	}
	for _, methods := range parsed.Paths {
		described += len(methods)
	}
	if described != len(ops) {
		t.Errorf("openapi.json has %d operations, the tables %d", described, len(ops))
	}

	// Served without credentials, past a guard that lets nothing through.
	h := New(nil, nil, nil).Handler(denyAll{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/openapi.json", nil))
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), doc) || w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("served: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
}

type denyAll struct{}

func (denyAll) Require(http.Handler, ...auth.Scope) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
}

type bindParams struct {
	ID     string            `path:"id"`
	Limit  int               `query:"limit" minimum:"1" maximum:"10" default:"5"`
	Class  []agentview.Class `query:"class"`
	Level  agentview.Level   `query:"level" default:"part"`
	Part   string            `query:"part" pattern:"^[1-9](\\.[1-9])*$"`
	Cursor *Cursor           `query:"cursor"`
	On     bool              `query:"on"`
	Body   bindBody          `body:"json"`
}

type bindBody struct {
	Name  string            `json:"name"`
	Level agentview.Level   `json:"level,omitempty"`
	Count int               `json:"count" maximum:"3"`
	Tags  []agentview.Class `json:"tags,omitempty"`
}

func TestBind(t *testing.T) {
	t.Parallel()
	cur := Cursor{ID: "01J0000000000000000000000A"}.String()
	for _, c := range []struct {
		query, body string
		code        openapi.Code // "" for success
		check       func(bindParams) bool
	}{
		{"", `{}`, "", func(p bindParams) bool {
			return p.ID == "m1" && p.Limit == 5 && p.Level == agentview.LevelPart && p.Class == nil && p.Cursor == nil
		}},
		{"limit=10&class=human,unknown&level=sections&part=1.2&on=true&cursor=" + cur, `{"name":"x","count":3,"tags":["machine"]}`, "", func(p bindParams) bool {
			return p.Limit == 10 && slices.Equal(p.Class, []agentview.Class{"human", "unknown"}) && p.Level == "sections" &&
				p.Part == "1.2" && p.On && p.Cursor.ID == "01J0000000000000000000000A" && p.Body.Count == 3 && p.Body.Tags[0] == "machine"
		}},
		{"limit=", `{}`, "", func(p bindParams) bool { return p.Limit == 5 }}, // empty is absent
		{"limit=0", `{}`, openapi.CodeInvalidParam, nil},
		{"limit=11", `{}`, openapi.CodeInvalidParam, nil},
		{"limit=ten", `{}`, openapi.CodeInvalidParam, nil},
		{"class=human,spam", `{}`, openapi.CodeInvalidParam, nil},
		{"class=,human", `{}`, openapi.CodeInvalidParam, nil},
		{"level=all", `{}`, openapi.CodeInvalidParam, nil},
		{"part=0.1", `{}`, openapi.CodeInvalidParam, nil},
		{"on=maybe", `{}`, openapi.CodeInvalidParam, nil},
		{"cursor=x", `{}`, openapi.CodeInvalidParam, nil},
		{"", `{"count":4}`, openapi.CodeInvalidBody, nil},
		{"", `{"level":"all"}`, openapi.CodeInvalidBody, nil},
		{"", `{"tags":["human","spam"]}`, openapi.CodeInvalidBody, nil},
		{"", `{"extra":1}`, openapi.CodeInvalidBody, nil},
		{"", `{`, openapi.CodeInvalidBody, nil},
		{"", `{"name":"` + strings.Repeat("x", maxBody) + `"}`, openapi.CodeBodyTooLarge, nil},
		{"", "", openapi.CodeUnsupportedType, nil}, // no Content-Type
	} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/x/m1?"+c.query, strings.NewReader(c.body))
		r.SetPathValue("id", "m1")
		if c.body != "" {
			r.Header.Set("Content-Type", "application/json; charset=utf-8")
		}
		var p bindParams
		err := bind(r, &p)
		var code openapi.Code
		if prob, ok := errors.AsType[*openapi.Problem](err); ok {
			code = prob.Code
		} else if err != nil {
			t.Errorf("%s %.40s: %v is not a problem", c.query, c.body, err)
		}
		if code != c.code || c.check != nil && !c.check(p) {
			t.Errorf("%s %.40s: %v, %+v", c.query, c.body, err, p)
		}
	}
}

// The constraints bind enforces are the ones the document states.
func TestDocumentConstraints(t *testing.T) {
	t.Parallel()
	// A write endpoint as M2 adds them: one entry and a typed handler.
	create := jsonRoute(http.MethodPost, "/x/{id}", "x", read,
		func(*API, context.Context, *auth.Viewer, bindParams) (Thread, error) { return Thread{}, nil }).withStatus(http.StatusCreated)
	download := rawRoute(http.MethodGet, "/x/{id}/raw", "x", "text/plain", read,
		func(*API, http.ResponseWriter, *http.Request, *auth.Viewer, IDParams) error { return nil })
	doc, err := openapi.Document("t", "1", []openapi.Op{create.Op, download.Op})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"name": "limit"`, `"default": 5`, `"minimum": 1`, `"maximum": 10`,
		`"style": "form"`, `"explode": false`, `"pattern": "^[1-9](\\.[1-9])*$"`, `"default": "part"`,
		`"$ref": "#/components/schemas/bindBody"`, `"maximum": 3`, `"text/plain": {}`, `"201": {`,
		`"$ref": "#/components/schemas/Thread"`,
	} {
		if !bytes.Contains(doc, []byte(want)) {
			t.Errorf("document lacks %s", want)
		}
	}
	if bytes.Contains(doc, []byte(`"null"`)) {
		t.Error("document allows null")
	}
}
