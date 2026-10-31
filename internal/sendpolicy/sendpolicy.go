// Package sendpolicy decides where one outbound submission goes (§8.9,
// §13.5): into the outbox, or back to a draft with a reason. It is a pure
// function of its input: no clock, no database. The store gathers the
// facts (grant, counters, thread correspondents) and calls Decide itself,
// inside the transaction that holds the grant's lock
// (store.WithDecision); an outbox row can only be created through the
// store.PolicyTx that carries that decision, and only when it is Queue
// (in queued, or in held when Held: the review tier, review.go).
// internal/submit is the one caller, for every submission path.
//
// Exceeding a grant is a downgrade, never a refusal: every outcome other
// than Queue is a draft with a hold reason, so the caller never has a
// reason to retry another way. The one exception is the SMTP port, which
// rejects mail when Decision.Tier is None (§13.6): a principal with no
// grant on the From address has nothing to leave as a draft there.
package sendpolicy

import (
	"fmt"
	"slices"
	"strings"
)

// Tier is identity_grants.send_policy.
type Tier string

const (
	None   Tier = "none"
	Draft  Tier = "draft"
	Review Tier = "review"
	Auto   Tier = "auto"
)

func (t Tier) rank() int {
	switch t {
	case Draft:
		return 1
	case Review:
		return 2
	case Auto:
		return 3
	}
	return 0
}

// ParseTier validates a stored or typed tier.
func ParseTier(s string) (Tier, error) {
	switch t := Tier(s); t {
	case None, Draft, Review, Auto:
		return t, nil
	}
	return "", fmt.Errorf("send policy %q: want none, draft, review or auto", s)
}

// RecipientScope is identity_grants.recipient_scope.
type RecipientScope string

const (
	// ScopeThread: replies only, to people already in the thread.
	ScopeThread RecipientScope = "thread"
	// ScopeAllowlist: every recipient is in the grant's allowlist.
	ScopeAllowlist RecipientScope = "allowlist"
	// ScopeAny: no restriction.
	ScopeAny RecipientScope = "any"
)

// ParseRecipientScope validates a stored or typed recipient scope.
func ParseRecipientScope(s string) (RecipientScope, error) {
	switch r := RecipientScope(s); r {
	case ScopeThread, ScopeAllowlist, ScopeAny:
		return r, nil
	}
	return "", fmt.Errorf("recipient scope %q: want thread, allowlist or any", s)
}

// HoldReason is drafts.hold_reason. The list is closed by the column's
// CHECK; Decide writes all but withdrawn, which a recall from held
// writes (store.WithdrawOutbox, store.PromoteHeld).
type HoldReason string

const (
	HoldPolicy     HoldReason = "policy"
	HoldOwner      HoldReason = "owner"
	HoldLimit      HoldReason = "limit"
	HoldRecipients HoldReason = "recipients"
	HoldSuspended  HoldReason = "suspended"
	HoldLoop       HoldReason = "loop"
	HoldReview     HoldReason = "review"
	HoldWithdrawn  HoldReason = "withdrawn"
)

// Principal kinds (principals.kind).
const (
	KindHuman   = "human"
	KindAgent   = "agent"
	KindService = "service"
)

// SystemPrincipalID is the system principal, the approver of mail a
// policy let go (§8.8). Same value as store.SystemPrincipalID.
const SystemPrincipalID = "00000000000000000000000000"

// AutoSubmittedValue is what automatic mail carries in Auto-Submitted
// (§8.9, RFC 3834).
const AutoSubmittedValue = "auto-generated"

// Limits are a grant's rate limits; nil means no limit.
type Limits struct {
	MaxPerHour             *int
	MaxPerDay              *int
	MaxNewRecipientsPerDay *int
}

func ptr(n int) *int { return &n }

