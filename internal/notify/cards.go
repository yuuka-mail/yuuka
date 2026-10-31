package notify

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Cards of the review tier (§8.4, §8.9). Every field is rendered by the
// system from the outbox or draft rows and signed by yuuka; nothing an
// agent wrote about its mail appears, so a card cannot be talked into
// looking harmless. Plain text only, every field cleaned like Summary.
// The one button that acts is the recall of a held message; nothing on
// a card sends mail, so a stolen Telegram account can at worst hold mail
// back.

// Attachment is one attachment as a card lists it.
type Attachment struct {
	Name string
	Type string
	Size int
}

// HeldCard is mail the review passed, waiting out its recall window.
type HeldCard struct {
	OutboxID string
	From     string
	To       []string
	Cc       []string
	Bcc      []string
	// Submitter is kind:name.
	Submitter string
	// ThreadSubject is the subject of the message replied to, "" for a
	// new thread.
	ThreadSubject string
	Subject       string
	Attachments   []Attachment
	// Excerpt is the start of the body; HeldText keeps 3 lines and 300
	// characters of it.
	Excerpt string
	// Passed names the checks that passed.
	Passed []string
	SendAt time.Time
	Now    time.Time
}

// EscalatedCard is a submission the review escalated to a draft.
type EscalatedCard struct {
	DraftID   string
	From      string
	To        []string
	Cc        []string
	Bcc       []string
	Submitter string
	Subject   string
	// Reasons are the checks that did not pass, in their words.
	Reasons []string
}

// SuspendedCard is a grant the circuit breaker suspended.
type SuspendedCard struct {
	Identity  string
	Principal string
	Recalls   int
	Withdrawn int
}

// Excerpt lines and characters on a held card (§8.4: the start of the
// body, so a person sees what goes out, not a summary of it).
const (
	excerptLines = 3
	excerptChars = 300
)

// RecallData is the callback data of the recall button of held row id.
func RecallData(id string) string { return "w:" + id }

// HeldText renders c, in the system's voice.
func HeldText(c HeldCard, loc *time.Location) string {
	var b strings.Builder
	b.WriteString("yuuka · held for review\n")
	fmt.Fprintf(&b, "%s → %s\n", clean(c.From), recipients(c.To, c.Cc, c.Bcc))
	fmt.Fprintf(&b, "submitted by %s\n", clean(c.Submitter))
	if c.ThreadSubject != "" {
		fmt.Fprintf(&b, "in reply to: %s\n", clean(c.ThreadSubject))
	}
	fmt.Fprintf(&b, "subject: %s\n", clean(c.Subject))
	for _, a := range c.Attachments {
		fmt.Fprintf(&b, "attachment: %s (%s, %s)\n", clean(a.Name), clean(a.Type), size(a.Size))
	}
	if ex := excerpt(c.Excerpt); ex != "" {
		b.WriteString("\n" + ex + "\n\n")
	}
	if len(c.Passed) > 0 {
		fmt.Fprintf(&b, "passed: %s\n", strings.Join(c.Passed, ", "))
	}
	if loc == nil {
		loc = time.Local
	}
	left := max(c.SendAt.Sub(c.Now), 0).Round(time.Minute)
	fmt.Fprintf(&b, "goes out at %s (in %d min) unless recalled", c.SendAt.In(loc).Format("15:04"), int(left.Minutes()))
	return b.String()
}

// EscalatedText renders c.
func EscalatedText(c EscalatedCard) string {
	var b strings.Builder
	b.WriteString("yuuka · escalated to a draft\n")
	fmt.Fprintf(&b, "%s → %s\n", clean(c.From), recipients(c.To, c.Cc, c.Bcc))
	fmt.Fprintf(&b, "submitted by %s\n", clean(c.Submitter))
	fmt.Fprintf(&b, "subject: %s\n", clean(c.Subject))
	for _, r := range c.Reasons {
		fmt.Fprintf(&b, "· %s\n", clean(r))
	}
	b.WriteString("Nothing was sent. It waits as a draft for a person.")
	return b.String()
}

// SuspendedText renders c.
func SuspendedText(c SuspendedCard) string {
	return fmt.Sprintf("yuuka · grant suspended\n%s on %s: %d recalls by a person in 24 hours.\n"+
		"%d other held message(s) went back to drafts. Everything it submits stays a draft until "+
		"`yuuka grant set --resume`.", clean(c.Principal), clean(c.Identity), c.Recalls, c.Withdrawn)
}

func recipients(to, cc, bcc []string) string {
	var parts []string
	for _, l := range []struct {
		label string
		addrs []string
	}{{"", to}, {"cc ", cc}, {"bcc ", bcc}} {
		if len(l.addrs) == 0 {
			continue
		}
		shown := make([]string, len(l.addrs))
		for i, a := range l.addrs {
			shown[i] = clean(a)
		}
		parts = append(parts, l.label+strings.Join(shown, ", "))
	}
	if len(parts) == 0 {
		return "?"
	}
	return strings.Join(parts, "; ")
}

// excerpt keeps the first lines that say something, each cleaned, within
// the bounds.
func excerpt(s string) string {
	var lines []string
	n := 0
	for line := range strings.SplitSeq(s, "\n") {
		line = clean(line)
		if line == "" {
			continue
		}
		if r := utf8.RuneCountInString(line); n+r > excerptChars {
			line = string([]rune(line)[:max(excerptChars-n, 0)]) + "…"
			lines = append(lines, line)
			break
		}
		n += utf8.RuneCountInString(line)
		lines = append(lines, line)
		if len(lines) == excerptLines {
			break
		}
	}
	return strings.Join(lines, "\n")
}

func size(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// Cards sends the review tier's cards through a Telegram bot.
type Cards struct {
	TG *Telegram
	// Origin is the WebUI's origin, for the buttons that open it; ""
	// leaves them out.
	Origin string
	// Location is where "goes out at" is told; nil is the local zone.
	Location *time.Location
	// Recall puts the recall button on held cards: only when a Poller
	// answers it.
	Recall bool
}

// Held sends the card of a held message with its recall button.
func (c *Cards) Held(ctx context.Context, h HeldCard) error {
	var row []Button
	if c.Recall {
		row = append(row, Button{Text: "Recall", Data: RecallData(h.OutboxID)})
	}
	if u := c.link("outbox", "o", h.OutboxID); u != "" {
		row = append(row, Button{Text: "Open", URL: u})
	}
	var rows [][]Button
	if len(row) > 0 {
		rows = append(rows, row)
	}
	_, err := c.TG.SendCard(ctx, HeldText(h, c.Location), rows)
	return err
}

// Escalated sends the card of an escalated submission.
func (c *Cards) Escalated(ctx context.Context, e EscalatedCard) error {
	var rows [][]Button
	if u := c.link("drafts", "d", e.DraftID); u != "" {
		rows = append(rows, []Button{{Text: "Open draft", URL: u}})
	}
	_, err := c.TG.SendCard(ctx, EscalatedText(e), rows)
	return err
}

// Suspended sends the card of a suspended grant.
func (c *Cards) Suspended(ctx context.Context, s SuspendedCard) error {
	_, err := c.TG.SendCard(ctx, SuspendedText(s), nil)
	return err
}

// link is the WebUI's view of one outbox row or draft.
func (c *Cards) link(area, key, id string) string {
	if c.Origin == "" || id == "" {
		return ""
	}
	q := url.Values{"area": {area}, key: {id}}
	return strings.TrimSuffix(c.Origin, "/") + "/?" + q.Encode()
}
