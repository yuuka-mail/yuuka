package api

import (
	"net/http"
	"slices"
	"testing"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// The kinds the API offers are the kinds the log holds.
func TestChangeKindsMatchTheLog(t *testing.T) {
	if got := agentview.ChangeKind("").Enum(); !slices.Equal(got, store.ChangeKinds) {
		t.Errorf("ChangeKind %q, store %q", got, store.ChangeKinds)
	}
}

func TestChanges(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	reader := h.token(auth.MessageRead)
	h.spy.take()

	// Without since: where the log is now, nothing listed.
	now := decode[ChangePage](t, h.get("/api/v1/changes", reader))
	if len(now.Changes) != 0 || now.More || now.Cursor == "" {
		t.Fatalf("from now: %+v", now)
	}
	msg := decode[MessagePage](t, h.get("/api/v1/messages?limit=1", reader)).Messages[0]
	h.annotation(h.agent, msg.ID, store.SetLabel("followed", true))
	got := decode[ChangePage](t, h.get("/api/v1/changes?since="+now.Cursor, reader))
	if len(got.Changes) != 1 || got.Changes[0].Kind != "flag" || got.Changes[0].ID != msg.ID || got.More || got.Cursor == now.Cursor {
		t.Errorf("after a label: %+v", got)
	}

	// From the start of the log: the kinds the scopes cover, unless
	// asked for. Without delivery:read, no delivery or outbox changes.
	start := agentview.ChangeCursor(0)
	all := func(bearer, query string) map[agentview.ChangeKind]int {
		kinds := map[agentview.ChangeKind]int{}
		cursor := start
		for {
			w := h.get("/api/v1/changes?limit=500&since="+cursor+query, bearer)
			if w.Code != http.StatusOK {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			p := decode[ChangePage](t, w)
			for _, c := range p.Changes {
				kinds[c.Kind]++
				if (c.Kind == "delivery") != (c.MessageID != "") {
					t.Errorf("%s %s: message_id %q", c.Kind, c.ID, c.MessageID)
				}
			}
			if cursor = p.Cursor; !p.More {
				return kinds
			}
		}
	}
	narrow := all(reader, "")
	if narrow["message"] == 0 || narrow["flag"] == 0 || narrow["delivery"] != 0 || narrow["prefix"] != 0 {
		t.Errorf("with message:read: %v", narrow)
	}
	wide := all(h.token(auth.MessageRead, auth.DeliveryRead, auth.PrefixRead), "")
	if wide["delivery"] == 0 || wide["prefix"] == 0 || wide["message"] != narrow["message"] {
		t.Errorf("with delivery:read and prefix:read: %v", wide)
	}
	if only := all(reader, "&kinds=flag"); len(only) != 1 || only["flag"] == 0 {
		t.Errorf("kinds=flag: %v", only)
	}
	h.spy.take()

	// Asking for a kind beyond the scopes is refused, not filtered.
	w := h.get("/api/v1/changes?kinds=message,delivery", reader)
	if w.Code != http.StatusForbidden || problemOf(t, w).Code != openapi.CodeScopeRequired {
		t.Errorf("kinds=delivery without delivery:read: %d %s", w.Code, w.Body.String())
	}
	if calls := h.spy.take(); len(calls) != 0 {
		t.Errorf("a refused request reached the store: %v", calls)
	}
	for q, want := range map[string]openapi.Code{
		"since=" + agentview.ChangeCursor(1<<40): openapi.CodeCursorExpired,
		"since=42":                               openapi.CodeInvalidParam,
		"kinds=mail":                             openapi.CodeInvalidParam,
	} {
		w := h.get("/api/v1/changes?"+q, reader)
		if p := problemOf(t, w); p.Code != want {
			t.Errorf("%s: %d %+v, want %s", q, w.Code, p, want)
		}
	}
	if w := h.get("/api/v1/changes?since="+agentview.ChangeCursor(1<<40), reader); w.Code != http.StatusGone {
		t.Errorf("expired cursor: %d", w.Code)
	}
}
