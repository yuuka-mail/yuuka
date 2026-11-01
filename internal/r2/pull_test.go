package r2_test

import (
	"encoding/base64"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yuuka-mail/yuuka/internal/r2"
)

func testPullMeta() r2.PullMeta {
	return r2.PullMeta{
		Account:      "personal",
		SourceKey:    "loc:1700000000:42:Archive/2026",
		Mailbox:      "Archive/2026",
		UIDValidity:  1700000000,
		UID:          42,
		InternalDate: time.Date(2026, 10, 31, 23, 30, 0, 0, time.FixedZone("x", -5*3600)),
	}
}

func TestPullKey(t *testing.T) {
	// 23:30 on the last day of October in UTC-5 is already November in UTC.
	at := time.Date(2026, 10, 31, 23, 30, 0, 0, time.FixedZone("x", -5*3600))
	k := r2.PullKey(at, testULID)
	if k != "pull/2026/11/"+testULID+".eml" {
		t.Fatalf("PullKey = %q", k)
	}
	if err := r2.CheckPullKey(k); err != nil {
		t.Fatal(err)
	}
	if err := r2.CheckSentKey(k); err == nil {
		t.Fatal("CheckSentKey accepted a pull key")
	}
	if err := r2.CheckPullKey(r2.SentKey(at, testULID)); err == nil {
		t.Fatal("CheckPullKey accepted a sent key")
	}
}

// refusedKeys are keys no Go write may reach the raw bucket with, whatever
// the prefix it is for (§2.2).
func refusedKeys(prefix string) []string {
	return []string{
		"raw/2026/10/" + testULID + ".eml",
		"pending/" + testULID,
		"",
		prefix + "/",
		prefix + "/2026/10/" + testULID,
		prefix + "/2026/10/" + testULID + ".eml/x",
		prefix + "/2026/10/" + strings.ToLower(testULID) + ".eml",
		prefix + "/2026/10/x.eml",
		prefix + "/2026/10/" + testULID[:25] + ".eml",
		prefix + "/2026/10/8" + testULID[1:] + ".eml",
		prefix + "/2026/13/" + testULID + ".eml",
		prefix + "/2026/00/" + testULID + ".eml",
		prefix + "/2026/1/" + testULID + ".eml",
		prefix + "/26/10/" + testULID + ".eml",
		prefix + "/0000/10/" + testULID + ".eml",
		prefix + "/+026/10/" + testULID + ".eml",
		prefix + "/2026-10/" + testULID + ".eml",
		prefix + "//2026/10/" + testULID + ".eml",
		"/" + prefix + "/2026/10/" + testULID + ".eml",
		prefix + "/../raw/10/" + testULID + ".eml",
		prefix + "/2026/../../raw/" + testULID + ".eml",
		prefix + "/2026/./10/" + testULID + ".eml",
		strings.ToUpper(prefix[:1]) + prefix[1:] + "/2026/10/" + testULID + ".eml",
		prefix + "x/2026/10/" + testULID + ".eml",
		"x/" + prefix + "/2026/10/" + testULID + ".eml",
	}
}

func TestCheckPullKeyRejects(t *testing.T) {
	for _, k := range append(refusedKeys("pull"), "sent/2026/10/"+testULID+".eml") {
		if err := r2.CheckPullKey(k); err == nil {
			t.Errorf("CheckPullKey(%q) accepted", k)
		}
	}
}

// TestRawBucketGate is the code gate of §2.2: a key outside the exact
// sent/ and pull/ shapes never becomes a request, whichever write names it.
func TestRawBucketGate(t *testing.T) {
	f, cfg := setup(t)
	raw := r2.NewRawBucket(cfg)
	ctx := t.Context()
	body := []byte("From: a@example.com\r\n\r\nhi\r\n")

	for _, k := range append(refusedKeys("sent"), "pull/2026/10/"+testULID+".eml") {
		if err := raw.PutSent(ctx, k, body); err == nil {
			t.Errorf("PutSent(%q) accepted", k)
		}
	}
	for _, k := range append(refusedKeys("pull"), "sent/2026/10/"+testULID+".eml") {
		if err := raw.PutPull(ctx, k, body, testPullMeta()); err == nil {
			t.Errorf("PutPull(%q) accepted", k)
		}
	}
	if len(f.reqs) != 0 {
		t.Fatalf("a refused put still reached the server: %v", f.reqs)
	}
}

