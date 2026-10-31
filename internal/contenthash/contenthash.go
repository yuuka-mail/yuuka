// Package contenthash computes raw_objects.content_hash, the fold and
// redelivery-dedupe key of §4.1.
//
// It deliberately does not parse MIME. The header block is split only at
// RFC 5322 field boundaries (a line that does not start with SP or HTAB
// starts a new field; folded continuation lines belong to the field above),
// transport fields are dropped by name, and every remaining byte -- kept
// fields, the blank separator line and the body -- is hashed verbatim. That
// keeps the key independent of any parser version, so M0 can dedupe without
// a parser and a reparse can never change the result.
//
// The algorithm is frozen as v1 and locked by golden tests. Changing the
// drop list or the splitting rules means a v2 that applies only to newly
// ingested raw; existing rows are never recomputed.
package contenthash

import (
	"bytes"
	"crypto/sha256"
	"strings"
)

// V1 returns the v1 content hash of a raw message.
func V1(raw []byte) [32]byte {
	h := sha256.New()
	rest := raw
	drop := false
	for len(rest) > 0 {
		var line []byte
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i+1], rest[i+1:]
		} else {
			line, rest = rest, nil
		}
		if string(line) == "\n" || string(line) == "\r\n" {
			// End of the header block: the separator and body are content.
			h.Write(line)
			h.Write(rest)
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			// Continuation: shares the fate of the field it folds into.
			if !drop {
				h.Write(line)
			}
			continue
		}
		drop = isTransport(line)
		if !drop {
			h.Write(line)
		}
	}
	var sum [32]byte
	h.Sum(sum[:0])
	return sum
}

// isTransport reports whether the field starting on line is one that each
// hop or each delivery attempt adds or rewrites. A line without a colon is
// not a well-formed field and is kept, so malformed input never shrinks.
func isTransport(line []byte) bool {
	name, _, ok := bytes.Cut(line, []byte(":"))
	if !ok {
		return false
	}
	// RFC 5322 obsolete syntax allows whitespace before the colon.
	n := strings.ToLower(strings.TrimRight(string(name), " \t"))
	switch n {
	case "received", "received-spf", "authentication-results", "dkim-signature", "return-path", "delivered-to":
		return true
	}
	return strings.HasPrefix(n, "arc-") || strings.HasPrefix(n, "x-")
}