// DefaultLimits are what a grant gets where its columns are NULL (or
// where there is no grant row), by the kind of the submitting principal.
// The database leaves them to this code (0003, §13.5), so changing them
// needs no migration.
//
// They are deliberately low. A limit that trips turns mail into a draft
// a human looks at; one that is too high lets a runaway or injected agent
// send a day's worth of mail before anyone notices. An operator who needs
// more writes the number into the grant (`yuuka grant set`).
//
//   - human: none. A person pressing send is the confirmation; the
//     limits exist to bound automation.
//   - service: 30 an hour, 200 a day, 5 new recipients a day. Services
//     mostly mail the same few addresses (alerts, reports); an alert
//     storm beyond 30 an hour is better read as drafts than as mail.
//   - agent and anything else: 10 an hour, 50 a day, 5 new recipients a
//     day. An agent answers threads; more than that in an hour is a loop
//     or an injection far more often than real work.
func DefaultLimits(kind string) Limits {
	switch kind {
	case KindHuman:
		return Limits{}
	case KindService:
		return Limits{MaxPerHour: ptr(30), MaxPerDay: ptr(200), MaxNewRecipientsPerDay: ptr(5)}
	}
	return Limits{MaxPerHour: ptr(10), MaxPerDay: ptr(50), MaxNewRecipientsPerDay: ptr(5)}
}

// DefaultHoldSeconds is the recall window of the review tier where a
// grant's hold_seconds is NULL (or there is no grant row): how long mail
// that passed the review waits in held before it is sent (§8.9). Ten
// minutes is long enough to read the card on a phone and press recall,
// short enough that a reply still reads as a reply. Like the limits it
// lives here, so changing it needs no migration.
const DefaultHoldSeconds = 600

// EffectiveHoldSeconds is the recall window a grant's hold_seconds
// stands for: nil (NULL, no number of its own) is DefaultHoldSeconds,
// anything else itself, 0 meaning no window. Effective applies it; it is
// exported for whoever compares a stored grant with what applies.
func EffectiveHoldSeconds(stored *int) int {
	if stored == nil {
		return DefaultHoldSeconds
	}
	return *stored
}

// MaxAutoPerThreadRecipient bounds automatic mail (approved by system)
// to one recipient within one thread over the last 24 hours, whoever
// sent it (§8.9 邮件循环). Two auto-responders that slipped past the
// header rules still stop after this many rounds a day.
const MaxAutoPerThreadRecipient = 3

// Principal is the submitter or the confirmer.
type Principal struct {
	ID   string
	Kind string
}

// Identity is the identity sent as.
type Identity struct {
	ID        string
	OwnerID   string
	OwnerKind string
	// Disabled also covers a draft's identity that no longer is what its
	// From address resolves to (a new exact identity took the address).
	Disabled bool
}

// Grant is the identity_grants row of (identity, submitter).
type Grant struct {
	Policy         Tier
	RecipientScope RecipientScope
	// Allowlist holds addresses (with @) and domains (without), matched
	// case-insensitively: an address exactly, a domain exactly (no
	// subdomains). Its domains are also the link domains the review tier
	// accepts (review.go).
	Allowlist []string
	Limits    Limits
	// HoldSeconds is the recall window of the review tier; nil means
	// DefaultHoldSeconds, 0 no window.
	HoldSeconds *int
	Suspended   bool
}

// Counters are what the grant has done recently. Windows are rolling:
// "hour" is the last 60 minutes, "day" the last 24 hours, so no time
// zone decides when a day starts.
type Counters struct {
	// Outbox rows of (identity, submitter) created in the window.
	SentLastHour int
	SentLastDay  int
	// Distinct addresses this grant contacted first in the last day.
	NewRecipientsLastDay int
	// FirstContact are the recipients of this submission (folded) that
	// no outbox row has ever gone to.
	FirstContact []string
	// AutoInThread counts, per folded recipient, the automatic outbox
	// rows (approved by system, or held for review from a submitter who
	// is not a human) in the replied-to thread over the last day. Nil
	// when not a reply.
	AutoInThread map[string]int
}

// Reply describes the message replied to.
type Reply struct {
	// ThreadID is its thread; the review tier compares the threads of
	// what the mail cites with it.
	ThreadID string
	// Correspondents are the folded addresses recipient_scope thread
	// admits: who wrote into the thread, whom its mail was addressed to,
	// as far as its headers can be trusted, and whom we sent to in it
	// (store.ReplyFacts says which headers count).
	Correspondents []string
	// The RFC 3834 signals of the message replied to, when known from its
	// headers, and whether it is classified machine (internal/classify
	// already counts Auto-Submitted and Precedence among its signals, so
	// Machine covers them when the headers are not at hand).
	AutoSubmitted string
	Precedence    string
	Machine       bool
}