// TestRawBucketMethods pins the raw bucket's method set: reads, and the two
// create-only writes §2.2 allows. A new write or a delete has to come
// through here, and through §2.2, first.
func TestRawBucketMethods(t *testing.T) {
	var got []string
	typ := reflect.TypeFor[*r2.RawBucket]()
	for i := range typ.NumMethod() {
		got = append(got, typ.Method(i).Name)
	}
	slices.Sort(got)
	if want := []string{"Get", "List", "PutPull", "PutSent"}; !slices.Equal(got, want) {
		t.Fatalf("RawBucket methods = %v, want %v", got, want)
	}
}

func TestRawBucketPutPull(t *testing.T) {
	f, cfg := setup(t)
	raw := r2.NewRawBucket(cfg)
	ctx := t.Context()
	key := "pull/2026/10/" + testULID + ".eml"
	body := []byte("From: a@example.com\r\n\r\nhi\r\n")
	meta := testPullMeta()
	enc, err := meta.Encode()
	if err != nil {
		t.Fatal(err)
	}

	if err := raw.PutPull(ctx, key, body, meta); err != nil {
		t.Fatal(err)
	}
	req := f.reqs[len(f.reqs)-1]
	if req.Method != http.MethodPut || req.URL.Path != "/b/"+key {
		t.Fatalf("sent %s %s", req.Method, req.URL)
	}
	if got := req.Header.Get("If-None-Match"); got != "*" {
		t.Fatalf("If-None-Match = %q", got)
	}
	if got := req.Header.Get("X-Amz-Meta-Yuuka-Pull"); got != enc {
		t.Fatalf("x-amz-meta-yuuka-pull = %q, want %q", got, enc)
	}

	o, err := raw.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r2.DecodePullMeta(o.Metadata[r2.PullMetadataKey])
	if err != nil {
		t.Fatal(err)
	}
	if !got.InternalDate.Equal(meta.InternalDate) {
		t.Fatalf("internal_date = %v", got.InternalDate)
	}
	got.InternalDate = meta.InternalDate
	if got != meta {
		t.Fatalf("metadata read back as %+v", got)
	}

	// A retry whose first try landed: 412, read back, same bytes and
	// metadata, so it succeeds.
	n := len(f.reqs)
	if err := raw.PutPull(ctx, key, body, meta); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	if len(f.reqs) != n+2 || f.reqs[n+1].Method != http.MethodGet {
		t.Fatalf("retry did not read back: %v", f.reqs[n:])
	}

	other := meta
	other.UID = 43
	for name, put := range map[string]func() error{
		"bytes":    func() error { return raw.PutPull(ctx, key, []byte("other bytes"), meta) },
		"metadata": func() error { return raw.PutPull(ctx, key, body, other) },
	} {
		if err := put(); err == nil || errors.Is(err, r2.ErrExists) {
			t.Errorf("different %s: %v", name, err)
		}
	}
	if o := f.objects["/b/"+key]; string(o.body) != string(body) || o.meta.Get("X-Amz-Meta-Yuuka-Pull") != enc {
		t.Fatalf("stored object replaced: %q %v", o.body, o.meta)
	}

	n = len(f.reqs)
	big := meta
	big.Mailbox = strings.Repeat("m", r2.MaxPullMetadata)
	if err := raw.PutPull(ctx, "pull/2026/11/"+testULID+".eml", body, big); err == nil {
		t.Error("oversized metadata accepted")
	}
	if err := raw.PutPull(ctx, "pull/2026/11/"+testULID+".eml", make([]byte, r2.MaxObjectSize+1), meta); err == nil {
		t.Error("oversized body accepted")
	}
	if err := raw.PutPull(ctx, "pull/2026/11/"+testULID+".eml", body, r2.PullMeta{}); err == nil {
		t.Error("empty metadata accepted")
	}
	if len(f.reqs) != n {
		t.Fatalf("a refused put still reached the server: %v", f.reqs[n:])
	}

	err = raw.PutPull(ctx, "pull/2026/10/"+badRequestULID+".eml", body, meta)
	if err == nil || errors.Is(err, r2.ErrExists) {
		t.Fatalf("400 answer: %v", err)
	}
}

