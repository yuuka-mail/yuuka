package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"text/tabwriter"

	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

const identityUsage = "usage: yuuka identity add <local>@<zone>|*@<zone> --owner <kind>:<name> --transport NAME [--display-name N] [--signature S] [--plain-strip-markup] [--account A] | yuuka identity ls [--all] | yuuka identity rm <local>@<zone>|*@<zone>"

// identityCmd is `yuuka identity`: the addresses yuuka may send as, the
// transport each goes out through and who owns it (§9.3, §8.9).
func (h *host) identityCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(identityUsage)
	}
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("identity add", flag.ContinueOnError)
		owner := fs.String("owner", "", "who the address belongs to: human:<name> (who must exist), agent:<name> or service:<name>. "+
			"Only a human owner can send as a human-owned identity; everyone else stops at a draft (§8.9)")
		transport := fs.String("transport", "", "name of the transport in the process configuration that carries this identity's mail")
		display := fs.String("display-name", "", "display name in From; it replaces whatever an SMTP client writes")
		signature := fs.String("signature", "", "Markdown appended to composed mail")
		strip := fs.Bool("plain-strip-markup", false, "render the text/plain part without Markdown markup")
		account := fs.String("account", "", "account the address belongs to (default: the only account)")
		if err := parseInterspersed(fs, args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 || *owner == "" || *transport == "" {
			return errors.New(identityUsage)
		}
		return h.identityAdd(ctx, fs.Arg(0), store.Identity{
			DisplayName: *display, Signature: *signature, PlainStripMarkup: *strip, Transport: *transport,
		}, *owner, *account)
	case "ls":
		fs := flag.NewFlagSet("identity ls", flag.ContinueOnError)
		all := fs.Bool("all", false, "include disabled identities")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New(identityUsage)
		}
		return h.identityList(ctx, *all)
	case "rm":
		fs := flag.NewFlagSet("identity rm", flag.ContinueOnError)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New(identityUsage)
		}
		return h.identityRemove(ctx, fs.Arg(0))
	}
	return errors.New(identityUsage)
}

// parseInterspersed parses flags that may come after the one positional
// argument too (`identity add a@example.com --owner ...`), which the flag
// package alone stops at.
func parseInterspersed(fs *flag.FlagSet, args []string) error {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	return fs.Parse(append([]string{"--"}, pos...))
}

// grantee returns the principal named kind:name for an identity owner or
// a grant (store.Grantee): an agent or service is created on first use,
// as `token issue` does; a human must exist.
func (h *host) grantee(ctx context.Context, s string) (store.Principal, error) {
	kind, name, err := parsePrincipal(s)
	if err != nil {
		return store.Principal{}, err
	}
	p, created, err := h.principal(ctx, kind, name)
	if created {
		h.note("created principal %s:%s", kind, name)
	}
	return p, err
}

func (h *host) identityAdd(ctx context.Context, addr string, i store.Identity, owner, account string) error {
	zone, prefix, err := store.SplitIdentityAddress(addr)
	if err != nil {
		return err
	}
	if !store.ValidName(i.Transport) {
		return fmt.Errorf("--transport %q: want a name like the ones in the configuration ([A-Za-z0-9][A-Za-z0-9._-]*)", i.Transport)
	}
	accts, err := h.db.Accounts(ctx)
	if err != nil {
		return err
	}
	switch {
	case account != "":
		for _, a := range accts {
			if a.Name == account {
				i.AccountID = a.ID
			}
		}
		if i.AccountID == "" {
			return fmt.Errorf("--account %q: no such account", account)
		}
	case len(accts) == 1:
		i.AccountID, account = accts[0].ID, accts[0].Name
	case len(accts) == 0:
		return errors.New("no account exists yet; the Cloudflare account (YUUKA_ACCOUNT) is created when yuuka serve first ingests mail")
	default:
		return fmt.Errorf("%d accounts exist; choose one with --account", len(accts))
	}
	p, err := h.grantee(ctx, owner)
	if err != nil {
		return err
	}
	now := h.now()
	i.ID, i.Zone, i.Prefix, i.OwnerID, i.CreatedAt = ulid.New(now), zone, prefix, p.ID, now
	err = h.db.AddIdentity(ctx, i, h.action("identity.add", map[string]any{
		"identity_id": i.ID, "address": i.Address(), "account": account, "owner": p.Kind + ":" + p.Name, "transport": i.Transport,
	}))
	if errors.Is(err, store.ErrDuplicate) {
		return fmt.Errorf("%s is already an identity; `yuuka identity rm` it first", i.Address())
	}
	if err != nil {
		return err
	}
	if prefix == "" {
		h.note("Every address @%s without an identity of its own now sends as this one.", zone)
	}
	if p.Kind == "human" {
		h.note("Owned by %s:%s, who sends as it without a grant; anyone else stops at a draft.", p.Kind, p.Name)
	} else {
		h.note("Owned by %s:%s; without a grant it holds its own mail for review (`yuuka grant set` to change).", p.Kind, p.Name)
	}
	return h.say("added %s (%s)", shown(i.Address()), i.ID)
}

func (h *host) identityList(ctx context.Context, all bool) error {
	ids, err := h.db.Identities(ctx, all)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(h.out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ADDRESS\tOWNER\tTRANSPORT\tDISPLAY NAME\tID\tDISABLED")
	for _, i := range ids {
		name := shown(i.DisplayName)
		if name == "" {
			name = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s:%s\t%s\t%s\t%s\t%s\n", shown(i.Address()), i.OwnerKind, i.OwnerName,
			i.Transport, name, i.ID, stamp(i.DisabledAt))
	}
	return tw.Flush()
}

func (h *host) identityRemove(ctx context.Context, addr string) error {
	zone, prefix, err := store.SplitIdentityAddress(addr)
	if err != nil {
		return err
	}
	i, err := h.db.IdentityAt(ctx, zone, prefix)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%s is not an enabled identity", shown(addr))
	}
	if err != nil {
		return err
	}
	err = h.db.DisableIdentity(ctx, i.ID, h.action("identity.disable", map[string]any{"identity_id": i.ID, "address": i.Address()}))
	if err != nil {
		return err
	}
	if prefix != "" {
		h.note("Mail from %s now resolves to *@%s if that identity exists.", i.Address(), zone)
	}
	return h.say("disabled %s (%s); its drafts, outbox rows and grants keep pointing at it", shown(i.Address()), i.ID)
}
