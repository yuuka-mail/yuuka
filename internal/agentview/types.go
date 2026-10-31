package agentview

import "time"

// The JSON shape of the view, shared by the MCP tools and the REST API
// (§5.1). Types with an Enum method list their values once, here; both
// contracts' schemas are derived from them by internal/openapi.

// Class is the effective machine/human split of §3 principle 4, as
// classify.Class names it.
type Class string

const (
	ClassUnknown Class = "unknown"
	ClassMachine Class = "machine"
	ClassHuman   Class = "human"
)

func (Class) Enum() []string { return []string{"unknown", "machine", "human"} }

// Level is how much of a message a view carries (§8.1).
type Level string

const (
	// LevelSnippet: facts, header fields, a short snippet, attachments.
	LevelSnippet Level = "snippet"
	// LevelSections adds the table of contents.
	LevelSections Level = "sections"
	// LevelPart adds the blocks, from the anchor on if there is one, up
	// to MaxChars.
	LevelPart Level = "part"
)

func (Level) Enum() []string { return []string{"snippet", "sections", "part"} }

// View is the structured view of one message.
type View struct {
	// URI is yuuka://message/<ulid>. Every anchor in the view is a
	// fragment of it.
	URI     string  `json:"uri"`
	Version int     `json:"view_version"`
	Level   Level   `json:"level"`
	Facts   Facts   `json:"facts"`
	Content Content `json:"content"`
}

// Facts are asserted by yuuka itself: they come from the store and from
// the trusted receiver, never from what the sender wrote.
type Facts struct {
	// ReceivedAt is when the message reached yuuka: its earliest delivery,
	// or for mail it only sent, when the sent copy was submitted (§4.6).
	ReceivedAt  time.Time `json:"received_at"`
	DeliveredTo []string  `json:"delivered_to"`
	Class       Class     `json:"class"`
	// Sent is set when this system sent the message (§4.6), whether or
	// not it also arrived here.
	Sent *Sent `json:"sent,omitempty"`
	Auth Auth  `json:"auth"`
	// Downgraded is true when the message fails the §4.3 minimal
	// downgrade (unclaimed prefix, or dmarc=fail from the trusted
	// receiver). Agents treat such mail as suspect.
	Downgraded bool `json:"downgraded"`
	// LeakChecks judge each delivery to a claimed prefix that expects
	// mail from known domains (§3 principle 3). Absent when nothing was
	// judged, and withheld with the other delivery facts.
	LeakChecks []LeakCheck `json:"leak_checks,omitempty"`
	// Warnings are short sentences for the reader, in a fixed vocabulary.
	Warnings []string `json:"warnings,omitempty"`
	Stripped Stripped `json:"stripped"`
}

// Sent marks mail this system sent: the sent copy of an outbox row.
type Sent struct {
	OutboxID string `json:"outbox_id"`
	// From is the address of the identity it was sent as.
	From string `json:"from"`
	// State is the outbox state (§4.5), read when the view was built:
	// whether it reached anyone is this, not that the message exists.
	State string `json:"state"`
}

// Auth is the authentication line of §8.1. Only the trusted receiver's
// Authentication-Results counts (§10.1.1); without it the message is
// unverified and nothing else is filled in.
type Auth struct {
	Verified bool `json:"verified"`
	// Withheld is set when the viewer lacks delivery:read; nothing but
	// FromDomain is filled in then.
	Withheld bool `json:"withheld,omitempty"`
	// NotReceived is set for mail this system sent and the viewer has no
	// delivery of: no receiver judged it, so there is no verdict, and
	// nothing but FromDomain is filled in.
	NotReceived bool   `json:"not_received,omitempty"`
	SPF         string `json:"spf,omitempty"`
	DKIM        string `json:"dkim,omitempty"`
	DMARC       string `json:"dmarc,omitempty"`
	// FromDomain is the domain of the From header. Whether it is genuine
	// is answered by DMARC alone (§8.1).
	FromDomain string `json:"from_domain,omitempty"`
	// Reason says why the message is unverified.
	Reason string `json:"reason,omitempty"`
	// Line is the one-line form, e.g.
	// "spf=pass dkim=pass dmarc=pass from_domain=example.com".
	Line string `json:"line"`
}

// LeakVerdict is the leak check of one delivery to a claimed prefix
// (§3 principle 3): ok when DMARC proved an expected sender, leak when
// the sender is not one the address was given to, spoof when the From
// claims an expected sender that DMARC did not prove.
type LeakVerdict string

