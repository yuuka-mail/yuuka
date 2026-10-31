package api

import (
	"time"

	"github.com/yuuka-mail/yuuka/internal/agentview"
)

// The shapes of sending (M2): drafts, their submission, the identities a
// viewer may send as, and the outbox. Like types.go, openapi.json is
// derived from them.

// Actor is a principal as lists show it (§13.2).
type Actor struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // human, agent, service or system
	Name string `json:"name"`
}

// Cite is a citation a draft carries (§8.1): a yuuka:// anchor and the
// quoted text, which is what the review of §8.9 reads.
type Cite struct {
	URI   string `json:"uri" jsonschema:"yuuka://message/<id>, optionally with #part/<section>/para/<n>"`
	Quote string `json:"quote,omitempty" jsonschema:"the quoted text, as it read when cited"`
}

// DraftBody is the content of a draft as written. Every field is
// replaced on update; an absent from keeps the draft's (update) or
// defaults to the address the replied-to message came in through
// (create, with in_reply_to).
type DraftBody struct {
	From         string   `json:"from,omitempty" jsonschema:"the address to send from; GET /api/v1/identities lists which may be used. Defaults to the reply identity of in_reply_to"`
	To           []string `json:"to,omitempty" jsonschema:"bare addresses"`
	Cc           []string `json:"cc,omitempty" jsonschema:"bare addresses"`
	Bcc          []string `json:"bcc,omitempty" jsonschema:"bare addresses"`
	Subject      string   `json:"subject,omitempty"`
	BodyMarkdown string   `json:"body_markdown,omitempty" jsonschema:"the body as Markdown; the identity's signature is added when it is sent"`
	InReplyTo    string   `json:"in_reply_to,omitempty" jsonschema:"the id of the message this replies to"`
	Cites        []Cite   `json:"cites,omitempty" jsonschema:"the passages the draft relies on, which the review checks; at most 32, each quote at most 1000 characters"`
}

// CreateDraftParams are the parameters of POST /api/v1/drafts.
type CreateDraftParams struct {
	Body DraftBody `body:"json"`
}

// UpdateDraftParams are the parameters of PUT /api/v1/drafts/{id}.
type UpdateDraftParams struct {
	ID   string    `path:"id"`
	Body DraftBody `body:"json"`
}

// DraftsParams are the parameters of GET /api/v1/drafts.
type DraftsParams struct {
	State  []DraftState `query:"state" jsonschema:"only drafts in these states; absent means draft,held"`
	Cursor *Cursor      `query:"cursor" jsonschema:"next_cursor of the previous page"`
	Limit  int          `query:"limit" minimum:"1" maximum:"500" default:"100"`
}

// DraftState is where a draft stands: being written, held by its last
// submission (hold_reason says why), or gone to the outbox.
type DraftState string

func (DraftState) Enum() []string { return []string{"draft", "held", "outbox"} }

// HoldReason is why a submission stopped at the draft (§8.9).
type HoldReason string

func (HoldReason) Enum() []string {
	return []string{"policy", "owner", "limit", "recipients", "suspended", "loop", "review", "withdrawn"}
}

