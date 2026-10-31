package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/yuuka-mail/yuuka/internal/sendpolicy"
	"github.com/yuuka-mail/yuuka/internal/store"
)

const grantUsage = "usage: yuuka grant set --identity <addr>|*@<zone> --principal <kind>:<name> [--policy none|draft|review|auto] " +
	"[--recipients thread|allowlist|any] [--allow a@x,domain,...] [--max-per-hour N|default] [--max-per-day N|default] " +
	"[--max-new-recipients-per-day N|default] [--hold-seconds N|default] [--suspend|--resume] | " +
	"yuuka grant ls [--identity A] [--principal P] | yuuka grant rm --identity A --principal P"

// grantCmd is `yuuka grant`: who may send as which identity, and how far
// (§13.5, §8.9).
func (h *host) grantCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(grantUsage)
	}
	switch args[0] {
	case "set":
		return h.grantSet(ctx, args[1:])
	case "ls":
		fs := flag.NewFlagSet("grant ls", flag.ContinueOnError)
		identity := fs.String("identity", "", "only the grants on this identity")
		principal := fs.String("principal", "", "only the grants of <kind>:<name>")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New(grantUsage)
		}
		return h.grantList(ctx, *identity, *principal)
	case "rm":
		fs := flag.NewFlagSet("grant rm", flag.ContinueOnError)
		identity := fs.String("identity", "", "the identity as yuuka identity ls shows it")
		principal := fs.String("principal", "", "<kind>:<name>")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || *identity == "" || *principal == "" {
			return errors.New(grantUsage)
		}
		return h.grantRemove(ctx, *identity, *principal)
	}
	return errors.New(grantUsage)
}

// grantIdentity is the enabled identity at addr exactly: a grant names
// an identity, so *@zone means the pattern and a@zone only an identity of
// its own, never the pattern that would send it.
func (h *host) grantIdentity(ctx context.Context, addr string) (store.Identity, error) {
	zone, prefix, err := store.SplitIdentityAddress(addr)
	if err != nil {
		return store.Identity{}, err
	}
	i, err := h.db.IdentityAt(ctx, zone, prefix)
	if errors.Is(err, store.ErrNotFound) {
		return i, fmt.Errorf("%s is not an enabled identity (see `yuuka identity ls`)", shown(addr))
	}
	return i, err
}

// limitFlag is a nullable number: a number, or "default" for NULL (the
// default of sendpolicy.DefaultLimits, or sendpolicy.DefaultHoldSeconds).
type limitFlag struct{ v **int }

func (f limitFlag) String() string {
	if f.v == nil || *f.v == nil {
		return "default"
	}
	return strconv.Itoa(**f.v)
}

func (f limitFlag) Set(s string) error {
	if s == "default" {
		*f.v = nil
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return fmt.Errorf("want a number >= 0 or default")
	}
	*f.v = &n
	return nil
}

