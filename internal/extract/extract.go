// Package extract finds the values a notification mail exists to
// deliver (§3 principle 4): one-time codes, sign-in and verification
// links, and how long they last. The notification zone shows them at a
// glance (§12.9), and the exfiltration check of the review tier (§8.9)
// looks for them in outgoing mail.
//
// It is a pure function of the parsed message and the time the message
// reached yuuka, so the extractions table is derived and reparse rebuilds
// it (§4.2). Everything it returns was written by the sender: the values
// are data, never instructions, and links are never opened or fetched.
//
// The matcher is built for precision. A value is taken only near words
// that say what it is, in the languages yuuka's mail comes in; a number
// next to "order", a currency or a unit is not a code, and a link is a
// sign-in link only when its words or its path say so. Missing a code
// costs a glance at the subject; showing an order number as a code
// teaches the reader to distrust the zone.
package extract

import (
	"slices"
	"time"
	"unicode/utf8"

	"github.com/yuuka-mail/yuuka/internal/mimeparse"
)

// Version is messages.extract_version. Bump it whenever the output for
// the same input changes: the backlog pass re-extracts every message
// with an older version (ingest.ProcessBacklog), so a better matcher
// reaches old mail without a reparse.
const Version = 1

// Kind is extractions.kind. The list is closed by the table's CHECK.
type Kind string

const (
	// OTP is a one-time code, as the reader types it: digits without
	// the spaces or hyphens they were grouped with.
	OTP Kind = "otp"
	// Link is a URL that signs in, verifies, confirms or resets.
	Link Kind = "link"
	// Expiry is how long the codes and links of the message last: Value
	// is an ISO 8601 duration such as PT10M, Expires the end of it.
	Expiry Kind = "expiry"
	// Amount is reserved for sums of money; nothing extracts it yet.
	Amount Kind = "amount"
)

// Source is where in the message a value was found (extractions.source).
type Source string

const (
	Subject Source = "subject"
	Body    Source = "body"
	// HTML is the target of an <a href>, which body text does not hold.
	HTML Source = "html"
)

// Extraction is one value found in a message.
type Extraction struct {
	Kind  Kind
	Value string
	// Confidence is 1..100: how sure the matcher is that Value is what
	// Kind says. Nothing under minConfidence is returned.
	Confidence int
	Source     Source
	// Expires is set for Expiry only.
	Expires time.Time
}

// Input is what the matcher reads.
type Input struct {
	Subject string
	Body    string
	Links   []mimeparse.Link
	// Date is the Date header, zero if missing. The sender wrote it.
	Date time.Time
	// ReceivedAt is when the message reached yuuka (messages.received_at).
	ReceivedAt time.Time
}

// Bounds on what one message yields and costs.
const (
	minConfidence = 60
	maxOTPs       = 3
	maxLinks      = 3
	// maxScan bounds the body text read. Codes sit at the top of short
	// mail; the rest of a long one is a newsletter.
	maxScan = 64 << 10
)

// Message extracts from a parsed message that reached yuuka at receivedAt.
func Message(m mimeparse.Message, receivedAt time.Time) []Extraction {
	return Of(Input{Subject: m.Subject, Body: m.BodyText, Links: m.Links, Date: m.Date, ReceivedAt: receivedAt})
}

// Of returns the extractions of in, in the order they are stored: codes,
// then links, best first, then the expiry if there is a code or a link
// for it to apply to.
func Of(in Input) []Extraction {
	subject, body := clip(in.Subject, 1024), clip(in.Body, maxScan)
	texts := []text{{Subject, subject}, {Body, body}}
	out := best(otps(texts), maxOTPs)
	out = append(out, best(links(subject, body, in.Links), maxLinks)...)
	if len(out) > 0 {
		if e, ok := expiry(texts, in.Date, in.ReceivedAt); ok {
			out = append(out, e)
		}
	}
	return out
}

// clip cuts s to at most n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

type text struct {
	src Source
	s   string
}

// best keeps the most confident extraction of each value, best first (a
// tie keeps the order found), at most n.
func best(xs []Extraction, n int) []Extraction {
	var out []Extraction
	for _, x := range xs {
		if x.Confidence < minConfidence {
			continue
		}
		if i := slices.IndexFunc(out, func(o Extraction) bool { return o.Value == x.Value }); i >= 0 {
			if x.Confidence > out[i].Confidence {
				out[i] = x
			}
			continue
		}
		out = append(out, x)
	}
	slices.SortStableFunc(out, func(a, b Extraction) int { return b.Confidence - a.Confidence })
	if len(out) > n {
		out = out[:n]
	}
	return out
}
