package triage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/yuuka-mail/yuuka/internal/agentview"
	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/extract"
	"github.com/yuuka-mail/yuuka/internal/mimeparse"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/triage/score"
)

// DefaultMaxChars bounds the content of one state. An encoder reads a
// few hundred tokens; the start of a mail is where its kind and its
// pitch are.
const DefaultMaxChars = 4000

// viewer is who the scorer reads as: the system, which sees every
// message. The scorer is not a principal (it holds no credential and
// decides nothing); what it is given is narrowed below, not by scope.
var viewer = &auth.Viewer{
	PrincipalID: store.SystemPrincipalID, PrincipalKind: "system", PrincipalName: "system",
	Scopes: []auth.Scope{auth.MessageRead, auth.DeliveryRead},
}

// Messages is what the input builder reads from the store: the message
// and whether its sender is known. Nothing else: notes, labels,
// annotations, proposals, drafts and the rest of the thread are
// decisions or other mail, which agents can write and senders can
// steer; the scorer never sees them (§8.10).
type Messages interface {
	MessageByID(ctx context.Context, v *auth.Viewer, id string) (store.MessageDetail, error)
	KnownSender(ctx context.Context, v *auth.Viewer, id string) (bool, error)
}

// RawReader reads a raw message (ingest.Ingester.ReadRaw).
type RawReader interface {
	ReadRaw(ctx context.Context, ref store.RawRef) ([]byte, error)
}

// ErrNotTarget: the message is not one to score: it arrived nowhere,
// or every delivery is downgraded by §4.3, which already hides it.
var ErrNotTarget = errors.New("triage: not a message to score")

// Built is the state of one message and the record of what it held.
type Built struct {
	MessageID string // aliases resolved
	State     string
	Input     score.Input
}

// Build reads message id and builds its state.
func Build(ctx context.Context, ms Messages, raws RawReader, id string, maxChars int) (Built, error) {
	m, err := ms.MessageByID(ctx, viewer, id)
	if err != nil {
		return Built{}, err
	}
	if !escapes(m.Deliveries) {
		return Built{}, ErrNotTarget
	}
	raw, err := raws.ReadRaw(ctx, m.Raw)
	if err != nil {
		return Built{}, fmt.Errorf("message %s: %w", m.ID, err)
	}
	known, err := ms.KnownSender(ctx, viewer, m.ID)
	if err != nil {
		return Built{}, err
	}
	state, in := State(m, raw, known, maxChars)
	return Built{MessageID: m.ID, State: state, Input: in}, nil
}

// escapes: some delivery escapes the §4.3 downgrade, as notDowngraded
// says in the store. Not MessageDetail.Downgraded, which a model
// downgrade will join: scoring must not depend on its own result.
func escapes(ds []store.DeliveryFact) bool {
	for _, d := range ds {
		if d.PrefixState == store.PrefixClaimed && (d.Auth == nil || d.Auth.DMARC != "fail") {
			return true
		}
	}
	return false
}

// State renders what the scorer reads about message m, with raw the
// message of m.Raw: yuuka's facts, then the sender's content fenced as
// untrusted (§8.3), cut to maxChars characters. The facts are booleans
// and the authentication line; the extractor's values (codes, links) are
// never in it, only whether there were any, and the content has no
// attachment or extraction lines. It is a pure function of its
// arguments.
func State(m store.MessageDetail, raw []byte, knownSender bool, maxChars int) (string, score.Input) {
	vin := agentview.InputOf(viewer, m, raw)
	vin.Extracted = nil
	v, _ := agentview.Select(agentview.Build(vin), agentview.LevelPart, agentview.Anchor{})
	v.Content.Extracted, v.Content.Attachments, v.Content.Sections = nil, nil, nil
	msg := mimeparse.Parse(raw)

	f := score.Facts{
		Auth:        cutBytes(v.Facts.Auth.Line, maxAuthLine),
		Claimed:     claimed(m.Deliveries),
		KnownSender: knownSender,
		ListID:      msg.ListID != "",
		Class:       m.Class.String(),
	}
	from := domainOf(msg.FromAddr)
	for _, a := range msg.ReplyTo {
		if d := domainOf(a); d != "" && d != from {
			f.ReplyToOther = true
		}
	}
	for _, x := range m.Extractions {
		switch extract.Kind(x.Kind) {
		case extract.OTP:
			f.HasCode = true
		case extract.Link:
			f.HasLink = true
		}
	}

	content := v.ContentText()
	truncated := v.Content.More != ""
	if maxChars > 0 && utf8.RuneCountInString(content) > maxChars {
		content = string([]rune(content)[:maxChars])
		truncated = true
	}

	var b strings.Builder
	b.WriteString("An email received by the mail system yuuka. The facts come from yuuka; " +
		"everything inside the untrusted_mail_content tags was written by the sender.\n\nFacts:\n")
	fmt.Fprintf(&b, "- authentication by the receiving server: %s\n", f.Auth)
	fmt.Fprintf(&b, "- delivered to an address the recipient gave out: %s\n", yesNo(f.Claimed))
	fmt.Fprintf(&b, "- the sender's address has written before: %s\n", yesNo(f.KnownSender))
	fmt.Fprintf(&b, "- Reply-To is in another domain than From: %s\n", yesNo(f.ReplyToOther))
	fmt.Fprintf(&b, "- mailing list (List-Id header): %s\n", yesNo(f.ListID))
	fmt.Fprintf(&b, "- sent by a machine or a person, by its headers: %s\n", f.Class)
	fmt.Fprintf(&b, "- contains a one-time code: %s\n", yesNo(f.HasCode))
	fmt.Fprintf(&b, "- contains a sign-in or verification link: %s\n", yesNo(f.HasLink))
	b.WriteByte('\n')
	b.WriteString(agentview.Fence(v.URI, content))
	if truncated {
		b.WriteString("The content was cut; the rest of the email is not shown.\n")
	}
	state := b.String()
	sum := sha256.Sum256([]byte(state))
	return state, score.Input{
		View: agentview.Version, Parser: mimeparse.Version,
		Chars: utf8.RuneCountInString(content), Truncated: truncated,
		SHA256: hex.EncodeToString(sum[:]), Facts: f,
	}
}

// maxAuthLine is what a score's input keeps of the auth line. The line
// is short, but its from_domain is the sender's to make long.
const maxAuthLine = 300

// cutBytes shortens s to at most n bytes, on a character boundary.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func claimed(ds []store.DeliveryFact) bool {
	for _, d := range ds {
		if d.PrefixState == store.PrefixClaimed {
			return true
		}
	}
	return false
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return strings.ToLower(addr[i+1:])
	}
	return ""
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