// parseAllowlist reads --allow: addresses and domains, lowercased.
func parseAllowlist(s string) ([]string, error) {
	out := []string{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for e := range strings.SplitSeq(s, ",") {
		e = strings.ToLower(strings.TrimSpace(e))
		local, domain, isAddr := strings.Cut(e, "@")
		bad := e == "" || strings.IndexFunc(e, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 ||
			strings.Count(e, "@") > 1 || isAddr && (local == "" || domain == "") || !isAddr && !strings.Contains(e, ".")
		if bad {
			return nil, fmt.Errorf("--allow %q: want addresses (a@example.net) and domains (example.net), comma-separated", e)
		}
		out = append(out, e)
	}
	return out, nil
}

func (h *host) grantSet(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("grant set", flag.ContinueOnError)
	identity := fs.String("identity", "", "the identity as yuuka identity ls shows it, e.g. a@example.com or *@example.com")
	principal := fs.String("principal", "", "who may send as it: human:<name>, agent:<name> or service:<name>")
	policy := fs.String("policy", "", "none, draft, review (checked, then held for a recall window) or auto; a new grant starts at none")
	scope := fs.String("recipients", "", "thread (replies to the thread's participants), allowlist or any; a new grant starts at thread")
	allow := fs.String("allow", "", "comma-separated addresses and domains for --recipients allowlist; replaces the list, empty clears it")
	var g store.Grant
	fs.Var(limitFlag{&g.MaxPerHour}, "max-per-hour", "messages in any 60 minutes, or default")
	fs.Var(limitFlag{&g.MaxPerDay}, "max-per-day", "messages in any 24 hours, or default")
	fs.Var(limitFlag{&g.MaxNewRecipientsPerDay}, "max-new-recipients-per-day", "addresses never written to before, in any 24 hours, or default")
	fs.Var(limitFlag{&g.HoldSeconds}, "hold-seconds", "recall window of the review tier in seconds, 0 for none, or default")
	suspend := fs.Bool("suspend", false, "suspend the grant: every submission stays a draft until --resume")
	resume := fs.Bool("resume", false, "lift a suspension")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *identity == "" || *principal == "" || *suspend && *resume {
		return errors.New(grantUsage)
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	ident, err := h.grantIdentity(ctx, *identity)
	if err != nil {
		return err
	}
	p, err := h.grantee(ctx, *principal)
	if err != nil {
		return err
	}
	// set changes what is given and keeps the rest: the stored row, or
	// the column defaults of a new one (none, thread, no own limits).
	cur, err := h.db.GrantFor(ctx, ident.ID, p.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		cur = store.Grant{Policy: sendpolicy.None, RecipientScope: sendpolicy.ScopeThread, Allowlist: []string{}}
	case err != nil:
		return err
	}
	cur.IdentityID, cur.PrincipalID = ident.ID, p.ID
	if given["policy"] {
		if cur.Policy, err = sendpolicy.ParseTier(*policy); err != nil {
			return err
		}
	}
	if given["recipients"] {
		if cur.RecipientScope, err = sendpolicy.ParseRecipientScope(*scope); err != nil {
			return err
		}
	}
	if given["allow"] {
		if cur.Allowlist, err = parseAllowlist(*allow); err != nil {
			return err
		}
	}
	if given["max-per-hour"] {
		cur.MaxPerHour = g.MaxPerHour
	}
	if given["max-per-day"] {
		cur.MaxPerDay = g.MaxPerDay
	}
	if given["max-new-recipients-per-day"] {
		cur.MaxNewRecipientsPerDay = g.MaxNewRecipientsPerDay
	}
	if given["hold-seconds"] {
		cur.HoldSeconds = g.HoldSeconds
	}
	if *suspend && cur.SuspendedAt == nil {
		now := h.now()
		cur.SuspendedAt = &now
	}
	if *resume {
		cur.SuspendedAt = nil
	}

	args2 := map[string]any{
		"identity_id": ident.ID, "identity": ident.Address(), "principal_id": p.ID, "principal": p.Kind + ":" + p.Name,
		"policy": cur.Policy, "recipient_scope": cur.RecipientScope, "allowlist": cur.Allowlist,
		"max_per_hour": cur.MaxPerHour, "max_per_day": cur.MaxPerDay, "max_new_recipients_per_day": cur.MaxNewRecipientsPerDay,
		"hold_seconds": cur.HoldSeconds, "suspended": cur.SuspendedAt != nil,
	}
	err = h.db.SetGrant(ctx, cur, h.action("grant.set", args2))
	if errors.Is(err, store.ErrOwnerCap) {
		// Refused rather than stored and capped: a row saying auto that
		// can never act would mislead whoever reads it next.
		return fmt.Errorf("%s belongs to %s:%s; %s:%s can be granted draft at most (§8.9), and the cap holds whatever the grant says",
			shown(ident.Address()), ident.OwnerKind, ident.OwnerName, p.Kind, p.Name)
	}
	if err != nil {
		return err
	}
	if cur.Policy == sendpolicy.Review {
		h.note("review: mail the checks pass is held for the recall window, then sent; mail they escalate stays a draft. " +
			"Two recalls by a person within a day suspend the grant.")
	}
	if cur.RecipientScope == sendpolicy.ScopeAllowlist && len(cur.Allowlist) == 0 {
		h.note("recipients allowlist with an empty --allow admits nobody.")
	}
	return h.say("%s:%s on %s: %s", p.Kind, p.Name, shown(ident.Address()), describeGrant(cur, p.Kind))
}

// describeGrant is a grant in one line, defaults spelled out.
func describeGrant(g store.Grant, kind string) string {
	def := sendpolicy.DefaultLimits(kind)
	limit := func(v, d *int) string {
		switch {
		case v != nil:
			return strconv.Itoa(*v)
		case d != nil:
			return strconv.Itoa(*d) + " (default)"
		}
		return "none"
	}
	s := fmt.Sprintf("%s, recipients %s", g.Policy, g.RecipientScope)
	if g.RecipientScope == sendpolicy.ScopeAllowlist {
		s += " [" + strings.Join(g.Allowlist, ",") + "]"
	}
	s += fmt.Sprintf(", %s/hour, %s/day, %s new recipients/day", limit(g.MaxPerHour, def.MaxPerHour),
		limit(g.MaxPerDay, def.MaxPerDay), limit(g.MaxNewRecipientsPerDay, def.MaxNewRecipientsPerDay))
	// The window only means something under review; elsewhere it is
	// shown when the grant has a number of its own.
	switch {
	case g.HoldSeconds != nil && (g.Policy == sendpolicy.Review || *g.HoldSeconds > 0):
		s += fmt.Sprintf(", hold %ds", *g.HoldSeconds)
	case g.Policy == sendpolicy.Review:
		s += fmt.Sprintf(", hold %ds (default)", sendpolicy.EffectiveHoldSeconds(nil))
	}
	if g.SuspendedAt != nil {
		s += ", SUSPENDED since " + stamp(g.SuspendedAt)
	}
	return s
}

func (h *host) grantList(ctx context.Context, identity, principal string) error {
	var identityID, principalID string
	if identity != "" {
		i, err := h.grantIdentity(ctx, identity)
		if err != nil {
			return err
		}
		identityID = i.ID
	}
	if principal != "" {
		kind, name, err := parsePrincipal(principal)
		if err != nil {
			return err
		}
		p, err := h.db.PrincipalByName(ctx, kind, name)
		if err != nil {
			return err
		}
		principalID = p.ID
	}
	gs, err := h.db.Grants(ctx, identityID, principalID)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(h.out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "IDENTITY\tPRINCIPAL\tGRANT")
	for _, g := range gs {
		_, _ = fmt.Fprintf(tw, "%s\t%s:%s\t%s\n", shown(g.IdentityAddress), g.PrincipalKind, g.PrincipalName, describeGrant(g, g.PrincipalKind))
	}
	return tw.Flush()
}

func (h *host) grantRemove(ctx context.Context, identity, principal string) error {
	ident, err := h.grantIdentity(ctx, identity)
	if err != nil {
		return err
	}
	kind, name, err := parsePrincipal(principal)
	if err != nil {
		return err
	}
	p, err := h.db.PrincipalByName(ctx, kind, name)
	if err != nil {
		return err
	}
	err = h.db.RemoveGrant(ctx, ident.ID, p.ID, h.action("grant.remove", map[string]any{
		"identity_id": ident.ID, "identity": ident.Address(), "principal_id": p.ID, "principal": principal,
	}))
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%s has no grant on %s", principal, shown(ident.Address()))
	}
	if err != nil {
		return err
	}
	// What applies now is the default, which is not always nothing.
	d := sendpolicy.Effective(sendpolicy.Input{
		Submitter: sendpolicy.Principal{ID: p.ID, Kind: p.Kind},
		Identity:  sendpolicy.Identity{ID: ident.ID, OwnerID: ident.OwnerID, OwnerKind: ident.OwnerKind},
	})
	return h.say("removed the grant of %s on %s; the default applies: %s", principal, shown(ident.Address()), d.Policy)
}
