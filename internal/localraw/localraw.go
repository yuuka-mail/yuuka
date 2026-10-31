// Package localraw keeps the local copy of raw MIME next to R2 (§3
// principle 1): the same raw/YYYY/MM/<id>.eml (received) or
// sent/YYYY/MM/<id>.eml (sent) key under a data directory, written once and
// never overwritten.
package localraw

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Dir is a local raw store rooted at a directory.
type Dir struct {
	Root string
}

// Path returns the file path for key, rejecting keys outside raw/ and sent/
// and keys that could escape Root.
func (d Dir) Path(key string) (string, error) {
	known := strings.HasPrefix(key, "raw/") || strings.HasPrefix(key, "sent/")
	if !known || !filepath.IsLocal(key) || filepath.ToSlash(filepath.Clean(key)) != key {
		return "", fmt.Errorf("localraw: refusing key %q", key)
	}
	return filepath.Join(d.Root, filepath.FromSlash(key)), nil
}

// Put stores data under key. The write is atomic (temp file, fsync, then a
// hard link that fails if the target exists), so a crash never leaves a
// partial file at key and a second writer can never replace the first.
// If key already exists with the same bytes, Put succeeds; with different
// bytes it fails, because both kinds of copy are immutable and that means
// corruption.
func (d Dir) Put(key string, data []byte) error {
	path, err := d.Path(key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("localraw: %w", err)
	}
	tmp, err := writeTemp(dir, data)
	if tmp != "" {
		// After the link below the data lives on under path; the temp
		// name is only scaffolding.
		defer func() { _ = os.Remove(tmp) }()
	}
	if err != nil {
		return fmt.Errorf("localraw: write %s: %w", key, err)
	}
	// Link, not rename: rename would silently replace an existing file.
	if err := os.Link(tmp, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return sameContent(path, key, data)
		}
		return fmt.Errorf("localraw: %w", err)
	}
	return syncDir(dir)
}

// Get returns the bytes stored under key; a missing key is an error
// wrapping fs.ErrNotExist.
func (d Dir) Get(key string) ([]byte, error) {
	path, err := d.Path(key)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("localraw: %w", err)
	}
	return b, nil
}

// writeTemp writes data to a new read-only temp file in dir and fsyncs it.
func writeTemp(dir string, data []byte) (name string, err error) {
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	if _, err := f.Write(data); err != nil {
		return f.Name(), err
	}
	if err := f.Chmod(0o440); err != nil {
		return f.Name(), err
	}
	return f.Name(), f.Sync()
}

func sameContent(path, key string, data []byte) error {
	existing, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("localraw: %w", err)
	}
	if a, b := sha256.Sum256(existing), sha256.Sum256(data); !bytes.Equal(a[:], b[:]) {
		return fmt.Errorf("localraw: %s already exists with different content", key)
	}
	return nil
}

// syncDir makes the new directory entry durable.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("localraw: %w", err)
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("localraw: sync dir: %w", err)
	}
	return nil
}
