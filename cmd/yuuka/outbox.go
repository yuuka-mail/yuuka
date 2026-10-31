package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/outbox"
	"github.com/yuuka-mail/yuuka/internal/store"
)

const outboxUsage = "usage: yuuka outbox ls [--state S] [--limit N] | yuuka outbox resolve <id> --sent|--resend|--abandon"

var outboxStates = []string{
	store.OutboxHeld, store.OutboxQueued, store.OutboxSending, store.OutboxSent, store.OutboxPartial,
	store.OutboxUnknown, store.OutboxFailed, store.OutboxDead, store.OutboxWithdrawn,
}

// outboxCmd is `yuuka outbox`: what is being sent, and the human decision
// on mail whose fate is unknown (§4.5).
func (h *host) outboxCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(outboxUsage)
	}
	switch args[0] {
	case "ls":
		fs := flag.NewFlagSet("outbox ls", flag.ContinueOnError)
		state := fs.String("state", "", "only rows in this state ("+strings.Join(outboxStates, ", ")+")")
		limit := fs.Int("limit", 50, "at most this many rows, newest first")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New(outboxUsage)
		}
		if *state != "" && !slices.Contains(outboxStates, *state) {
			return fmt.Errorf("--state %q: want one of %s", *state, strings.Join(outboxStates, ", "))
		}
		return h.outboxList(ctx, store.OutboxQuery{State: *state, Limit: *limit})
	case "resolve":
		fs := flag.NewFlagSet("outbox resolve", flag.ContinueOnError)
		sent := fs.Bool("sent", false, "it was delivered: settle it as sent")
		resend := fs.Bool("resend", false, "send it again, accepting that someone may get it twice")
		abandon := fs.Bool("abandon", false, "give up: settle it as dead")
		// The id comes first, as in the usage; flags may follow it.
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return errors.New(outboxUsage)
		}
		id := args[1]
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New(outboxUsage)
		}
		var how []string
		for name, set := range map[string]bool{store.ResolveSent: *sent, store.ResolveResend: *resend, store.ResolveAbandon: *abandon} {
			if set {
				how = append(how, name)
			}
		}
		if len(how) != 1 {
			return errors.New("outbox resolve: give exactly one of --sent, --resend, --abandon")
		}
		return h.outboxResolve(ctx, strings.ToUpper(id), how[0])
	}
	return errors.New(outboxUsage)
}

func (h *host) outboxList(ctx context.Context, q store.OutboxQuery) error {
	rows, err := h.db.OutboxList(ctx, h.viewer(auth.MessageRead), q)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(h.out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tSTATE\tIDENTITY\tFROM\tRECIPIENTS\tATTEMPTS\tNEXT TRY\tLAST ERROR")
	for _, r := range rows {
		attempts := fmt.Sprint(r.Attempts)
		if r.LastOutcome != "" {
			attempts += " (" + r.LastOutcome + ")"
		}
		errText := r.Error
		if errText == "" {
			errText = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.State, shown(r.Identity), shown(r.FromAddr),
			recipientSummary(r.Recipients), attempts, stamp(r.NextRetryAt), shown(errText))
	}
	return tw.Flush()
}

// recipientSummary is the first recipient, how many more, and the count
// of each state: "a@example.net +2 [accepted 2, deferred 1]".
func recipientSummary(rs []store.OutboxRecipientState) string {
	if len(rs) == 0 {
		return "-"
	}
	s := shown(rs[0].Addr)
	if len(rs) > 1 {
		s += fmt.Sprintf(" +%d", len(rs)-1)
	}
	counts := map[string]int{}
	var order []string
	for _, r := range rs {
		if counts[r.State] == 0 {
			order = append(order, r.State)
		}
		counts[r.State]++
	}
	parts := make([]string, len(order))
	for i, st := range order {
		parts[i] = fmt.Sprintf("%s %d", st, counts[st])
	}
	return s + " [" + strings.Join(parts, ", ") + "]"
}

func (h *host) outboxResolve(ctx context.Context, id, how string) error {
	state, err := h.db.ResolveOutbox(ctx, id, how, h.action("outbox.resolve", nil), outbox.DefaultRetry)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("no outbox row %s", id)
	case err != nil:
		return err
	}
	switch how {
	case store.ResolveResend:
		h.note("a running yuuka serve sends it on its next look at the queue")
	case store.ResolveSent:
		if state == store.OutboxPartial || state == store.OutboxFailed {
			h.note("some recipients were deferred and will be retried")
		}
	}
	return h.say("%s is now %s", id, state)
}