const (
	VerdictOK    LeakVerdict = "ok"
	VerdictLeak  LeakVerdict = "leak"
	VerdictSpoof LeakVerdict = "spoof"
)

func (LeakVerdict) Enum() []string { return []string{"ok", "leak", "spoof"} }

// LeakCheck is the verdict on one delivery.
type LeakCheck struct {
	// To is the prefix@zone the delivery went to.
	To      string      `json:"to"`
	Verdict LeakVerdict `json:"verdict"`
	// Domain is an organizational domain: the DMARC-proven sender for ok
	// and leak, the claimed one for spoof. A leak without it proved
	// nothing about its sender.
	Domain string `json:"domain,omitempty"`
}

// Stripped counts what the view left out, so a reader knows it is there.
type Stripped struct {
	TrackingPixels int `json:"tracking_pixels,omitempty"`
	// Images without alt text; images with alt text appear as text.
	Images int `json:"images,omitempty"`
	// Hidden elements: preheaders, display:none and the like. Hidden text
	// is a favourite carrier of prompt injection.
	Hidden int `json:"hidden,omitempty"`
	// UnsafeLinks are links whose scheme is not http, https, mailto or
	// tel; their text is kept without the target.
	UnsafeLinks int `json:"unsafe_links,omitempty"`
	// QuotedLines are the lines of collapsed quoted replies.
	QuotedLines int `json:"quoted_lines,omitempty"`
}

// Content is everything the sender wrote: untrusted (§8.3).
type Content struct {
	Notice      string       `json:"notice"`
	From        Address      `json:"from"`
	Subject     string       `json:"subject"`
	Date        *time.Time   `json:"date,omitempty"`
	Snippet     string       `json:"snippet"`
	Attachments []Attachment `json:"attachments,omitempty"`
	Sections    []Section    `json:"sections,omitempty"`
	Parts       []Part       `json:"parts,omitempty"`
	// More is the anchor of the first block a LevelPart view left out
	// for MaxChars; empty when nothing was left out.
	More string `json:"more,omitempty"`
}

// Address is a mailbox as the sender wrote it.
type Address struct {
	Name string `json:"name,omitempty"`
	Addr string `json:"addr,omitempty"`
}

// Attachment is a part shown as a file. Its bytes are not in the view.
type Attachment struct {
	Section   string `json:"section"`
	Filename  string `json:"filename,omitempty"`
	MediaType string `json:"media_type"`
}

// Section is one entry of the table of contents: a heading and the blocks
// up to the next one, or the start of a part before its first heading.
type Section struct {
	Anchor string `json:"anchor"`
	Title  string `json:"title"`
	// Level is the heading level, 0 for the untitled start of a part.
	Level  int `json:"level,omitempty"`
	Blocks int `json:"blocks"`
	Chars  int `json:"chars"`
}

// Part is one readable body part (text/plain or text/html). Of the
// representations of a multipart/alternative only the richest renderable
// one is shown (RFC 2046 orders them by increasing faithfulness).
type Part struct {
	Anchor    string  `json:"anchor"` // part/<section>
	MediaType string  `json:"media_type"`
	Blocks    []Block `json:"blocks"`
}

// Kind is the kind of a block.
type Kind string

const (
	KindHeading Kind = "heading"
	KindPara    Kind = "para"
	KindItem    Kind = "item"
	KindPre     Kind = "pre"
	// KindQuote is a collapsed quoted reply: Spans hold the attribution
	// line if there is one ("On ... wrote:"), Lines how many lines were
	// collapsed. The quoted text itself is the earlier message.
	KindQuote Kind = "quote"
)

func (Kind) Enum() []string { return []string{"heading", "para", "item", "pre", "quote"} }

// Block is one addressable unit of text. Its anchor's para number counts
// every block of the part, whatever its kind.
type Block struct {
	Anchor string `json:"anchor"` // part/<section>/para/<n>
	Kind   Kind   `json:"kind"`
	// Level is the heading level, or the list nesting depth of an item.
	Level int    `json:"level,omitempty"`
	Spans []Span `json:"spans,omitempty"`
	Lines int    `json:"lines,omitempty"`
}

// Span is a run of text, a link when Href is set. Href is the real target
// as written in the mail, never the text shown for it.
type Span struct {
	Text string `json:"text"`
	Href string `json:"href,omitempty"`
}
