package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/authn"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

const tokenUsage = "usage: yuuka token issue --principal <kind>:<name> --scope <s1,s2,...> [--label L] [--ttl D] | yuuka token issue --kind smtp --principal <kind>:<name> [--label L] [--ttl D] | yuuka token ls [--principal <kind>:<name>] | yuuka token revoke <credential-id>"

// tokenCmd is `yuuka token`: bearer tokens and SMTP submission passwords
// for agents and services (§13.3, §13.6, §13.7).
func (h *host) tokenCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(tokenUsage)
	}
	switch args[0] {
	case "issue":
		fs := flag.NewFlagSet("token issue", flag.ContinueOnError)
		kind := fs.String("kind", "token", "token (a bearer token for REST and MCP) or smtp (a password for the SMTP submission port)")
		principal := fs.String("principal", "", "agent:<name> or service:<name>; created if it does not exist")
		scopes := fs.String("scope", "", "comma-separated scopes, all of them explicit: "+strings.Join(auth.Strings(auth.AllScopes), ",")+
			"; an smtp password holds submission:send and nothing else")
		label := fs.String("label", "", "what the token is for, e.g. the machine it lives on")
		ttl := fs.Duration("ttl", 0, "lifetime, e.g. 720h; 0 means until revoked")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || *principal == "" {
			return errors.New(tokenUsage)
		}
		switch *kind {
		case "token":
			if *scopes == "" {
				return errors.New(tokenUsage)
			}
			return h.tokenIssue(ctx, auth.KindToken, *principal, *scopes, *label, *ttl)
		case "smtp":
			// The kind names the one thing it can do; a --scope that says
			// anything else is a mistake, not a request for more.
			if *scopes != "" && *scopes != string(auth.SubmissionSend) {
				return fmt.Errorf("--scope %q: an smtp password holds exactly %s", *scopes, auth.SubmissionSend)
			}
			return h.tokenIssue(ctx, auth.KindSMTPPassword, *principal, string(auth.SubmissionSend), *label, *ttl)
		}
		return fmt.Errorf("--kind %q: want token or smtp", *kind)
	case "ls":
		fs := flag.NewFlagSet("token ls", flag.ContinueOnError)
		principal := fs.String("principal", "", "only the tokens of <kind>:<name>")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New(tokenUsage)
		}
		return h.tokenList(ctx, *principal)
	case "revoke":
		fs := flag.NewFlagSet("token revoke", flag.ContinueOnError)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New(tokenUsage)
		}
		return h.tokenRevoke(ctx, fs.Arg(0))
	}
	return errors.New(tokenUsage)
}

// principalName is the CHECK on principals.name, checked here for a
// readable error.
var principalName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func parsePrincipal(s string) (kind, name string, err error) {
	kind, name, ok := strings.Cut(s, ":")
	if !ok || !principalName.MatchString(name) {
		return "", "", fmt.Errorf("principal %q: want <kind>:<name>, e.g. agent:arisu", s)
	}
	return kind, name, nil
}

// parseScopes reads --scope. Every scope is named; there is no default
// and no shorthand, so a token never holds more than was typed. The
// result is in the order of §13.4, without repeats.
func parseScopes(s string) ([]auth.Scope, error) {
	var names []string
	for f := range strings.SplitSeq(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			return nil, fmt.Errorf("--scope %q: empty scope", s)
		}
		names = append(names, f)
	}
	given, err := auth.ParseScopes(names)
	if err != nil {
		return nil, fmt.Errorf("--scope: %w; known: %s", err, strings.Join(auth.Strings(auth.AllScopes), ", "))
	}
	var out []auth.Scope
	for _, sc := range auth.AllScopes {
		if slices.Contains(given, sc) {
			out = append(out, sc)
		}
	}
	return out, nil
}

