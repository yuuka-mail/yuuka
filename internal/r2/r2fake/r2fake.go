// Package r2fake is an in-memory stand-in for an R2 bucket, for tests.
package r2fake

import (
	"context"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/yuuka-mail/yuuka/internal/r2"
)

// Bucket is safe for concurrent use. Err, when set, makes every call fail.
type Bucket struct {
	mu      sync.Mutex
	objects map[string]r2.Object
	Err     error
}

// New returns an empty bucket.
func New() *Bucket { return &Bucket{objects: map[string]r2.Object{}} }

// Put stores a copy of body under key, replacing what is there. It is a
// seeding helper for tests, standing in for the Worker; code under test
// writes through PutSent.
func (b *Bucket) Put(key string, body []byte, meta map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[key] = r2.Object{Body: slices.Clone(body), Metadata: maps.Clone(meta)}
}

// PutSent behaves like r2.RawBucket.PutSent: only keys that pass
// r2.CheckSentKey, create-only, ErrExists when the key is taken.
func (b *Bucket) PutSent(_ context.Context, key string, body []byte) error {
	if err := r2.CheckSentKey(key); err != nil {
		return err
	}
	if len(body) > r2.MaxObjectSize {
		return fmt.Errorf("put %s: larger than %d bytes", key, r2.MaxObjectSize)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.Err != nil {
		return b.Err
	}
	if _, ok := b.objects[key]; ok {
		return fmt.Errorf("put %s: %w", key, r2.ErrExists)
	}
	b.objects[key] = r2.Object{Body: slices.Clone(body), Metadata: map[string]string{}}
	return nil
}

// Has reports whether key exists.
func (b *Bucket) Has(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.objects[key]
	return ok
}

// Get implements the bucket read.
func (b *Bucket) Get(_ context.Context, key string) (r2.Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.Err != nil {
		return r2.Object{}, b.Err
	}
	o, ok := b.objects[key]
	if !ok {
		return r2.Object{}, fmt.Errorf("get %s: %w", key, r2.ErrNotFound)
	}
	return r2.Object{Body: slices.Clone(o.Body), Metadata: maps.Clone(o.Metadata)}, nil
}

// List yields keys under prefix in lexical order, like S3.
func (b *Bucket) List(_ context.Context, prefix string) iter.Seq2[string, error] {
	b.mu.Lock()
	err := b.Err
	var keys []string
	for k := range b.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	b.mu.Unlock()
	slices.Sort(keys)
	return func(yield func(string, error) bool) {
		if err != nil {
			yield("", err)
			return
		}
		for _, k := range keys {
			if !yield(k, nil) {
				return
			}
		}
	}
}

// Delete removes key; a missing key is not an error.
func (b *Bucket) Delete(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.Err != nil {
		return b.Err
	}
	delete(b.objects, key)
	return nil
}