// Input is everything Decide looks at.
type Input struct {
	// Submitter started the submission; its grant applies and its
	// counters count (outbox.submitted_by).
	Submitter Principal
	// CanSubmit: the acting credential holds submission:send (the
	// confirmer's when ConfirmedBy is set). Without it the submission
	// stays a draft, not an error (§13.4, §8.9).
	CanSubmit bool
	// ConfirmedBy is set when a human presses send on a held draft
	// someone else (or they) wrote. Their decision replaces the grant's
	// tier, limits and checks, but not the owner cap.
	ConfirmedBy *Principal
	Identity    Identity
	// Grant is nil when (identity, submitter) has no row.
	Grant *Grant
	// Recipients are the envelope recipients of all roles, any spelling.
	Recipients []string
	// Reply is nil for a new thread.
	Reply    *Reply
	Counters Counters
	// Review is what the review tier's checks read (review.go). The
	// store gathers it only where they run: tier review, no human
	// confirming. Nil there means it could not be gathered, and the
	// submission stays a draft.
	Review *ReviewFacts
}

// Decision is where the submission goes.
type Decision struct {
	// Queue: create the outbox row, in queued, or in held when Held.
	// Otherwise it is a draft with HoldReason and Detail.
	Queue bool
	// Held: the review passed, and the row waits HoldSeconds in held
	// for a recall before it is released to queued (store.PromoteHeld).
	// Only Decide sets it (TestOneWayToTheOutbox).
	Held        bool
	HoldSeconds int
	// Review is the verdict of the review tier's checks: set when they
	// ran, whether they passed (Held) or escalated (HoldReview). It is
	// stored with the outcome, outbox.review or drafts.review.
	Review *Verdict
	// ApprovedBy is outbox.approved_by when Queue and not Held: the
	// human who pressed send, or SystemPrincipalID when a policy let it
	// go. A held row has none until it is released (ReleasedBy).
	ApprovedBy string
	HoldReason HoldReason
	// Detail says the rest for people (drafts.hold_detail).
	Detail string
	// Tier is the effective tier: the grant's or the default, lowered by
	// the owner cap. The SMTP port rejects the message when it is None.
	Tier Tier
	// AutoSubmitted is the Auto-Submitted header value the composed or
	// normalized message must carry, "" for none. Set when a policy, not
	// a person, lets automatic mail go.
	AutoSubmitted string
}

// Fold is the case folding of addresses for matching: store.FoldAddr,
// which writes outbox_recipients.addr_fold.
func Fold(addr string) string { return strings.ToLower(addr) }

// Effective returns the grant that applies to in: the row with NULL
// limits filled from DefaultLimits, or the default grant when there is
// no row.
//
// Defaults without a row (§13.5):
//   - the identity's owner, human: auto, any recipient. A person pressing
//     send on their own address is the confirmation.
//   - the owner, agent or service: review, thread (§13.5's default for an
//     agent's own identity).
//   - an agent on a human-owned identity: draft. Writing drafts in the
//     operator's name is what agents are for (§8.2); sending is not.
//   - anyone else: none.
func Effective(in Input) Grant {
	lim := DefaultLimits(in.Submitter.Kind)
	if in.Grant != nil {
		g := *in.Grant
		if g.Limits.MaxPerHour == nil {
			g.Limits.MaxPerHour = lim.MaxPerHour
		}
		if g.Limits.MaxPerDay == nil {
			g.Limits.MaxPerDay = lim.MaxPerDay
		}
		if g.Limits.MaxNewRecipientsPerDay == nil {
			g.Limits.MaxNewRecipientsPerDay = lim.MaxNewRecipientsPerDay
		}
		g.HoldSeconds = ptr(EffectiveHoldSeconds(g.HoldSeconds))
		return g
	}
	g := Grant{Policy: None, RecipientScope: ScopeThread, Limits: lim, HoldSeconds: ptr(DefaultHoldSeconds)}
	switch {
	case in.Submitter.ID == in.Identity.OwnerID && in.Submitter.Kind == KindHuman:
		g.Policy, g.RecipientScope = Auto, ScopeAny
	case in.Submitter.ID == in.Identity.OwnerID:
		g.Policy = Review
	case in.Submitter.Kind == KindAgent && in.Identity.OwnerKind == KindHuman:
		g.Policy = Draft
	}
	return g
}

