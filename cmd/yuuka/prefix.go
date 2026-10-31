package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/ingest"
	"github.com/yuuka-mail/yuuka/internal/store"
)

const prefixUsage = "usage: yuuka prefix ls [--state claimed|discovered] [--zone Z] | yuuka prefix claim <local>@<zone> | yuuka prefix unclaim <local>@<zone>"

// prefixCmd is `yuuka prefix`: the registry of §3.3.
func (h *host) prefixCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(prefixUsage)
	}
	switch args[0] {
	case "ls":
		fs := flag.NewFlagSet("prefix ls", flag.ContinueOnError)
		state := fs.String("state", "", "only claimed or only discovered prefixes")
		zone := fs.String("zone", "", "only this zone (domain)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New(prefixUsage)
		}
		q := store.PrefixQuery{Zone: strings.ToLower(*zone)}
		switch *state {
		case "":
		case "claimed":
			q.State = store.PrefixClaimed
		case "discovered":
			q.State = store.PrefixDiscovered
		default:
			return fmt.Errorf("--state %q: want claimed or discovered", *state)
		}
		return h.prefixList(ctx, q)
	case "claim", "unclaim":
		fs := flag.NewFlagSet("prefix "+args[0], flag.ContinueOnError)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New(prefixUsage)
		}
		zone, prefix, err := parsePrefixAddress(fs.Arg(0))
		if err != nil {
			return err
		}
		if args[0] == "claim" {
			return h.prefixClaim(ctx, zone, prefix)
		}
		return h.prefixUnclaim(ctx, zone, prefix)
	}
	return errors.New(prefixUsage)
}

// parsePrefixAddress reads <local>@<zone> exactly as ingest keys the
// envelope recipient (ingest.SplitAddress), so a claim matches the mail.
func parsePrefixAddress(addr string) (zone, prefix string, err error) {
	if strings.IndexFunc(addr, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", "", fmt.Errorf("%q: an address has no spaces or control characters", addr)
	}
	zone, prefix = ingest.SplitAddress(addr)
	if zone == "" || prefix == "" || !strings.Contains(addr, "@") {
		return "", "", fmt.Errorf("%q: want <local>@<zone>, e.g. github@example.com", addr)
	}
	return zone, prefix, nil
}

func (h *host) prefixList(ctx context.Context, q store.PrefixQuery) error {
	sum, err := h.db.PrefixStats(ctx, h.viewer(auth.PrefixRead), q)
	if err != nil {
		return err
	}
	// Write errors surface on Flush.
	tw := tabwriter.NewWriter(h.out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ZONE\tPREFIX\tSTATE\tFIRST SEEN\tLAST SEEN\tDELIVERIES")
	for _, p := range sum.Prefixes {
		state := "discovered"
		if p.State == store.PrefixClaimed {
			state = "claimed"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n", shown(p.Zone), shown(p.Prefix), state,
			stamp(p.FirstSeen), stamp(p.LastSeen), p.Deliveries)
	}
	return tw.Flush()
}

func (h *host) prefixClaim(ctx context.Context, zone, prefix string) error {
	addr := shown(prefix + "@" + zone)
	got, err := h.db.SetPrefixState(ctx, zone, prefix, store.PrefixClaimed, h.action("prefix.claim", nil))
	if err != nil {
		return err
	}
	switch got {
	case store.PrefixUnchanged:
		return h.say("%s is already claimed", addr)
	case store.PrefixCreated:
		return h.say("claimed %s; no mail has reached it yet", addr)
	}
	return h.say("claimed %s; its mail is no longer downgraded unless DMARC failed, and new mail to it notifies", addr)
}

func (h *host) prefixUnclaim(ctx context.Context, zone, prefix string) error {
	addr := shown(prefix + "@" + zone)
	got, err := h.db.SetPrefixState(ctx, zone, prefix, store.PrefixDiscovered, h.action("prefix.unclaim", nil))
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%s is not in the prefix registry", addr)
	}
	if err != nil {
		return err
	}
	switch got {
	case store.PrefixUnchanged:
		return h.say("%s is not claimed", addr)
	case store.PrefixRemoved:
		return h.say("unclaimed %s; it never received mail, so it left the registry", addr)
	}
	return h.say("unclaimed %s; its mail is downgraded again", addr)
}
