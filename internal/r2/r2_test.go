package r2_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/r2"
)

const (
	testULID = "01M67SX66B38NKRKAYDXR834N3"
	// badRequestULID names a sent key the fake answers with a 400.
	badRequestULID = "01M67SX66B38NKRKAYDXR834N4"
)

func TestSentKey(t *testing.T) {
	// 23:30 on the last day of October in UTC-5 is already November in UTC.
	at := time.Date(2026, 10, 31, 23, 30, 0, 0, time.FixedZone("x", -5*3600))
	k := r2.SentKey(at, testULID)
	if k != "sent/2026/11/"+testULID+".eml" {
		t.Fatalf("SentKey = %q", k)
	}
	if err := r2.CheckSentKey(k); err != nil {
		t.Fatal(err)
	}
}

func TestCheckSentKeyRejects(t *testing.T) {
	for _, k := range []string{
		"raw/2026/10/" + testULID + ".eml",
		"pending/" + testULID,
		"",
		"sent/",
		"sent/2026/10/" + testULID,
		"sent/2026/10/" + testULID + ".eml/x",
		"sent/2026/10/" + strings.ToLower(testULID) + ".eml",
		"sent/2026/10/x.eml",
		"sent/2026/13/" + testULID + ".eml",
		"sent/2026/00/" + testULID + ".eml",
		"sent/2026/1/" + testULID + ".eml",
		"sent/+026/10/" + testULID + ".eml",
		"sent//2026/10/" + testULID + ".eml",
		"/sent/2026/10/" + testULID + ".eml",
		"sent/../raw/10/" + testULID + ".eml",
		"Sent/2026/10/" + testULID + ".eml",
	} {
		if err := r2.CheckSentKey(k); err == nil {
			t.Errorf("CheckSentKey(%q) accepted", k)
		}
	}
}

func TestRawBucketPutSent(t *testing.T) {
	f, cfg := setup(t)
	raw := r2.NewRawBucket(cfg)
	ctx := t.Context()
	key := "sent/2026/10/" + testULID + ".eml"
	body := []byte("From: a@example.com\r\n\r\nhi\r\n")

	if err := raw.PutSent(ctx, key, body); err != nil {
		t.Fatal(err)
	}
	req := f.reqs[len(f.reqs)-1]
	if req.Method != http.MethodPut || req.URL.Path != "/b/"+key {
		t.Fatalf("sent %s %s", req.Method, req.URL)
	}
	if got := req.Header.Get("If-None-Match"); got != "*" {
		t.Fatalf("If-None-Match = %q", got)
	}
	for h := range req.Header {
		if strings.HasPrefix(strings.ToLower(h), "x-amz-checksum") || strings.EqualFold(h, "X-Amz-Sdk-Checksum-Algorithm") {
			t.Errorf("PUT carries checksum header %s", h)
		}
	}

	if err := raw.PutSent(ctx, key, []byte("other bytes")); !errors.Is(err, r2.ErrExists) {
		t.Fatalf("second put: %v", err)
	}
	if got := string(f.objects["/b/"+key].body); got != string(body) {
		t.Fatalf("stored object replaced: %q", got)
	}

	n := len(f.reqs)
	for _, k := range []string{
		"raw/2026/10/" + testULID + ".eml",
		"pending/" + testULID,
		"sent/2026/10/" + testULID + ".eml/x",
	} {
		if err := raw.PutSent(ctx, k, body); err == nil {
			t.Errorf("PutSent(%q) accepted", k)
		}
	}
	if err := raw.PutSent(ctx, "sent/2026/11/"+testULID+".eml", make([]byte, r2.MaxObjectSize+1)); err == nil {
		t.Error("oversized put accepted")
	}
	if len(f.reqs) != n {
		t.Fatalf("a refused put still reached the server: %v", f.reqs[n:])
	}

	err := raw.PutSent(ctx, "sent/2026/10/"+badRequestULID+".eml", body)
	if err == nil || errors.Is(err, r2.ErrExists) {
		t.Fatalf("400 answer: %v", err)
	}
}

// fakeS3 answers just enough of the S3 protocol to check what the client
// sends: path-style addressing, no default checksums, pagination, and
// PUT with S3's conditional semantics (without If-None-Match it overwrites,
// as S3 and R2 do, so a test can tell the header is what protects a key).
// A stored object is served back with its x-amz-meta-* headers.
type fakeS3 struct {
	mu      sync.Mutex
	reqs    []*http.Request
	objects map[string]fakeObject
}

type fakeObject struct {
	body []byte
	meta http.Header
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r.Clone(context.Background()))
	switch {
	case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/"+badRequestULID+".eml"):
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `<Error><Code>InvalidRequest</Code><Message>no</Message></Error>`)
	case r.Method == http.MethodPut:
		if _, ok := f.objects[r.URL.Path]; ok && r.Header.Get("If-None-Match") == "*" {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = fmt.Fprint(w, `<Error><Code>PreconditionFailed</Code><Message>At least one of the pre-conditions you specified did not hold</Message></Error>`)
			return
		}
		meta := http.Header{}
		for h, v := range r.Header {
			if strings.HasPrefix(strings.ToLower(h), "x-amz-meta-") {
				meta[h] = v
			}
		}
		f.objects[r.URL.Path] = fakeObject{body, meta}
		w.Header().Set("ETag", `"x"`)
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
	case r.Method == http.MethodGet && f.objects[r.URL.Path].body != nil:
		o := f.objects[r.URL.Path]
		maps.Copy(w.Header(), o.meta)
		w.Header().Set("Content-Length", strconv.Itoa(len(o.body)))
		_, _ = w.Write(o.body)
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
	f := &fakeS3{objects: map[string]fakeObject{}}
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
