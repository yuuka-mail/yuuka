package api

import "time"

// The JSON shapes of the REST API. web/src/api/types.gen.ts is generated
// from this file by tygo (go generate ./internal/api); keep everything the
// WebUI reads here and nothing else.

// Class is the effective machine/human split of §3 principle 4.
type Class string

const (
	ClassUnknown Class = "unknown"
	ClassMachine Class = "machine"
	ClassHuman   Class = "human"
)

// MessageSummary is a message as lists show it.
type MessageSummary struct {
	ID         string    `json:"id"`
	ThreadID   string    `json:"thread_id"`
	ReceivedAt time.Time `json:"received_at"`
	Subject    string    `json:"subject"`
	FromAddr   string    `json:"from_addr"`
	FromName   string    `json:"from_name"`
	Snippet    string    `json:"snippet"`
	Class      Class     `json:"class"`
	// Downgraded: hidden from default views by the §4.3 minimal downgrade
	// (only discovered prefixes, or DMARC failed).
	Downgraded bool `json:"downgraded"`
	// Quarantined: the parser had a problem; see Message.parse_error.
	Quarantined bool `json:"quarantined"`
	HasAttach   bool `json:"has_attach"`
	// Recipients are prefix@zone of the deliveries; empty in search hits.
	Recipients []string `json:"recipients"`
}

// MessagePage is one page of GET /api/v1/messages.
type MessagePage struct {
	Messages []MessageSummary `json:"messages"`
	// NextCursor continues the list; absent on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// AuthResult is one method result of a trusted Authentication-Results.
type AuthResult struct {
	Method string            `json:"method"`
	Result string            `json:"result"`
	Props  map[string]string `json:"props,omitempty"`
}

// Auth is what the trusted receiver reported for a delivery (§10.1.1).
type Auth struct {
	// Trusted is false when no trusted Authentication-Results was found;
	// everything else is then empty except Reason.
	Trusted    bool         `json:"trusted"`
	Reason     string       `json:"reason,omitempty"`
	AuthservID string       `json:"authserv_id,omitempty"`
	DMARC      string       `json:"dmarc,omitempty"`
	Results    []AuthResult `json:"results,omitempty"`
	RemoteIP   string       `json:"remote_ip,omitempty"`
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
	Auth *Auth `json:"auth,omitempty"`
}

// Message is GET /api/v1/messages/{id}: parsed fields and the plain text
// body. Its id may differ from the requested one when that was merged
// into another message (§4.2).
type Message struct {
	MessageSummary `tstype:",extends"`
	MessageID      string     `json:"message_id"`
	InReplyTo      string     `json:"in_reply_to"`
	References     []string   `json:"references"`
	Date           *time.Time `json:"date,omitempty"` // absent if missing or unparseable
	BodyText       string     `json:"body_text"`
	ParseError     string     `json:"parse_error,omitempty"`
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
	Claimed    bool      `json:"claimed"`
	Label      string    `json:"label"`
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
	Messages   int       `json:"messages"`
	Downgraded int       `json:"downgraded"`
}

// PrefixList is GET /api/v1/prefixes. Messages and Downgraded are
// distinct totals: mail sent to two prefixes counts once here.
type PrefixList struct {
	Messages   int      `json:"messages"`
	Downgraded int      `json:"downgraded"`
	Prefixes   []Prefix `json:"prefixes"`
}

// SearchResult is GET /api/v1/search.
type SearchResult struct {
	Total int              `json:"total"`
	Hits  []MessageSummary `json:"hits"`
}

// ErrorBody is the body of every non-2xx response.
type ErrorBody struct {
	Error string `json:"error"`
}
