package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
)

type hostEnv struct {
	t    *testing.T
	db   *store.DB
	pool *pgxpool.Pool
}

func newHostEnv(t *testing.T) *hostEnv {
	t.Helper()
	db, pool := storetest.New(t)
	return &hostEnv{t: t, db: db, pool: pool}
}

// run runs one host command line and returns stdout, stderr and the error.
func (e *hostEnv) run(cmd string, args ...string) (string, string, error) {
	e.t.Helper()
	var out, msg bytes.Buffer
	h := &host{db: e.db, out: &out, msg: &msg, now: time.Now, user: "tester"}
	var err error
	switch cmd {
	case "prefix":
		err = h.prefixCmd(e.t.Context(), args)
	case "token":
		err = h.tokenCmd(e.t.Context(), args)
	default:
		e.t.Fatalf("no command %s", cmd)
	}
	return out.String(), msg.String(), err
}

func (e *hostEnv) must(cmd string, args ...string) string {
	e.t.Helper()
	out, _, err := e.run(cmd, args...)
	if err != nil {
		e.t.Fatalf("%s %v: %v", cmd, args, err)
	}
	return out
}

// lastAction returns actor, credential and args of the newest action
// with tool.
func (e *hostEnv) lastAction(tool string) (actor string, credential *string, args map[string]any) {
	e.t.Helper()
	err := e.pool.QueryRow(e.t.Context(), `SELECT actor, credential_id, args FROM actions WHERE tool = $1 ORDER BY id DESC LIMIT 1`, tool).
		Scan(&actor, &credential, &args)
	if err != nil {
		e.t.Fatalf("action %s: %v", tool, err)
	}
	return actor, credential, args
}
