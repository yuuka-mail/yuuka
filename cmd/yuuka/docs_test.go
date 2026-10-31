package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The operator docs restate what the code reads, because operators do not
// read Go. These tests keep the two from drifting: every variable, command
// and flag the code knows must appear in docs/operate. They scan the
// package's source rather than run it, since flag sets and lookups live
// inside the commands; a name the scan misses is one more reason to spell
// it out as a literal.

func operateDoc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "operate", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// source returns the package's non-test Go source, concatenated.
func source(t *testing.T) string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(src)
	}
	return b.String()
}

// missing lists the names in want that doc lacks. A name must end where
// it ends in doc, so that --allow does not count as --all.
func missing(doc string, want map[string]bool) []string {
	var out []string
	for w := range want {
		if !regexp.MustCompile(regexp.QuoteMeta(w) + `([^A-Za-z0-9_-]|$)`).MatchString(doc) {
			out = append(out, w)
		}
	}
	sort.Strings(out)
	return out
}

func TestConfigDocumented(t *testing.T) {
	want := map[string]bool{}
	for _, m := range regexp.MustCompile(`"(YUUKA_[A-Z0-9_]*[A-Z0-9])"`).FindAllStringSubmatch(source(t), -1) {
		want[m[1]] = true
	}
	// The per-transport variables are built from the transport's name;
	// the docs write that name as <NAME>.
	_, _ = loadTransport(func(k string) string {
		want[k] = true
		return ""
	}, "<name>")
	if len(want) < 20 {
		t.Fatalf("found only %d variables; has the source changed shape?", len(want))
	}
	if m := missing(operateDoc(t, "config.md"), want); len(m) > 0 {
		t.Errorf("docs/operate/config.md does not mention %s", strings.Join(m, ", "))
	}
}

func TestCommandsDocumented(t *testing.T) {
	want := map[string]bool{}
	usages := []string{usage().Error(), prefixUsage, tokenUsage, outboxUsage, identityUsage, grantUsage}
	cmd := regexp.MustCompile(`yuuka ([a-z]+)(?: ([a-z]+(?:\|[a-z]+)*))?`)
	for _, u := range usages {
		for _, m := range cmd.FindAllStringSubmatch(u, -1) {
			if m[2] == "" {
				want["yuuka "+m[1]] = true
				continue
			}
			for _, sub := range strings.Split(m[2], "|") {
				want["yuuka "+m[1]+" "+sub] = true
			}
		}
	}
	flag := regexp.MustCompile(`fs\.(?:String|Int|Bool|Duration)\("([a-z-]+)"|fs\.Var\([^,]+, "([a-z-]+)"`)
	for _, m := range flag.FindAllStringSubmatch(source(t), -1) {
		want["--"+m[1]+m[2]] = true
	}
	if len(want) < 30 {
		t.Fatalf("found only %d commands and flags; has the source changed shape?", len(want))
	}
	if m := missing(operateDoc(t, "cli.md"), want); len(m) > 0 {
		t.Errorf("docs/operate/cli.md does not mention %s", strings.Join(m, ", "))
	}
}
