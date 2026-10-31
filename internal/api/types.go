package api

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// The request and response shapes of the REST API. openapi.json is
// derived from them and the route table (§5.1); the WebUI's TypeScript
// types are generated from that.
//
// Parameters are structs whose fields carry path:"name" or query:"name",
// the constraints of internal/openapi (minimum, maximum, pattern,
// default) and a jsonschema:"description". Enum types list their values
// in an Enum method. Each endpoint gets its own parameter type, so it
// accepts exactly the values its type admits.

// IDParams name one message or thread.
type IDParams struct {
	ID string `path:"id"`
}

// MessagesParams are the parameters of GET /api/v1/messages.
type MessagesParams struct {
	Prefix     *Mailbox          `query:"prefix" jsonschema:"only mail delivered to this prefix@zone, whatever its class"`
	Class      []agentview.Class `query:"class" jsonschema:"only mail of these classes"`
	Downgraded Downgraded        `query:"downgraded" default:"hide"`
	Label      string            `query:"label" jsonschema:"only mail carrying this label (exact, case-sensitive)"`
	Archived   *bool             `query:"archived" jsonschema:"only archived (true) or unarchived (false) mail; absent is both"`
	Triage     *TriageFilter     `query:"triage" pattern:"^[a-z][a-z0-9_]*:[a-z][a-z0-9_]*$" jsonschema:"only mail whose latest score gives this option of this question the highest probability, as <question>:<option> such as phishing:yes; no threshold, and a model that abstained matches nothing"`
	Cursor     *Cursor           `query:"cursor" jsonschema:"next_cursor of the previous page"`
	Limit      int               `query:"limit" minimum:"1" maximum:"500" default:"100"`
}

// Downgraded selects by the §4.3 minimal downgrade: hide downgraded mail
// (the default views), include it, or show only it (the review list).
type Downgraded string

func (Downgraded) Enum() []string { return []string{"hide", "include", "only"} }

// SearchDowngraded is Downgraded without only: search has no review list.
type SearchDowngraded string

func (SearchDowngraded) Enum() []string { return []string{"hide", "include"} }

// Mailbox is a prefix@zone parameter.
type Mailbox struct{ Prefix, Zone string }

func (m *Mailbox) UnmarshalText(b []byte) error {
	s := string(b)
	i := strings.LastIndexByte(s, '@')
	if i <= 0 || i == len(s)-1 {
		return errors.New("want prefix@zone")
	}
	m.Prefix, m.Zone = strings.ToLower(s[:i]), strings.ToLower(s[i+1:])
	return nil
}

// Cursor is a position in a list ordered by (time, id), newest first,
// that new rows join at the top: messages by arrival, drafts by their
// last change. It is opaque to clients: base64url of
// "<RFC 3339 nano> <id>".
type Cursor store.MessageCursor

func (c *Cursor) UnmarshalText(b []byte) error {
	raw, err := base64.RawURLEncoding.DecodeString(string(b))
	if err == nil {
		at, id, ok := strings.Cut(string(raw), " ")
		if t, terr := time.Parse(time.RFC3339Nano, at); ok && terr == nil && id != "" {
			*c = Cursor{ReceivedAt: t, ID: id}
			return nil
		}
	}
	return errors.New("malformed; pass next_cursor back as it came")
}

func (c Cursor) String() string {
	return base64.RawURLEncoding.EncodeToString([]byte(c.ReceivedAt.UTC().Format(time.RFC3339Nano) + " " + c.ID))
}

// ViewParams are the parameters of GET /api/v1/messages/{id}/view.
type ViewParams struct {
	ID     string           `path:"id"`
	Level  agentview.Level  `query:"level" default:"part"`
	Anchor agentview.Anchor `query:"anchor" jsonschema:"start at this anchor, such as part/1/para/3"`
}

// AttachmentParams are the parameters of an attachment download.
type AttachmentParams struct {
	ID string `path:"id"`
	// The section bounds keep the number short; mail never nests deeper.
	Part string `path:"part" pattern:"^[1-9][0-9]{0,3}(\\.[1-9][0-9]{0,3}){0,15}$" jsonschema:"the IMAP section of the attachment, as the view lists it"`
}