// OwnerCapped reports whether the hard cap of §8.9 applies: the identity
// belongs to a human and someone else submits. Then the tier is draft at
// most, whatever the grant says.
func OwnerCapped(submitterID string, id Identity) bool {
	return id.OwnerKind == KindHuman && submitterID != id.OwnerID
}

// Decide applies §8.9 in this order; the first rule that stops the
// submission names the hold reason.
//
//  1. identity disabled                       → draft (policy)
//  2. a human's confirmation of a held draft: the confirmer must be a
//     human, and the owner if the identity is human-owned (owner);
//     otherwise queue, approved by the confirmer. Steps 3–11 are the
//     grant's, and a person's decision replaces them.
//  3. owner cap: human-owned, submitter ≠ owner → draft (owner)
//  4. credential lacks submission:send         → draft (policy)
//  5. tier none or draft                       → draft (policy)
//  6. grant suspended (circuit breaker)        → draft (suspended)
//  7. no recipients, or one outside the scope  → draft (recipients)
//  8. automatic reply to automatic mail, or the per-thread cap → draft (loop)
//  9. max_per_hour, max_per_day, max_new_recipients_per_day → draft (limit)
//  10. tier review: every check of review.go, none skipped; any that
//     escalates → draft (review) with the verdict, all pass → held for
//     the grant's hold_seconds. No facts to check → draft (review).
//  11. tier auto                               → queue
//
// Automatic means the submitter is not a human: the loop rules apply to
// it, and when it is queued it is approved by system and carries
// Auto-Submitted. Held mail carries it too, and is approved when it is
// released (ReleasedBy).
func Decide(in Input) Decision {
	g := Effective(in)
	tier := g.Policy
	capped := OwnerCapped(in.Submitter.ID, in.Identity)
	if capped && tier.rank() > Draft.rank() {
		tier = Draft
	}
	draft := func(r HoldReason, format string, args ...any) Decision {
		return Decision{HoldReason: r, Detail: fmt.Sprintf(format, args...), Tier: tier}
	}

	if in.Identity.Disabled {
		return draft(HoldPolicy, "the identity is disabled, or another identity now covers the From address")
	}

	if c := in.ConfirmedBy; c != nil {
		switch {
		case c.Kind != KindHuman:
			return draft(HoldPolicy, "only a human can confirm a draft; %s is a %s", c.ID, c.Kind)
		case !in.CanSubmit:
			return draft(HoldPolicy, "the confirming credential lacks submission:send")
		case OwnerCapped(c.ID, in.Identity):
			return draft(HoldOwner, "the identity belongs to another human; only its owner can send as it")
		}
		return Decision{Queue: true, ApprovedBy: c.ID, Tier: tier}
	}

	if capped {
		return draft(HoldOwner, "the identity belongs to a human and the submitter is not that human: drafts only (§8.9)")
	}
	if !in.CanSubmit {
		return draft(HoldPolicy, "the credential lacks submission:send")
	}
	switch tier {
	case None:
		return draft(HoldPolicy, "no grant to send as this identity")
	case Draft:
		return draft(HoldPolicy, "the grant allows drafts only")
	}
	if g.Suspended {
		return draft(HoldSuspended, "the grant is suspended")
	}

	rcpts := fold(in.Recipients)
	if len(rcpts) == 0 {
		return draft(HoldRecipients, "no recipients")
	}
	if r, why := outsideScope(g, in.Reply, rcpts); r != "" {
		return draft(HoldRecipients, "%s: %s", r, why)
	}

	automatic := in.Submitter.Kind != KindHuman
	if automatic && in.Reply != nil {
		if why := automaticMail(in.Reply); why != "" {
			return draft(HoldLoop, "an automatic reply to automatic mail (%s)", why)
		}
		for _, r := range rcpts {
			if n := in.Counters.AutoInThread[r]; n >= MaxAutoPerThreadRecipient {
				return draft(HoldLoop, "%d automatic messages to %s in this thread in the last 24 hours (at most %d)",
					n, r, MaxAutoPerThreadRecipient)
			}
		}
	}

	c := in.Counters
	if m := g.Limits.MaxPerHour; m != nil && c.SentLastHour+1 > *m {
		return draft(HoldLimit, "max_per_hour %d reached (%d in the last hour)", *m, c.SentLastHour)
	}
	if m := g.Limits.MaxPerDay; m != nil && c.SentLastDay+1 > *m {
		return draft(HoldLimit, "max_per_day %d reached (%d in the last 24 hours)", *m, c.SentLastDay)
	}
	if m := g.Limits.MaxNewRecipientsPerDay; m != nil {
		var fresh int
		for _, r := range rcpts {
			if slices.Contains(c.FirstContact, r) {
				fresh++
			}
		}
		if fresh > 0 && c.NewRecipientsLastDay+fresh > *m {
			return draft(HoldLimit, "max_new_recipients_per_day %d: %d new recipient(s) here, %d in the last 24 hours",
				*m, fresh, c.NewRecipientsLastDay)
		}
	}

	if tier == Review {
		if in.Review == nil {
			return draft(HoldReview, "the review could not read this submission, so it stays a draft for a human")
		}
		v := review(g, in, rcpts)
		if why := v.escalated(); why != "" {
			d := draft(HoldReview, "the review escalated it: %s", why)
			d.Review = &v
			return d
		}
		d := Decision{Queue: true, Held: true, HoldSeconds: *g.HoldSeconds, Review: &v, Tier: tier}
		if automatic {
			d.AutoSubmitted = AutoSubmittedValue
		}
		return d
	}
	d := Decision{Queue: true, ApprovedBy: in.Submitter.ID, Tier: tier}
	if automatic {
		d.ApprovedBy, d.AutoSubmitted = SystemPrincipalID, AutoSubmittedValue
	}
	return d
}

