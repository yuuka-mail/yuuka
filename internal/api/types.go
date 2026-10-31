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

// Cursor is a position in a list that new mail joins at the top. It is
// opaque to clients: base64url of "<RFC 3339 nano> <id>".
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
	// Deliveries are envelope and authentication facts. They need
	// delivery:read and are absent without it.
	Deliveries []Delivery `json:"deliveries,omitempty"`
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