func TestPullMetaEncoding(t *testing.T) {
	m := testPullMeta()
	m.Mailbox = "Входящие/日本"
	v, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Trim(v, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
		t.Fatalf("value is not base64url without padding: %q", v)
	}
	j, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		t.Fatal(err)
	}
	// The wire shape: field names are what a recovery tool reads, so they
	// are pinned here rather than derived from the struct.
	want := `{"v":1,"account":"personal","source_key":"loc:1700000000:42:Archive/2026",` +
		`"mailbox":"Входящие/日本","uidvalidity":1700000000,"uid":42,"internal_date":"2026-10-31T23:30:00-05:00"}`
	if string(j) != want {
		t.Fatalf("JSON = %s", j)
	}
	got, err := r2.DecodePullMeta(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, off := got.InternalDate.Zone(); off != -5*3600 || !got.InternalDate.Equal(m.InternalDate) {
		t.Fatalf("internal_date = %v", got.InternalDate)
	}
	got.InternalDate = m.InternalDate
	if got != m {
		t.Fatalf("round trip = %+v", got)
	}

	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	// A later version may add fields; this one ignores them.
	if _, err := r2.DecodePullMeta(enc(strings.Replace(want, `"v":1,`, `"v":1,"extra":true,`, 1))); err != nil {
		t.Fatalf("unknown field: %v", err)
	}
	for name, v := range map[string]string{
		"version 2":      enc(strings.Replace(want, `"v":1`, `"v":2`, 1)),
		"no version":     enc(strings.Replace(want, `"v":1,`, ``, 1)),
		"no uid":         enc(strings.Replace(want, `"uid":42,`, ``, 1)),
		"no date":        enc(strings.Replace(want, `,"internal_date":"2026-10-31T23:30:00-05:00"`, ``, 1)),
		"empty account":  enc(strings.Replace(want, `"account":"personal"`, `"account":""`, 1)),
		"uid too large":  enc(strings.Replace(want, `"uid":42`, `"uid":4294967296`, 1)),
		"not JSON":       enc("{"),
		"padded":         base64.URLEncoding.EncodeToString([]byte(want)),
		"std base64":     base64.RawStdEncoding.EncodeToString([]byte(`{"v":1,"account":"??>"}`)),
		"over the bound": strings.Repeat("A", r2.MaxPullMetadata),
	} {
		if _, err := r2.DecodePullMeta(v); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPullMetaEncodeRefuses(t *testing.T) {
	for name, edit := range map[string]func(*r2.PullMeta){
		"no account":     func(m *r2.PullMeta) { m.Account = "" },
		"no source key":  func(m *r2.PullMeta) { m.SourceKey = "" },
		"no mailbox":     func(m *r2.PullMeta) { m.Mailbox = "" },
		"no uidvalidity": func(m *r2.PullMeta) { m.UIDValidity = 0 },
		"no uid":         func(m *r2.PullMeta) { m.UID = 0 },
		"no date":        func(m *r2.PullMeta) { m.InternalDate = time.Time{} },
		// encoding/json would store U+FFFD in its place.
		"bad UTF-8": func(m *r2.PullMeta) { m.Mailbox = "INBOX\xff" },
		"oversize":  func(m *r2.PullMeta) { m.SourceKey = strings.Repeat("k", r2.MaxPullMetadata) },
	} {
		m := testPullMeta()
		edit(&m)
		if _, err := m.Encode(); err == nil {
			t.Errorf("%s: encoded", name)
		}
	}

	// The bound is on key plus value; find the largest mailbox that fits
	// and check the next byte does not.
	m := testPullMeta()
	lo, hi := 0, r2.MaxPullMetadata
	for lo < hi {
		mid := (lo + hi + 1) / 2
		m.Mailbox = strings.Repeat("m", mid)
		if _, err := m.Encode(); err == nil {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	m.Mailbox = strings.Repeat("m", lo)
	v, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(r2.PullMetadataKey) + len(v); n > r2.MaxPullMetadata || n < r2.MaxPullMetadata-3 {
		t.Fatalf("largest accepted value is %d bytes with its key", n)
	}
	if _, err := r2.DecodePullMeta(v); err != nil {
		t.Fatalf("decode the largest value: %v", err)
	}
}