// ReleasedBy is outbox.approved_by of a held row when its window ends:
// the submitter if a human (a person under a review grant pressed send),
// otherwise system, the policy that let automatic mail go.
func ReleasedBy(submitter Principal) string {
	if submitter.Kind == KindHuman {
		return submitter.ID
	}
	return SystemPrincipalID
}

// fold folds and deduplicates recipients, keeping their order.
func fold(addrs []string) []string {
	var out []string
	for _, a := range addrs {
		if f := Fold(a); strings.TrimSpace(f) != "" && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// outsideScope returns the first recipient the grant's scope does not
// admit, and why.
func outsideScope(g Grant, reply *Reply, rcpts []string) (string, string) {
	switch g.RecipientScope {
	case ScopeAny:
		return "", ""
	case ScopeThread:
		if reply == nil {
			return rcpts[0], "recipient scope thread admits replies to an existing thread only"
		}
		for _, r := range rcpts {
			if !slices.Contains(fold(reply.Correspondents), r) {
				return r, "not a correspondent in the thread (recipient scope thread)"
			}
		}
		return "", ""
	case ScopeAllowlist:
		allow := fold(g.Allowlist)
		for _, r := range rcpts {
			_, domain, _ := cutLast(r)
			if !slices.Contains(allow, r) && (domain == "" || !slices.Contains(allow, domain)) {
				return r, "not in the grant's allowlist"
			}
		}
		return "", ""
	}
	// An unknown scope admits nobody.
	return rcpts[0], fmt.Sprintf("unknown recipient scope %q", g.RecipientScope)
}

func cutLast(addr string) (local, domain string, ok bool) {
	i := strings.LastIndexByte(addr, '@')
	if i < 0 {
		return addr, "", false
	}
	return addr[:i], addr[i+1:], true
}

// automaticMail says why the replied-to message counts as automatic
// (RFC 3834 §2), or "".
func automaticMail(r *Reply) string {
	if a := strings.ToLower(strings.TrimSpace(r.AutoSubmitted)); a != "" && a != "no" {
		return "Auto-Submitted: " + a
	}
	switch p := strings.ToLower(strings.TrimSpace(r.Precedence)); p {
	case "bulk", "list", "junk":
		return "Precedence: " + p
	}
	if r.Machine {
		return "classified machine"
	}
	return ""
}