// PrefixesParams are the parameters of GET /api/v1/prefixes. Prefixes
// change slowly and only people page through them, so they page by
// offset; without a limit every matching prefix comes back.
type PrefixesParams struct {
	State  PrefixState `query:"state" jsonschema:"only claimed or only discovered prefixes"`
	Limit  int         `query:"limit" minimum:"1" maximum:"500"`
	Offset int         `query:"offset" minimum:"0"`
}

// PrefixState is how a prefix came to be known (§3.3).
type PrefixState string

func (PrefixState) Enum() []string { return []string{"claimed", "discovered"} }

// SearchParams are the parameters of GET /api/v1/search.
type SearchParams struct {
	Q          string           `query:"q" jsonschema:"words that must all occur in subject, sender or body; empty matches everything"`
	Downgraded SearchDowngraded `query:"downgraded" default:"hide"`
	Label      string           `query:"label" jsonschema:"only mail carrying this label (exact, case-sensitive)"`
	Archived   *bool            `query:"archived" jsonschema:"only archived (true) or unarchived (false) mail; absent is both"`
	Triage     *TriageFilter    `query:"triage" pattern:"^[a-z][a-z0-9_]*:[a-z][a-z0-9_]*$" jsonschema:"only mail whose latest score gives this option of this question the highest probability, as <question>:<option> such as phishing:yes; no threshold, and a model that abstained matches nothing"`
	Cursor     *Cursor          `query:"cursor" jsonschema:"next_cursor of the previous page"`
	Limit      int              `query:"limit" minimum:"1" maximum:"500" default:"50"`
}

// MessageSummary is a message as lists show it.
type MessageSummary struct {
	ID         string          `json:"id"`
	ThreadID   string          `json:"thread_id"`
	ReceivedAt time.Time       `json:"received_at"`
	Subject    string          `json:"subject"`
	FromAddr   string          `json:"from_addr"`
	FromName   string          `json:"from_name"`
	Snippet    string          `json:"snippet"`
	Class      agentview.Class `json:"class"`
	// Downgraded: hidden from default views by the §4.3 minimal downgrade
	// (only discovered prefixes, or DMARC failed).
	Downgraded bool `json:"downgraded"`
	// Quarantined: the parser had a problem; see Message.parse_error.
	Quarantined bool `json:"quarantined"`
	HasAttach   bool `json:"has_attach"`
	// Recipients are prefix@zone of the deliveries; empty in search hits.
	Recipients []string `json:"recipients"`
	// Outgoing is set for mail we sent: the latest outbox row that sent
	// it, and how far it got. That the message exists says nothing about
	// whether anyone received it.
	Outgoing *Outgoing `json:"outgoing,omitempty"`
	// Extracted is what the notification zone shows in place of the
	// subject (§12.9): the message's best code and link and how long
	// they last. Absent when nothing was found; in search hits, given
	// for machine mail only.
	Extracted *Extracted `json:"extracted,omitempty"`
	// SenderVerified: on a delivery, the trusted receiver's DMARC passed
	// for the organization of the From shown here, so that From is
	// genuine (§10.1.1). It is exactly what a rule's from.verified
	// tests. Absent for mail nobody received (only sent from here), in
	// search hits that are not machine mail, and without delivery:read:
	// it is an authentication fact.
	SenderVerified *bool `json:"sender_verified,omitempty"`
	// Labels and Archived are the message's annotations (§8.5); GET
	// /api/v1/messages/{id}/annotations says who set them. Archived mail
	// is out of the two zones but still listed and found.
	Labels   []string `json:"labels"`
	Archived bool     `json:"archived"`
	// Triage is what the latest score of the configured scorer leans to
	// (§15): by question, the option given the highest probability,
	// however slightly; a question the model abstained on is left out.
	// A model's observation, not yuuka's verdict, and nothing acts on it;
	// the message itself has the probabilities. Absent when the message
	// is not scored, and in prefix offenses and rule previews.
	Triage map[string]string `json:"triage,omitempty"`
}

// Extracted is the best of what yuuka's extractor found in a message.
// The values are the sender's: untrusted data (§8.3). A link is to be
// shown with its host and opened only by a person.
type Extracted struct {
	Code *agentview.Extraction `json:"code,omitempty"`
	Link *agentview.Extraction `json:"link,omitempty"`
	// ExpiresAt is when the code and link stop working, if the mail says.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Expired: ExpiresAt had passed when this response was made.
	Expired bool `json:"expired"`
}

