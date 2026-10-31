package store_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// insertOutbox matches SQL that creates outbox rows (not
// outbox_recipients).
var insertOutbox = regexp.MustCompile(`(?i)insert\s+into\s+(\w+\.)?"?outbox"?[\s(]`)

// TestOneWayToTheOutbox is the source-level half of the one-way door of
// AGENTS.md (外发按身份授权): in every non-test Go file of the module,
//
//   - the only SQL that inserts an outbox row is in enqueueOutbox,
//   - enqueueOutbox is called only by (*PolicyTx).Enqueue,
//   - a PolicyTx is built only by (*DB).WithDecision, with its decision
//     from sendpolicy.Decide, and the decision is never assigned
//     elsewhere,
//   - PolicyTx has no exported field to change it through.
//
// So a new way to queue mail without a decision fails here, whether it is
// a new INSERT, a new caller, or a PolicyTx built by hand. The behavioural
// half is TestPolicyTx.
func TestOneWayToTheOutbox(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s: %v", root, err)
	}
	var inserts, callers, builders, assigns, exported []string
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// The directories the go command leaves out of ./... (names
			// starting with "." or "_", testdata), so that copies of the
			// module elsewhere in the tree, such as git worktrees under
			// .claude/, are not mistaken for this module's code.
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
				name == "testdata" || name == "node_modules" || name == "local") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, decl := range f.Decls {
			where := rel + ": package level"
			if fd, ok := decl.(*ast.FuncDecl); ok {
				where = rel + ": " + funcName(fd)
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.BasicLit:
					if x.Kind == token.STRING && insertOutbox.MatchString(x.Value) {
						inserts = append(inserts, where)
					}
				case *ast.CallExpr:
					if name(x.Fun) == "enqueueOutbox" {
						callers = append(callers, where)
					}
				case *ast.CompositeLit:
					if name(x.Type) == "PolicyTx" {
						builders = append(builders, where)
						if !decidedBySendpolicy(x) {
							t.Errorf("%s builds a PolicyTx whose decision is not sendpolicy.Decide(...)", where)
						}
					}
				case *ast.AssignStmt:
					for _, l := range x.Lhs {
						if s, ok := l.(*ast.SelectorExpr); ok && s.Sel.Name == "decision" {
							assigns = append(assigns, where)
						}
					}
				case *ast.TypeSpec:
					if st, ok := x.Type.(*ast.StructType); ok && x.Name.Name == "PolicyTx" {
						for _, fl := range st.Fields.List {
							for _, n := range fl.Names {
								if n.IsExported() {
									exported = append(exported, n.Name)
								}
							}
							if len(fl.Names) == 0 {
								exported = append(exported, "embedded field")
							}
						}
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		what      string
		got, want []string
	}{
		{"INSERT INTO outbox", inserts, []string{"internal/store/outbox.go: enqueueOutbox"}},
		{"calls of enqueueOutbox", callers, []string{"internal/store/policytx.go: (*PolicyTx).Enqueue"}},
		{"PolicyTx literals", builders, []string{"internal/store/policytx.go: (*DB).WithDecision"}},
		{"assignments to .decision", assigns, nil},
		{"exported PolicyTx fields", exported, nil},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s: %q, want %q", c.what, c.got, c.want)
		}
	}
}

func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	recv := fd.Recv.List[0].Type
	if s, ok := recv.(*ast.StarExpr); ok {
		return "(*" + name(s.X) + ")." + fd.Name.Name
	}
	return "(" + name(recv) + ")." + fd.Name.Name
}

// name is the identifier an expression ends in: x, pkg.x, *x.
func name(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.StarExpr:
		return name(x.X)
	case *ast.IndexExpr:
		return name(x.X)
	}
	return ""
}

// decidedBySendpolicy reports whether composite literal lit sets its
// decision field to a call of sendpolicy.Decide.
func decidedBySendpolicy(lit *ast.CompositeLit) bool {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok || name(kv.Key) != "decision" {
			continue
		}
		call, ok := kv.Value.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		return ok && name(sel.X) == "sendpolicy" && sel.Sel.Name == "Decide"
	}
	return false
}

func letterFrom(from, to string) []byte {
	return []byte("From: Someone <" + from + ">\r\nTo: " + to + "\r\nSubject: hi\r\n\r\nHi.\r\n")
}