func (h *host) tokenIssue(ctx context.Context, credKind, principal, scopeList, label string, ttl time.Duration) error {
	kind, name, err := parsePrincipal(principal)
	if err != nil {
		return err
	}
	switch kind {
	case "agent", "service":
	case "human":
		return fmt.Errorf("%s: humans sign in with a passkey (yuuka admin enroll) and never hold tokens", principal)
	default:
		return fmt.Errorf("%s: tokens are for agent and service principals", principal)
	}
	scopes, err := parseScopes(scopeList)
	if err != nil {
		return err
	}
	// Checked again by IssueToken, but before a principal is created.
	if ttl < 0 {
		return fmt.Errorf("--ttl %v: want 0 (until revoked) or a positive duration", ttl)
	}
	p, created, err := h.principal(ctx, kind, name)
	if err != nil {
		return err
	}
	if created {
		h.note("created principal %s:%s", kind, name)
	}
	tok, err := authn.IssueToken(ctx, h.db, h.now(), authn.TokenRequest{
		Kind: credKind, PrincipalID: p.ID, Label: label, Scopes: scopes, TTL: ttl,
		IssuedBy: store.SystemPrincipalID, AuditArgs: h.auditArgs(),
	})
	if err != nil {
		return err
	}
	expires := "never expires"
	if ttl > 0 {
		expires = "expires " + h.now().Add(ttl).UTC().Format(time.RFC3339)
	}
	if credKind == auth.KindSMTPPassword {
		// The user name is the credential id: the port looks the row up
		// by it and compares the password's hash in constant time.
		id, _, _ := auth.ParseSecret(auth.PrefixSMTP, tok)
		h.note("SMTP password for %s:%s, %s. SMTP user name:", kind, name, expires)
		h.note("  %s", id)
		h.note("The password follows; it is shown once and cannot be recovered. Revoke it with `yuuka token revoke %s`.", id)
		h.note("What it may send as is decided by `yuuka grant`, not by the password.")
		return h.say("%s", tok)
	}
	id, _, _ := auth.ParseSecret(auth.PrefixToken, tok)
	h.note("Bearer token %s for %s:%s, scopes %s, %s.", id, kind, name, strings.Join(auth.Strings(scopes), ","), expires)
	h.note("It is shown once and cannot be recovered; revoke it with `yuuka token revoke %s`.", id)
	return h.say("%s", tok)
}

// principal returns kind:name, creating it (and recording that) if it
// does not exist.
func (h *host) principal(ctx context.Context, kind, name string) (store.Principal, bool, error) {
	p, err := h.db.PrincipalByName(ctx, kind, name)
	if err == nil {
		if p.DisabledAt != nil {
			return p, false, fmt.Errorf("principal %s:%s is disabled", kind, name)
		}
		return p, false, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return p, false, err
	}
	now := h.now()
	p = store.Principal{ID: ulid.New(now), Kind: kind, Name: name, DisplayName: name, CreatedAt: now}
	err = h.db.CreatePrincipal(ctx, p, h.action("principal.create", map[string]any{"principal_id": p.ID, "principal": kind + ":" + name}))
	if errors.Is(err, store.ErrDuplicate) {
		// Created concurrently; use that one.
		p, err = h.db.PrincipalByName(ctx, kind, name)
		return p, false, err
	}
	return p, err == nil, err
}

func (h *host) tokenList(ctx context.Context, principal string) error {
	var id string
	if principal != "" {
		kind, name, err := parsePrincipal(principal)
		if err != nil {
			return err
		}
		p, err := h.db.PrincipalByName(ctx, kind, name)
		if err != nil {
			return err
		}
		id = p.ID
	}
	toks, err := h.db.Tokens(ctx, id)
	if err != nil {
		return err
	}
	// Write errors surface on Flush.
	tw := tabwriter.NewWriter(h.out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tKIND\tPRINCIPAL\tLABEL\tSCOPES\tCREATED\tEXPIRES\tLAST USED\tREVOKED")
	for _, t := range toks {
		label := shown(t.Label)
		if label == "" {
			label = "-"
		}
		kind := t.Kind
		if kind == auth.KindSMTPPassword {
			kind = "smtp"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s:%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, kind, t.PrincipalKind, t.PrincipalName, label,
			strings.Join(t.Scopes, ","), stamp(&t.CreatedAt), stamp(t.ExpiresAt), stamp(t.LastUsedAt), stamp(t.RevokedAt))
	}
	return tw.Flush()
}

func (h *host) tokenRevoke(ctx context.Context, id string) error {
	if !ulid.Valid(id) {
		return fmt.Errorf("%q: want a credential id as `yuuka token ls` shows it", id)
	}
	err := authn.RevokeToken(ctx, h.db, id, h.action("auth.credential.revoke", nil))
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no unrevoked token or SMTP password %s", id)
	}
	if err != nil {
		return err
	}
	return h.say("revoked %s", id)
}