// MessagePage is one page of GET /api/v1/messages, newest first.
type MessagePage struct {
	Messages []MessageSummary `json:"messages"`
	// NextCursor continues the list; absent on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// DeliveryAuthResult is one method result of a trusted
// Authentication-Results.
type DeliveryAuthResult struct {
	Method string            `json:"method"`
	Result string            `json:"result"`
	Props  map[string]string `json:"props,omitempty"`
}

// DeliveryAuth is what the trusted receiver reported for a delivery
// (§10.1.1).
type DeliveryAuth struct {
	// Trusted is false when no trusted Authentication-Results was found;
	// everything else is then empty except Reason.
	Trusted    bool                 `json:"trusted"`
	Reason     string               `json:"reason,omitempty"`
	AuthservID string               `json:"authserv_id,omitempty"`
	DMARC      string               `json:"dmarc,omitempty"`
	Results    []DeliveryAuthResult `json:"results,omitempty"`
	RemoteIP   string               `json:"remote_ip,omitempty"`
}

// Delivery is one arrival of a message at prefix@zone.
type Delivery struct {
	ID            string    `json:"id"`
	Zone          string    `json:"zone"`
	Prefix        string    `json:"prefix"`
	PrefixClaimed bool      `json:"prefix_claimed"`
	EnvelopeFrom  string    `json:"envelope_from"`
	EnvelopeTo    string    `json:"envelope_to"`
	ReceivedAt    time.Time `json:"received_at"`
	// Auth is absent for deliveries ingested before auth was extracted.
	Auth *DeliveryAuth `json:"auth,omitempty"`
	// Leak is the leak check of this delivery (§3 principle 3), judged
	// against the prefix's expected domains as they are now. Absent for
	// an unclaimed prefix, a prefix that expects nothing yet, or a DMARC
	// failure that does not claim an expected sender (one that does is a
	// spoof, and stays downgraded).
	Leak *agentview.LeakCheck `json:"leak,omitempty"`
}

// Message is GET /api/v1/messages/{id}: parsed fields and the plain text
// body. Its id may differ from the requested one when that was merged
// into another message (§4.2).
type Message struct {
	MessageSummary
	MessageID  string     `json:"message_id"`
	InReplyTo  string     `json:"in_reply_to"`
	References []string   `json:"references"`
	Date       *time.Time `json:"date,omitempty"` // absent if missing or unparseable
	BodyText   string     `json:"body_text"`
	ParseError string     `json:"parse_error,omitempty"`
	// Extractions are everything the extractor found, in its order:
	// codes, then links, best first, then the expiry (§3 principle 4).
	Extractions []agentview.Extraction `json:"extractions"`
	// To, Cc and ReplyTo are the header fields with display names, read
	// from the raw message the viewer may read when it is fetched. Like
	// everything the sender wrote they are unverified (§8.3).
	To      []agentview.Address `json:"to"`
	Cc      []agentview.Address `json:"cc"`
	ReplyTo []agentview.Address `json:"reply_to"`
	// Reply is whom a reply goes to by default, worked out here so that
	// no client judges authentication itself.
	Reply ReplyAddressing `json:"reply"`
	// Deliveries are envelope and authentication facts. They need
	// delivery:read and are absent without it.
	Deliveries []Delivery `json:"deliveries,omitempty"`
	// Notes are principals' notes on this message and on its thread
	// (§8.10), message notes first: every one for a person, its own for
	// anyone else. They are their authors' words, not yuuka's, and as
	// untrusted as the mail they were written about.
	Notes []Note `json:"notes"`
	// TriageScore is the latest score of the configured scorer (§15): a
	// decision model's observation, not yuuka's verdict, and nothing
	// acts on it. Absent when no scorer is configured or it has not
	// scored this message.
	TriageScore *TriageScore `json:"triage_score,omitempty"`
}

// ReplyAddressing is whom a reply to a message goes to by default
// (§12.10): bare addresses as the message wrote them, each once by
// store.FoldAddr.
type ReplyAddressing struct {
	// To is the Reply-To when it counts by the rule of the thread
	// correspondents (§8.9: a delivery escaping the downgrade passed
	// DMARC), else the From. For mail we sent, whom it went to.
	To []string `json:"to"`
	// Cc is what a reply to all adds: the message's To and Cc, without
	// To and without our own addresses (those it reached us at, the From
	// we sent it as).
	Cc []string `json:"cc"`
	// UnverifiedReplyTo is a Reply-To that does not count, where it
	// names someone other than the From. On a forged sender it is the
	// forger's own mailbox: a client shows it, so that a person can pick
	// it knowingly, and never uses it unasked.
	UnverifiedReplyTo []string `json:"unverified_reply_to"`
}

// Thread is GET /api/v1/threads/{id}, aggregated over what the viewer
// can see.
type Thread struct {
	ID           string           `json:"id"`
	Subject      string           `json:"subject"`
	Participants []string         `json:"participants"`
	FirstAt      time.Time        `json:"first_at"`
	LastAt       time.Time        `json:"last_at"`
	Messages     []MessageSummary `json:"messages"`
}

// Prefix is one row of GET /api/v1/prefixes, the aggregation view.
type Prefix struct {
	Zone    string `json:"zone"`
	Prefix  string `json:"prefix"`
	Address string `json:"address"` // prefix@zone, the list filter value
	// Claimed prefixes were handed out on purpose; discovered ones were
	// first seen in mail and everything to them is downgraded (§3.3).
	Claimed bool   `json:"claimed"`
	Label   string `json:"label"`
	// FirstSeen and LastSeen are absent for a prefix that was claimed
	// before any mail reached it.
	FirstSeen  *time.Time `json:"first_seen,omitempty"`
	LastSeen   *time.Time `json:"last_seen,omitempty"`
	Messages   int        `json:"messages"`
	Downgraded int        `json:"downgraded"`
	// ExpectedDomains are the organizational domains the address was
	// given to, the operator's decision (yuuka prefix expect).
	ExpectedDomains []string `json:"expected_domains"`
	// The leak check of a claimed prefix (§3 principle 3), over the mail
	// the viewer may read; empty and zero for an unclaimed one.
	// Observed are the senders the trusted receiver's DMARC proved, most
	// mail first. Leaks counts messages from senders the address was not
	// given to, Unverified those of them that proved nothing about their
	// sender; Spoofs counts messages whose From claims an expected domain
	// without DMARC proving it. Leaks and spoofs stay zero while nothing
	// is expected.
	Observed    []ObservedDomain `json:"observed"`
	Leaks       int              `json:"leaks"`
	Unverified  int              `json:"unverified"`
	Spoofs      int              `json:"spoofs"`
	LastOffense *time.Time       `json:"last_offense,omitempty"`
}

// ObservedDomain is an organizational domain DMARC proved to have sent
// mail to a prefix.
type ObservedDomain struct {
	Domain   string    `json:"domain"`
	Messages int       `json:"messages"`
	LastSeen time.Time `json:"last_seen"`
	// Expected is whether the prefix was given to it.
	Expected bool `json:"expected"`
}

// PrefixParams name one prefix.
type PrefixParams struct {
	Address Mailbox `path:"address" jsonschema:"prefix@zone"`
}

// PrefixDetail is GET /api/v1/prefixes/{address}: the prefix and the
// latest mail that leaked or spoofed, newest first.
type PrefixDetail struct {
	Prefix
	Offenses []Offense `json:"offenses"`
}

// Offense is one message judged a leak or a spoof at the prefix.
type Offense struct {
	Message MessageSummary        `json:"message"`
	Verdict agentview.LeakVerdict `json:"verdict"`
	// Domain is the DMARC-proven sender of a leak, the claimed one of a
	// spoof; absent for a leak that proved nothing about its sender.
	Domain      string    `json:"domain,omitempty"`
	DeliveredAt time.Time `json:"delivered_at"`
}

// PrefixList is GET /api/v1/prefixes. Messages and Downgraded are
// distinct totals over all visible mail, whatever the query: mail sent
// to two prefixes counts once here. Total is how many prefixes the query
// matched, before limit and offset.
type PrefixList struct {
	Messages   int      `json:"messages"`
	Downgraded int      `json:"downgraded"`
	Total      int      `json:"total"`
	Prefixes   []Prefix `json:"prefixes"`
}

// SearchResult is GET /api/v1/search: a page of hits, newest first, and
// how many match in all.
type SearchResult struct {
	Total int              `json:"total"`
	Hits  []MessageSummary `json:"hits"`
	// NextCursor continues the hits; absent on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}
