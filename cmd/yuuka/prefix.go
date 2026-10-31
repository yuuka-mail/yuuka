package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/ingest"
	"github.com/yuuka-mail/yuuka/internal/leak"
	"github.com/yuuka-mail/yuuka/internal/store"
)

const prefixUsage = "usage: yuuka prefix ls [--state claimed|discovered] [--zone Z] | yuuka prefix claim <local>@<zone> | " +
	"yuuka prefix unclaim <local>@<zone> | yuuka prefix expect <local>@<zone> --domains <domain>[,<domain>]"

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
		zone, prefix, err := ingest.ParsePrefixAddress(fs.Arg(0))
		if err != nil {
			return err
		}
		if args[0] == "claim" {
			return h.prefixClaim(ctx, zone, prefix)
		}
		return h.prefixUnclaim(ctx, zone, prefix)
	case "expect":
		fs := flag.NewFlagSet("prefix expect", flag.ContinueOnError)
		domains := fs.String("domains", "", "comma-separated domains the address was given to; replaces the list, empty clears it")
		// The address may come before the flags, as with claim.
		rest, addr := args[1:], ""
		if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
			addr, rest = rest[0], rest[1:]
		}
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if addr == "" && fs.NArg() == 1 {
			addr = fs.Arg(0)
		} else if addr == "" || fs.NArg() != 0 {
			return errors.New(prefixUsage)
		}
		given := false
		fs.Visit(func(f *flag.Flag) { given = given || f.Name == "domains" })
		if !given {
			return errors.New("prefix expect: --domains is required (--domains '' expects nothing)")
		}
		zone, prefix, err := ingest.ParsePrefixAddress(addr)
		if err != nil {
			return err
		}
		var list []string
		for d := range strings.SplitSeq(*domains, ",") {
			if d = strings.TrimSpace(d); d != "" {
				list = append(list, d)
			}
		}
		return h.prefixExpect(ctx, zone, prefix, list)
	}
	return errors.New(prefixUsage)
}

func (h *host) prefixList(ctx context.Context, q store.PrefixQuery) error {
	sum, err := h.db.PrefixStats(ctx, h.viewer(auth.PrefixRead), q)
	if err != nil {
		return err
	}
	// Write errors surface on Flush.
	tw := tabwriter.NewWriter(h.out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ZONE\tPREFIX\tSTATE\tFIRST SEEN\tLAST SEEN\tDELIVERIES\tEXPECTED\tOBSERVED\tLEAKS\tSPOOFS")
	for _, p := range sum.Prefixes {
		// Unclaimed prefixes are not checked for leaks (§3.3).
		state, expected, observed, leaks, spoofs := "discovered", "-", "-", "-", "-"
		if p.State == store.PrefixClaimed {
			state, expected, observed = "claimed", list(p.ExpectedDomains), observedList(p.Observed)
			leaks, spoofs = strconv.Itoa(p.Leaks), strconv.Itoa(p.Spoofs)
			if p.Unverified > 0 {
				leaks += fmt.Sprintf(" (%d unverified)", p.Unverified)
			}
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", shown(p.Zone), shown(p.Prefix), state,
			stamp(p.FirstSeen), stamp(p.LastSeen), p.Deliveries, expected, observed, leaks, spoofs)
	}
	return tw.Flush()
}

func list(ds []string) string {
	if len(ds) == 0 {
		return "-"
	}
	return strings.Join(ds, ",")
}

// observedList is the DMARC-proven senders with their message counts,
// those the prefix was not given to marked with a !.
func observedList(os []store.ObservedDomain) string {
	if len(os) == 0 {
		return "-"
	}
	out := make([]string, len(os))
	for i, o := range os {
		mark := ""
		if !o.Expected {
			mark = "!"
		}
		out[i] = fmt.Sprintf("%s%s(%d)", mark, o.Domain, o.Messages)
	}
	return strings.Join(out, ",")
}

func (h *host) prefixExpect(ctx context.Context, zone, prefix string, domains []string) error {
	addr := shown(prefix + "@" + zone)
	stored, changed, err := h.db.SetPrefixExpected(ctx, zone, prefix, domains, h.action("prefix.expect", nil))
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("%s is not in the prefix registry; claim it first", addr)
	case errors.Is(err, store.ErrNotClaimed):
		return fmt.Errorf("%s is not claimed; leak detection ignores unclaimed prefixes, so claim it first", addr)
	case err != nil:
		return err
	}
	for _, d := range domains {
		if org := leak.OrgDomain(d); org != strings.ToLower(strings.TrimSuffix(d, ".")) {
			h.note("%s counts as %s: senders are compared by organizational domain.", shown(d), org)
		}
	}
	verb := "expects"
	if !changed {
		verb = "already expects"
	}
	if len(stored) == 0 {
		if err := h.say("%s %s nothing; its mail is not judged", addr, verb); err != nil {
			return err
		}
	} else if err := h.say("%s %s %s", addr, verb, strings.Join(stored, ", ")); err != nil {
		return err
	}
	// What the new expectation makes of the mail already there: it is
	// judged on read, so this is what every view now shows.
	sum, err := h.db.PrefixStats(ctx, h.viewer(auth.PrefixRead), store.PrefixQuery{Zone: zone, Prefix: prefix})
	if err != nil || len(sum.Prefixes) != 1 {
		return err
	}
	p := sum.Prefixes[0]
	var strangers []string
	for _, o := range p.Observed {
		if !o.Expected {
			strangers = append(strangers, fmt.Sprintf("%s (%d)", o.Domain, o.Messages))
		}
	}
	if len(strangers) > 0 {
		h.note("Proven senders it was not given to: %s.", strings.Join(strangers, ", "))
	}
	if p.Suspect() {
		h.note("Of its mail so far: %d leaked (%d from unverified senders), %d spoofed.", p.Leaks, p.Unverified, p.Spoofs)
	}
	return nil
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
