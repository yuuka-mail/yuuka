// Package transport hands assembled MIME bytes to an outbound exit and
// reports what became of them (§10.2).
//
// The bytes are ours: the composer (or the submission listener) builds
// them, the outbox stores them, and the transport delivers them unchanged.
// It never rewrites a message. Anything it cannot send as is — bare LF,
// overlong lines, header fields the exit does not let the caller set,
// 8-bit content the server cannot take — fails before a byte of the
// message leaves, so the stored copy is always what was offered.
//
// Every Send reports a per-recipient result and one Outcome derived from
// them. The distinction that matters to the outbox (§4.5) is whether the
// attempt crossed the commit point: before it, a failure proves nothing was
// delivered and a retry is safe; after it, a missing answer is Unknown and
// is never retried blindly.
package transport

import "fmt"

// Envelope is the SMTP envelope of one attempt. It is independent of the
// header fields: Bcc recipients appear only here, and From is the
// reverse-path (where bounces go), not the visible From.
type Envelope struct {
	// From is the reverse-path. Empty means the null reverse-path <>.
	From string
	// To lists the forward-paths, one result per entry in the same order.
	To []string
}

// Caps describes what an exit lets the caller control (§10.2, §10.4). It is
// configuration of a transport instance, not something probed at run time:
// the first test of a new exit is to send to ourselves and compare (§10.4.1).
type Caps struct {
	// CallerMessageID: the exit delivers our Message-ID. When false the exit
	// generates its own and rejects or rewrites ours, so we must not send one.
	CallerMessageID bool
	// CallerReturnPath: the envelope reverse-path we choose is the one bounces
	// go to, which VERP needs (§10.4.2).
	CallerReturnPath bool
	// CallerDKIM: the exit accepts a DKIM-Signature we computed (§10.4.4).
	CallerDKIM bool
	// StatusQuery: an Unknown attempt can be reconciled by asking the exit
	// about its ProviderID. Nothing implements such a query yet.
	StatusQuery bool
}

// CloudflareCaps are the caps of Cloudflare Email Sending. Message-ID,
// DKIM-Signature and Return-Path are platform-controlled headers there
// (§10.4.1, §10.4.2, §10.4.4), and there is no status query we use.
func CloudflareCaps() Caps {
	return Caps{
		CallerMessageID:  false,
		CallerReturnPath: false,
		CallerDKIM:       false,
		StatusQuery:      false,
	}
}

// Outcome summarises one attempt. It is derived from the recipient
// statuses alone, so the two can never disagree.
type Outcome int

const (
	// OutcomeAccepted: every recipient was accepted.
	OutcomeAccepted Outcome = iota + 1
	// OutcomePartial: the message was accepted for some recipients and
	// refused (temporarily or permanently) for others. Retry only the
	// StatusDeferred ones.
	OutcomePartial
	// OutcomeFailed: nothing was delivered and at least one recipient is
	// worth retrying (StatusDeferred).
	OutcomeFailed
	// OutcomeRejected: nothing was delivered and every recipient was refused
	// permanently. Retrying the same bytes through this exit is pointless.
	OutcomeRejected
	// OutcomeUnknown: the attempt crossed the commit point and no final
	// answer arrived. The message may or may not have been delivered;
	// reconcile, do not retry (§3 principle 6).
	OutcomeUnknown
)

func (o Outcome) String() string {
	switch o {
	case OutcomeAccepted:
		return "accepted"
	case OutcomePartial:
		return "partial"
	case OutcomeFailed:
		return "failed"
	case OutcomeRejected:
		return "rejected"
	case OutcomeUnknown:
		return "unknown"
	}
	return fmt.Sprintf("Outcome(%d)", int(o))
}

// Status is the fate of one recipient. The names follow
// outbox_recipients.state (§4).
type Status int

const (
	statusPending Status = iota // not decided yet; never in a returned result

	// StatusAccepted: the exit took responsibility for this recipient.
	StatusAccepted
	// StatusDeferred: not delivered, temporarily (4xx, or a local or network
	// failure before the commit point). Safe to retry.
	StatusDeferred
	// StatusRejected: not delivered, permanently (5xx, or a local check that
	// the same bytes will fail again).
	StatusRejected
	// StatusUnknown: accepted at RCPT, then the attempt crossed the commit
	// point without a final reply.
	StatusUnknown
)

func (s Status) String() string {
	switch s {
	case statusPending:
		return "pending"
	case StatusAccepted:
		return "accepted"
	case StatusDeferred:
		return "deferred"
	case StatusRejected:
		return "rejected"
	case StatusUnknown:
		return "unknown"
	}
	return fmt.Sprintf("Status(%d)", int(s))
}

// Reply is one SMTP reply. Text is the reply without its code; lines of a
// multi-line reply are joined with "\n".
type Reply struct {
	Code int
	Text string
}

// RecipientResult is what happened to one envelope recipient.
type RecipientResult struct {
	Addr   string
	Status Status
	// Code and Text come from the reply that decided Status: RCPT for a
	// refused recipient, the final reply after the data for the others.
	// Code is 0 when no reply decided it (a local check, a network failure
	// or an Unknown); Text then says why.
	Code int
	Text string
}

// SendResult is the outcome of one attempt. Send always returns one, also
// together with an error.
type SendResult struct {
	Outcome Outcome
	// Recipients has one entry per Envelope.To, in the same order.
	Recipients []RecipientResult
	// Committed reports that commit was called and returned nil, i.e. the
	// attempt crossed the point of no return. When false, nothing was
	// delivered to anyone and the outcome is never Unknown.
	Committed bool
	// Final is the reply to the end of the data, when one arrived. It is
	// kept verbatim because the exits' formats are not known yet (§10.4.1).
	Final Reply
	// ProviderID is the exit's id for the accepted message, parsed from
	// Final only when it has an unambiguous form ("queued as X").
	ProviderID string
	// MessageID is the Message-ID the exit actually used. It stays empty
	// until a real send shows where an exit reports it (§10.4.1); guessing
	// would break thread matching silently.
	MessageID string
}

// summarize derives the outcome from the recipient statuses.
func summarize(rs []RecipientResult) Outcome {
	var accepted, deferred, unknown int
	for _, r := range rs {
		switch r.Status {
		case StatusAccepted:
			accepted++
		case StatusDeferred:
			deferred++
		case StatusUnknown:
			unknown++
		}
	}
	switch {
	case unknown > 0:
		return OutcomeUnknown
	case accepted > 0 && accepted == len(rs):
		return OutcomeAccepted
	case accepted > 0:
		return OutcomePartial
	case deferred > 0:
		return OutcomeFailed
	default:
		return OutcomeRejected
	}
}
