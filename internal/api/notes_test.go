package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// Notes through REST (§8.10): any principal writes and deletes its own;
// the message detail carries every note to a person, an agent's own to
// the agent, each signed by its author. The audit log keeps no text.
func TestNotes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	msg := h.find("Invoice and logo")
	cite := `[{"uri":"yuuka://message/` + msg + `#part/1/para/1","quote":"invoice"}]`
	writer := h.token(auth.MessageAnnotate)
	put := func(body, bearer string) *httptest.ResponseRecorder {
		return h.do(http.MethodPut, "/api/v1/messages/"+msg+"/note", body, bearer)
	}

	w := put(`{"text":"looks like a real invoice SECRET-REST-NOTE","cites":`+cite+`}`, writer)
	n := decode[Note](t, w)
	if w.Code != http.StatusOK || n.Author.Kind != "agent" || n.Author.Name != "arisu" || n.Scope != "message" || n.MessageID != msg {
		t.Fatalf("put: %d %+v", w.Code, n)
	}
	w = put(`{"text":"rewritten SECRET-REST-NOTE","cites":`+cite+`}`, writer)
	if again := decode[Note](t, w); again.ID != n.ID || again.Text != "rewritten SECRET-REST-NOTE" {
		t.Errorf("overwrite: %+v", again)
	}
	for body, want := range map[string]openapi.Code{
		`{"text":"no cites","cites":[]}`:                    openapi.CodeInvalidBody,
		`{"text":"","cites":` + cite + `}`:                  openapi.CodeInvalidBody,
		`{"text":"bad cite","cites":[{"uri":"https://x"}]}`: openapi.CodeInvalidBody,
		`{"text":"x","cites":` + cite + `,"scope":"all"}`:   openapi.CodeInvalidBody,
	} {
		if w := put(body, writer); w.Code != http.StatusBadRequest || problemOf(t, w).Code != want {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	h.note(principalNamed(t, h, "agent", "momoi"), msg, store.ScopeThread, "momoi's thread note")
	h.note(h.human, msg, store.ScopeMessage, "the operator's note")

	for _, c := range []struct {
		bearer string
		want   []string
	}{
		{h.token(auth.MessageRead), []string{"rewritten SECRET-REST-NOTE"}},
		{h.tokenFor(h.human, auth.MessageRead), []string{"rewritten SECRET-REST-NOTE", "the operator's note", "momoi's thread note"}},
	} {
		m := decode[Message](t, h.get("/api/v1/messages/"+msg, c.bearer))
		var got []string
		for _, n := range m.Notes {
			got = append(got, n.Text)
		}
		if len(got) != len(c.want) {
			t.Errorf("notes %q, want %q", got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("notes %q, want %q", got, c.want)
			}
		}
	}

	if w := h.do(http.MethodDelete, "/api/v1/messages/"+msg+"/note", "", writer); w.Code != http.StatusNoContent {
		t.Errorf("delete: %d %s", w.Code, w.Body.String())
	}
	if w := h.do(http.MethodDelete, "/api/v1/messages/"+msg+"/note", "", writer); w.Code != http.StatusNotFound {
		t.Errorf("delete again: %d", w.Code)
	}
	var leaked int
	if err := h.pool.QueryRow(t.Context(), `SELECT count(*) FROM actions
		WHERE (args::text || coalesce(result::text, '')) LIKE '%SECRET-REST-NOTE%'`).Scan(&leaked); err != nil || leaked != 0 {
		t.Errorf("%d audit rows hold a note's text: %v", leaked, err)
	}
}
