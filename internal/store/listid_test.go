package store_test

import (
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/authres"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/store/storetest"
)

// messages.list_id (0010) is the List-Id's identifier, ” without one;
// a row from before the migration reads NULL until the backlog pass
// rebuilds it from its raw.
func TestListID(t *testing.T) {
	t.Parallel()
	db, pool := storetest.New(t)
	ctx := t.Context()
	acct := storetest.Account(t, db)
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	listed := letter("a@example.net", "dev@example.com", "Re: plan", "List-Id: The dev list <dev.lists.example.org>\r\n", "x")
	plain := letter("b@example.net", "dev@example.com", "Hello", "", "y")
	withList := inbound(t, db, acct, "dev", listed, at)
	without := inbound(t, db, acct, "dev", plain, at.Add(time.Minute))
	for id, want := range map[string]string{withList: "dev.lists.example.org", without: ""} {
		var got string
		if err := pool.QueryRow(ctx, `SELECT list_id FROM messages WHERE id = $1`, id).Scan(&got); err != nil || got != want {
			t.Errorf("list_id of %s = %q, %v; want %q", id, got, err, want)
		}
	}

	if _, err := pool.Exec(ctx, `UPDATE messages SET list_id = NULL WHERE id = $1`, withList); err != nil {
		t.Fatal(err)
	}
	pending, err := db.UnreadListIDs(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].MessageID != withList {
		t.Fatalf("unread list ids %+v, %v", pending, err)
	}
	d := pending[0].Deliveries[0]
	if _, err := db.Rebuild(ctx, pending[0], mimeparse.Parse(listed), map[string]authres.Auth{d.DeliveryID: authres.Extract(listed, "mx.cloudflare.net")}); err != nil {
		t.Fatal(err)
	}
	if pending, err := db.UnreadListIDs(ctx, 10); err != nil || len(pending) != 0 {
		t.Errorf("after rebuild: %+v, %v", pending, err)
	}
}
