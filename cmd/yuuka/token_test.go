package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yuuka-mail/yuuka/internal/api"
	"github.com/yuuka-mail/yuuka/internal/authn"
	"github.com/yuuka-mail/yuuka/internal/mcp"
	"github.com/yuuka-mail/yuuka/internal/store"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// A token issued by the CLI works on REST and over MCP stdio, and stops
// working on both the moment the CLI revokes it.
func TestTokenLifecycle(t *testing.T) {
	t.Parallel()
	e := newHostEnv(t)
	for _, bad := range [][]string{
		{"issue", "--principal", "agent:arisu"},
		{"issue", "--principal", "agent:arisu", "--scope", ""},
		{"issue", "--principal", "agent:arisu", "--scope", "message:read,"},
		{"issue", "--principal", "agent:arisu", "--scope", "message:read,everything"},
		{"issue", "--principal", "human:operator", "--scope", "message:read"},
		{"issue", "--principal", "system:yuuka", "--scope", "message:read"},
		{"issue", "--principal", "arisu", "--scope", "message:read"},
		{"issue", "--principal", "agent:arisu", "--scope", "message:read", "--ttl", "-1h"},
		{"revoke", "nope"},
	} {
		if _, _, err := e.run("token", bad...); err == nil {
			t.Errorf("token %v: no error", bad)
		}
	}
	var n int
	if err := e.pool.QueryRow(t.Context(), `SELECT count(*) FROM principals WHERE kind <> 'system'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refused issues created %d principals: %v", n, err)
	}

	tok, notes, err := e.run("token", "issue", "--principal", "agent:arisu", "--scope", "prefix:read,message:read,prefix:read", "--label", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	tok = strings.TrimSpace(tok)
	if !strings.HasPrefix(tok, "ykt_") || !strings.Contains(notes, "created principal agent:arisu") {
		t.Fatalf("issue: %q %q", tok, notes)
	}
	actor, _, args := e.lastAction("auth.token.issue")
	if actor != store.SystemPrincipalID || args["via"] != "cli" || args["label"] != "laptop" {
		t.Errorf("issue recorded %s %v", actor, args)
	}
	if _, _, args := e.lastAction("principal.create"); args["principal"] != "agent:arisu" || args["via"] != "cli" {
		t.Errorf("principal recorded %v", args)
	}
	// A second token reuses the principal.
	if _, notes, err := e.run("token", "issue", "--principal", "agent:arisu", "--scope", "message:read", "--ttl", "24h"); err != nil || strings.Contains(notes, "created") {
		t.Errorf("second issue: %q %v", notes, err)
	}

	ls := e.must("token", "ls", "--principal", "agent:arisu")
	id := strings.SplitN(tok, "_", 3)[1]
	secret := strings.SplitN(tok, "_", 3)[2]
	if strings.Count(ls, "\n") != 3 || !strings.Contains(ls, id) || !strings.Contains(ls, "message:read,prefix:read") ||
		!strings.Contains(ls, "laptop") || strings.Contains(ls, secret) {
		t.Errorf("ls:\n%s", ls)
	}
	if _, _, err := e.run("token", "ls", "--principal", "agent:nobody"); err == nil {
		t.Error("ls of an unknown principal")
	}

	svc, err := authn.New(authn.Config{DB: e.db, Origin: "https://mail.example.com", Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	rest := api.New(e.db, nil, quiet).Handler(svc)
	restStatus := func() int {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/prefixes", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		rest.ServeHTTP(w, r)
		return w.Code
	}
	if code := restStatus(); code != http.StatusOK {
		t.Fatalf("REST with a fresh token: %d", code)
	}

	// The stdio form, over an in-memory transport.
	srv := mcp.New(mcp.Config{DB: e.db, Log: quiet, Version: "test"})
	ct, st := sdk.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.ServeToken(ctx, authn.NewBearer(e.db, quiet, nil), tok, st) }()
	t.Cleanup(func() {
		cancel()
		t.Log("server stopped:", <-done)
	})
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	listPrefixes := func() *sdk.CallToolResult {
		res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: "list_prefixes"})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := listPrefixes(); res.IsError {
		t.Fatalf("MCP with a fresh token: %+v", res.Content)
	}

	if out := e.must("token", "revoke", id); !strings.Contains(out, "revoked "+id) {
		t.Errorf("revoke: %q", out)
	}
	if _, _, args := e.lastAction("auth.credential.revoke"); args["credential_id"] != id || args["via"] != "cli" {
		t.Errorf("revoke recorded %v", args)
	}
	if code := restStatus(); code != http.StatusUnauthorized {
		t.Errorf("REST after revoke: %d", code)
	}
	if res := listPrefixes(); !res.IsError {
		t.Errorf("MCP after revoke: %+v", res.Content)
	}
	if _, _, err := e.run("token", "revoke", id); err == nil {
		t.Error("revoked twice")
	}
	if ls := e.must("token", "ls"); !strings.Contains(ls, "agent:arisu") || strings.Count(ls, "\n") != 3 {
		t.Errorf("ls after revoke:\n%s", ls)
	}
}