// Draft is a draft as GET /api/v1/drafts/{id} gives it. Humans see every
// draft; anyone else only the drafts they wrote (§13.8).
type Draft struct {
	ID     string     `json:"id"`
	Author Actor      `json:"author"`
	State  DraftState `json:"state"`
	// IdentityID is the identity From goes out as, and IdentityOwner
	// owns it: when that is a person other than the viewer, only they
	// can confirm the draft (§8.9 owner cap), even where the viewer's
	// own tier on the identity is none and /identities leaves it out.
	IdentityID    string   `json:"identity_id"`
	IdentityOwner Actor    `json:"identity_owner"`
	From          string   `json:"from"`
	To            []string `json:"to"`
	Cc            []string `json:"cc"`
	Bcc           []string `json:"bcc"`
	Subject       string   `json:"subject"`
	BodyMarkdown  string   `json:"body_markdown"`
	// SMTP: a submission of the SMTP port held as a draft. Its body is
	// the MIME the service sent, not body_markdown; it is confirmed or
	// deleted, never edited.
	SMTP      bool   `json:"smtp"`
	InReplyTo string `json:"in_reply_to,omitempty"`
	Cites     []Cite `json:"cites"`
	// HoldReason and HoldDetail say why the last submission stopped here;
	// present in state held only.
	HoldReason HoldReason `json:"hold_reason,omitempty"`
	HoldDetail string     `json:"hold_detail,omitempty"`
	// Review is the verdict of the review that escalated it (hold_reason
	// review); absent otherwise.
	Review *Review `json:"review,omitempty"`
	// Outbox is the message it became, in state outbox.
	Outbox    *Outgoing `json:"outbox,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DraftPage is one page of GET /api/v1/drafts, most recently changed
// first. Editing a draft moves it to the top.
type DraftPage struct {
	Drafts []Draft `json:"drafts"`
	// NextCursor continues the list; absent on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// SubmitState is where a submission went: queued to be sent; held in
// the outbox after passing the review, sent at hold_until unless
// withdrawn first (§8.9); or stopped at its draft for a person, with
// hold_reason.
type SubmitState string

func (SubmitState) Enum() []string { return []string{"queued", "held", "draft"} }

// SubmitResult is where a submission went. Being held is not an error
// (§8.9): queued is false, and hold_reason and hold_detail say why.
type SubmitResult struct {
	State SubmitState `json:"state"`
	// Queued: state queued. Mail held for a recall is not queued yet.
	Queued  bool   `json:"queued"`
	DraftID string `json:"draft_id"`
	// OutboxID is the message in the outbox, in states queued and held.
	OutboxID string `json:"outbox_id,omitempty"`
	// HoldUntil ends the recall window, in state held: the message goes
	// out then unless withdrawn (POST /api/v1/outbox/{id}/withdraw).
	HoldUntil  *time.Time `json:"hold_until,omitempty"`
	HoldReason HoldReason `json:"hold_reason,omitempty"`
	HoldDetail string     `json:"hold_detail,omitempty"`
}

// CheckName is one check of the review tier (§8.9).
type CheckName string

func (CheckName) Enum() []string {
	return []string{"first_contact", "extracted_values", "cross_thread", "link_domains", "readable"}
}

// Review is the verdict of the review tier's checks on a submission
// (§8.9), every check in a fixed order. A check that did not pass
// escalated the submission to a person; when all passed, it was held
// for a recall.
type Review struct {
	Checks []ReviewCheck `json:"checks"`
}

// ReviewCheck is the outcome of one check. A check that did not pass
// says what tripped it in the one list that fits it.
type ReviewCheck struct {
	Name   CheckName `json:"name"`
	Pass   bool      `json:"pass"`
	Detail string    `json:"detail"`
	// Recipients never written to before (first_contact).
	Recipients []string `json:"recipients,omitempty"`
	// Extracted are values of incoming mail found in the outgoing
	// message (extracted_values).
	Extracted []ReviewExtracted `json:"extracted,omitempty"`
	// Refs are cited or attached messages it could not vouch for
	// (cross_thread).
	Refs []ReviewRef `json:"refs,omitempty"`
	// Links are linked domains never seen (link_domains).
	Links []ReviewLink `json:"links,omitempty"`
	// Unreadable are the parts the checks cannot read (readable).
	Unreadable []string `json:"unreadable,omitempty"`
}

// ReviewExtracted is a code or sign-in link of incoming mail found in
// the outgoing message. It says where, never what: the value is the
// secret, and the verdict does not hold it.
type ReviewExtracted struct {
	Kind agentview.ExtractionKind `json:"kind"`
	// MessageID is the incoming message it was extracted from, Source
	// where in that message.
	MessageID string                     `json:"message_id"`
	Source    agentview.ExtractionSource `json:"source"`
	// Where is where in the outgoing message: subject, body, an
	// attachment ...
	Where string `json:"where"`
}

// ReviewRef is a message the outgoing mail cited or carried.
type ReviewRef struct {
	// As is how it was referred to: a yuuka:// URI or a Message-ID.
	As    string `json:"as"`
	Where string `json:"where"`
	// MessageID is the message it resolved to; absent when no message
	// the submitter can see answers to it.
	MessageID string `json:"message_id,omitempty"`
}

// ReviewLink is a domain the outgoing mail links to.
type ReviewLink struct {
	// Host as linked: lowercased, in ASCII, without a final dot.
	Host  string `json:"host"`
	Where string `json:"where"`
	// Domain is its organizational domain; absent for an IP literal or
	// a name that is not one.
	Domain string `json:"domain,omitempty"`
}

// Tier is a grant's send_policy (§8.9).
type Tier string

func (Tier) Enum() []string { return []string{"none", "draft", "review", "auto"} }

// RecipientScope is a grant's recipient_scope (§13.5).
type RecipientScope string

func (RecipientScope) Enum() []string { return []string{"thread", "allowlist", "any"} }

// Limits are a grant's rate limits; an absent one is unlimited.
type Limits struct {
	MaxPerHour             *int `json:"max_per_hour,omitempty"`
	MaxPerDay              *int `json:"max_per_day,omitempty"`
	MaxNewRecipientsPerDay *int `json:"max_new_recipients_per_day,omitempty"`
}

// SendingIdentity is an identity and where the viewer's submissions as
// it stand before any particular message: what a composer shows next to
// From. The decision on a submission is still made when it is submitted,
// with its recipients, the counters and the thread.
type SendingIdentity struct {
	ID string `json:"id"`
	// Address is local@zone, or *@zone for a pattern that sends as any
	// local part of its zone.
	Address     string `json:"address"`
	DisplayName string `json:"display_name"`
	Owner       Actor  `json:"owner"`
	// Tier is the effective tier for the viewer: the grant's, or the
	// default without one (granted false), lowered to draft when the
	// identity belongs to another human (owner_capped).
	Tier           Tier           `json:"tier"`
	Granted        bool           `json:"granted"`
	OwnerCapped    bool           `json:"owner_capped"`
	RecipientScope RecipientScope `json:"recipient_scope"`
	Allowlist      []string       `json:"allowlist"`
	Limits         Limits         `json:"limits"`
	Suspended      bool           `json:"suspended"`
	// Sends: a submission goes out without a person, if its recipients
	// are within recipient_scope and the limits allow. Otherwise every
	// submission is held as a draft, and hold_reason says why.
	Sends      bool       `json:"sends"`
	HoldReason HoldReason `json:"hold_reason,omitempty"`
	HoldDetail string     `json:"hold_detail,omitempty"`
	// HoldSeconds is the recall window under review (hold_reason
	// review): mail the checks pass waits this long before it is sent,
	// 0 meaning only until the next release pass. Absent otherwise.
	HoldSeconds *int `json:"hold_seconds,omitempty"`
}

// IdentityList is GET /api/v1/identities: the identities the viewer may
// use (tier above none), by address.
type IdentityList struct {
	Identities []SendingIdentity `json:"identities"`
}

// ReplyIdentity is one identity a reply could go out as.
type ReplyIdentity struct {
	// From is the concrete address: the one the message came in through,
	// for a *@zone identity.
	From     string          `json:"from"`
	Identity SendingIdentity `json:"identity"`
}

// ReplyIdentities is GET /api/v1/messages/{id}/reply-identities: the
// addresses a reply defaults to, best first (§9.3). The first is what a
// draft replying without from gets; it may be empty.
type ReplyIdentities struct {
	Candidates []ReplyIdentity `json:"candidates"`
}

// Outgoing marks mail we sent, or are sending (§4.6): its outbox row.
type Outgoing struct {
	OutboxID string      `json:"outbox_id"`
	State    OutboxState `json:"state"`
}

// OutboxState is outbox.state (§4.5).
type OutboxState string

func (OutboxState) Enum() []string {
	return []string{"held", "queued", "sending", "sent", "partial", "unknown", "failed", "dead", "withdrawn"}
}

// RecipientKind is how a recipient was addressed.
type RecipientKind string

func (RecipientKind) Enum() []string { return []string{"to", "cc", "bcc"} }

// RecipientState is what is known of one recipient (§4.5).
type RecipientState string

func (RecipientState) Enum() []string {
	return []string{"pending", "accepted", "rejected", "deferred", "unknown"}
}

// AttemptOutcome is how a send attempt ended.
type AttemptOutcome string

func (AttemptOutcome) Enum() []string {
	return []string{"accepted", "partial", "rejected", "failed", "unknown", "abandoned"}
}

// OutboxRecipient is one envelope recipient and its state.
type OutboxRecipient struct {
	Addr  string         `json:"addr"`
	Kind  RecipientKind  `json:"kind"`
	State RecipientState `json:"state"`
}

// OutboxEntry is one outbox row: a message on its way, or its fate.
type OutboxEntry struct {
	ID    string      `json:"id"`
	State OutboxState `json:"state"`
	// Identity is the identity's address, *@zone for a pattern.
	Identity   string `json:"identity"`
	IdentityID string `json:"identity_id"`
	From       string `json:"from"`
	// DraftID is the draft it came from; absent for an SMTP submission
	// sent directly. MessageID is its sent copy, absent until indexed.
	DraftID   string `json:"draft_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	// Subject is the sent copy's subject; absent until it is indexed,
	// or when it has none.
	Subject     string `json:"subject,omitempty"`
	SubmittedBy Actor  `json:"submitted_by"`
	// ApprovedBy let it go: a person, or system for a policy (§8.8).
	// Absent while held.
	ApprovedBy *Actor `json:"approved_by,omitempty"`
	// HoldUntil ends the recall window of mail the review passed: it is
	// released to queued then, unless withdrawn first. Review is that
	// review's verdict. Both are absent for mail that was not reviewed.
	HoldUntil *time.Time `json:"hold_until,omitempty"`
	Review    *Review    `json:"review,omitempty"`
	// WithdrawnBy recalled it during its window: a person, its
	// submitter, or system when its grant was suspended.
	WithdrawnBy *Actor     `json:"withdrawn_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	NextRetryAt *time.Time `json:"next_retry_at,omitempty"`
	// Error is the last failure, as the exit put it.
	Error       string            `json:"error,omitempty"`
	Recipients  []OutboxRecipient `json:"recipients"`
	Attempts    int               `json:"attempts"`
	LastOutcome AttemptOutcome    `json:"last_outcome,omitempty"`
}

// OutboxParams are the parameters of GET /api/v1/outbox.
type OutboxParams struct {
	State  []OutboxState `query:"state" jsonschema:"only rows in these states"`
	Cursor string        `query:"cursor" pattern:"^[0-7][0-9A-HJKMNP-TV-Z]{25}$" jsonschema:"next_cursor of the previous page"`
	Limit  int           `query:"limit" minimum:"1" maximum:"500" default:"50"`
}

// OutboxPage is one page of GET /api/v1/outbox, newest first.
type OutboxPage struct {
	Entries []OutboxEntry `json:"entries"`
	// NextCursor continues the list; absent on the last page.
	NextCursor string `json:"next_cursor,omitempty"`
}

// Resolution is the human decision on mail whose delivery is unknown
// (§4.5): it was delivered, send it again (someone may get it twice), or
// give up.
type Resolution string

func (Resolution) Enum() []string { return []string{"sent", "resend", "abandon"} }

// ResolveBody is the body of POST /api/v1/outbox/{id}/resolve.
type ResolveBody struct {
	Resolution Resolution `json:"resolution"`
}

// ResolveParams are the parameters of POST /api/v1/outbox/{id}/resolve.
type ResolveParams struct {
	ID   string      `path:"id"`
	Body ResolveBody `body:"json"`
}

// Withdrawal is what POST /api/v1/outbox/{id}/withdraw did: the held
// message is withdrawn and will not be sent, and its draft is editable
// again, with hold_reason withdrawn.
type Withdrawal struct {
	OutboxID string `json:"outbox_id"`
	DraftID  string `json:"draft_id"`
	// Suspended is set when this recall tripped the circuit breaker of
	// the submitter's grant (§8.9).
	Suspended *Suspension `json:"suspended,omitempty"`
}

// Suspension is a grant the circuit breaker suspended: everything its
// principal submits as the identity stays a draft until a person
// resumes the grant.
type Suspension struct {
	IdentityID  string `json:"identity_id"`
	PrincipalID string `json:"principal_id"`
	// Recalls by people of the grant's mail submitted in the last 24
	// hours, this one included.
	Recalls int `json:"recalls"`
	// Withdrawn are the grant's other held messages, withdrawn with it.
	Withdrawn []string `json:"withdrawn"`
}