// TestPolicyTx is the behavioural half of TestOneWayToTheOutbox: what
// (*PolicyTx).Enqueue refuses.
func TestPolicyTx(t *testing.T) {
	t.Parallel()
	e := newSendEnv(t)
	ctx := t.Context()
	outboxRows := func() int {
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	newOutbox := func(msg []byte, rcpts ...string) store.NewOutbox {
		at := e.at()
		o := store.NewOutbox{ID: ulid.New(at), Raw: store.NewSentRaw(ulid.New(at), "sent/2026/11/"+ulid.New(at)+".eml", msg, at), Message: msg}
		for _, r := range rcpts {
			o.Recipients = append(o.Recipients, store.OutboxRecipient{Addr: r, Kind: store.RecipientTo})
		}
		return o
	}
	decide := func(v *auth.Viewer, from string, rcpts []string, f func(context.Context, *store.PolicyTx) error) error {
		return e.db.WithDecision(ctx, v, store.Submission{FromAddr: from, Recipients: rcpts, Now: e.at()}, f)
	}
	bob := []string{"bob@example.net"}

	// Not from WithDecision.
	var zero store.PolicyTx
	if err := zero.Enqueue(ctx, newOutbox(letterFrom("github@example.com", "bob@example.net"), bob...)); !errors.Is(err, store.ErrNotApproved) {
		t.Errorf("zero PolicyTx: %v", err)
	}
	var none *store.PolicyTx
	if err := none.Enqueue(ctx, store.NewOutbox{}); !errors.Is(err, store.ErrNotApproved) {
		t.Errorf("nil PolicyTx: %v", err)
	}

	// A held decision (arisu as the operator's address: the owner cap)
	// cannot be queued, whatever a copy of it is changed to.
	err := decide(e.arisu, "github@example.com", bob, func(ctx context.Context, p *store.PolicyTx) error {
		d := p.Decision()
		if d.Queue || d.HoldReason != sendpolicy.HoldOwner {
			t.Errorf("decision %+v", d)
		}
		d.Queue, d.ApprovedBy = true, e.arisu.PrincipalID
		return p.Enqueue(ctx, newOutbox(letterFrom("github@example.com", "bob@example.net"), bob...))
	})
	if !errors.Is(err, store.ErrNotApproved) {
		t.Errorf("held decision: %v", err)
	}

	// An approved decision queues only what it was made on.
	for name, o := range map[string]store.NewOutbox{
		"more recipients":  newOutbox(letterFrom("github@example.com", "bob@example.net"), "bob@example.net", "eve@example.org"),
		"other recipients": newOutbox(letterFrom("github@example.com", "bob@example.net"), "eve@example.org"),
		"another From":     newOutbox(letterFrom("arisu@example.com", "bob@example.net"), bob...),
		"two From fields":  newOutbox(append([]byte("From: github@example.com\r\n"), letterFrom("github@example.com", "bob@example.net")...), bob...),
		"two From in one":  newOutbox(letterFrom("github@example.com>, <arisu@example.com", "bob@example.net"), bob...),
		"bytes not the raw": func() store.NewOutbox {
			o := newOutbox(letterFrom("github@example.com", "bob@example.net"), bob...)
			o.Raw.Size++
			return o
		}(),
	} {
		err := decide(e.op, "github@example.com", bob, func(ctx context.Context, p *store.PolicyTx) error {
			if !p.Decision().Queue {
				t.Fatalf("operator as own identity: %+v", p.Decision())
			}
			return p.Enqueue(ctx, o)
		})
		if !errors.Is(err, store.ErrNotApproved) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if n := outboxRows(); n != 0 {
		t.Fatalf("%d outbox rows after refusals", n)
	}

	// The row is the decision's: identity, From, submitter, approver.
	// One decision, one outcome.
	o := newOutbox(letterFrom("GitHub@example.com", "Bob@example.net"), "Bob@example.net")
	err = decide(e.op, "GitHub@example.com", []string{"BOB@example.net"}, func(ctx context.Context, p *store.PolicyTx) error {
		if err := p.Enqueue(ctx, o); err != nil {
			return err
		}
		if err := p.Enqueue(ctx, newOutbox(letterFrom("github@example.com", "bob@example.net"), bob...)); !errors.Is(err, store.ErrNotApproved) {
			t.Errorf("second Enqueue: %v", err)
		}
		if err := p.HoldDraft(ctx, ulid.New(e.at()), e.at()); !errors.Is(err, store.ErrNotApproved) {
			t.Errorf("HoldDraft after Enqueue: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var identity, from, by, approved, state string
	if err := e.pool.QueryRow(ctx, `SELECT identity_id, from_addr, submitted_by, approved_by, state FROM outbox WHERE id = $1`, o.ID).
		Scan(&identity, &from, &by, &approved, &state); err != nil {
		t.Fatal(err)
	}
	if identity != e.star.ID || from != "github@example.com" || by != e.op.PrincipalID || approved != e.op.PrincipalID || state != "queued" {
		t.Errorf("row: %s %s %s %s %s", identity, from, by, approved, state)
	}
}

// Two submissions racing for the last slots of max_per_hour: the grant's
// lock lets exactly the allowed number through.
func TestPolicyTxRace(t *testing.T) {
	t.Parallel()
	e := newSendEnv(t)
	ctx := t.Context()
	at := e.at()
	limit := 2
	if err := e.db.SetGrant(ctx, store.Grant{
		IdentityID: e.arisuID.ID, PrincipalID: e.arisu.PrincipalID, Policy: sendpolicy.Auto,
		RecipientScope: sendpolicy.ScopeAny, MaxPerHour: &limit,
	}, store.Action{ID: ulid.New(at), Tool: "grant.set", At: at}); err != nil {
		t.Fatal(err)
	}
	const n = 6
	var mu sync.Mutex
	var queued, held int
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			err := e.db.WithDecision(ctx, e.arisu, store.Submission{FromAddr: "arisu@example.com", Recipients: []string{"bob@example.net"}, Now: time.Now()},
				func(ctx context.Context, p *store.PolicyTx) error {
					if !p.Decision().Queue {
						mu.Lock()
						held++
						mu.Unlock()
						return nil
					}
					at := e.at()
					msg := letterFrom("arisu@example.com", "bob@example.net")
					err := p.Enqueue(ctx, store.NewOutbox{
						ID: ulid.New(at), Raw: store.NewSentRaw(ulid.New(at), "sent/2026/11/"+ulid.New(at)+".eml", msg, at), Message: msg,
						Recipients: []store.OutboxRecipient{{Addr: "bob@example.net", Kind: store.RecipientTo}},
					})
					if err == nil {
						mu.Lock()
						queued++
						mu.Unlock()
					}
					return err
				})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if queued != limit || held != n-limit {
		t.Errorf("%d queued, %d held; want %d and %d", queued, held, limit, n-limit)
	}
}
