package r2_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yuuka-mail/yuuka/internal/r2"
)

// fakeS3 answers just enough of the S3 protocol to check what the client
// sends: path-style addressing, no default checksums, pagination.
type fakeS3 struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r.Clone(context.Background()))
	f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Query().Get("continuation-token") == "" {
			_, _ = fmt.Fprint(w, `<ListBucketResult><Name>b</Name><Prefix>pending/</Prefix><IsTruncated>true</IsTruncated>`+
				`<NextContinuationToken>t2</NextContinuationToken><Contents><Key>pending/A</Key></Contents></ListBucketResult>`)
			return
		}
		_, _ = fmt.Fprint(w, `<ListBucketResult><Name>b</Name><Prefix>pending/</Prefix><IsTruncated>false</IsTruncated>`+
			`<Contents><Key>pending/B</Key></Contents></ListBucketResult>`)
	case r.Method == http.MethodGet && r.URL.Path == "/b/raw/2026/10/x.eml":
		w.Header().Set("X-Amz-Meta-Yuuka-Marker", "eyJ2IjoxfQ")
		w.Header().Set("Content-Length", "5")
		_, _ = fmt.Fprint(w, "hello")
	case r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(w, `<Error><Code>NoSuchKey</Code><Message>nope</Message></Error>`)
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func setup(t *testing.T) (*fakeS3, r2.Config) {
	t.Helper()
	f := &fakeS3{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, r2.Config{Endpoint: srv.URL, Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s"}
}

func TestRawBucket(t *testing.T) {
	f, cfg := setup(t)
	raw := r2.NewRawBucket(cfg)
	ctx := t.Context()

	o, err := raw.Get(ctx, "raw/2026/10/x.eml")
	if err != nil {
		t.Fatal(err)
	}
	if string(o.Body) != "hello" || o.Metadata["yuuka-marker"] != "eyJ2IjoxfQ" {
		t.Fatalf("got %q %v", o.Body, o.Metadata)
	}
	if _, err := raw.Get(ctx, "raw/2026/10/missing.eml"); !errors.Is(err, r2.ErrNotFound) {
		t.Fatalf("missing key: %v", err)
	}
	for _, r := range f.reqs {
		if !strings.HasPrefix(r.URL.Path, "/b/") {
			t.Errorf("not path-style: %s", r.URL)
		}
		if r.Header.Get("X-Amz-Checksum-Mode") != "" {
			t.Errorf("response checksum validation requested: %v", r.Header)
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			t.Errorf("raw bucket issued %s", r.Method)
		}
	}
}

func TestPendingBucket(t *testing.T) {
	f, cfg := setup(t)
	p := r2.NewPendingBucket(cfg)
	ctx := t.Context()

	var keys []string
	for k, err := range p.List(ctx, "pending/") {
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	if strings.Join(keys, ",") != "pending/A,pending/B" {
		t.Fatalf("List = %v", keys)
	}
	if err := p.Delete(ctx, "pending/A"); err != nil {
		t.Fatal(err)
	}
	last := f.reqs[len(f.reqs)-1]
	if last.Method != http.MethodDelete || last.URL.Path != "/b/pending/A" {
		t.Fatalf("delete sent %s %s", last.Method, last.URL)
	}
	for _, r := range f.reqs {
		for h := range r.Header {
			if strings.HasPrefix(strings.ToLower(h), "x-amz-checksum") || strings.EqualFold(h, "X-Amz-Sdk-Checksum-Algorithm") {
				t.Errorf("%s %s carries checksum header %s", r.Method, r.URL, h)
			}
		}
	}
}
