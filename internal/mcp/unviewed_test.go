package mcp_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// The tools reach the store only through methods that take the caller's
// Viewer (§13.8), so what an agent sees is decided where everything
// else's is. The few that take none are listed with why. A source's
// configuration and password, and who reads which account, are read and
// changed by methods without a Viewer: no tool can reach them (§9.4).
var mcpUnviewed = map[string]string{
	"RecordAction": "the audit record of the call itself, in the caller's name",
}

func TestToolsReadAsTheViewer(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	viewer := reflect.TypeFor[*auth.Viewer]()
	db := reflect.TypeFor[*store.DB]()
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			// x.db.Method
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			inner, ok := sel.X.(*ast.SelectorExpr)
			if !ok || inner.Sel.Name != "db" {
				return true
			}
			seen[sel.Sel.Name] = true
			return true
		})
	}
	if !seen["Search"] || !seen["VisibleAccounts"] {
		t.Fatalf("found only %v: the walk is broken", seen)
	}
	for name := range seen {
		m, ok := db.MethodByName(name)
		if !ok {
			t.Errorf("%s is not a store method", name)
			continue
		}
		takes := false
		for i := range m.Type.NumIn() {
			takes = takes || m.Type.In(i) == viewer
		}
		if _, listed := mcpUnviewed[name]; !takes && !listed {
			t.Errorf("a tool calls %s, which takes no Viewer: what it reads or writes is not decided by what the caller may see", name)
		}
	}
}
