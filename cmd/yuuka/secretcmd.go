package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
)

const secretUsage = "usage: yuuka secret ls | yuuka secret check | yuuka secret rewrap"

// secretCmd is `yuuka secret`: every sealed third-party secret, whoever
// owns it (§17.2, §17.3). None of these prints a secret or anything
// derived from one; there is no way to read one back.
func (h *host) secretCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(secretUsage)
	}
	fs := flag.NewFlagSet("secret "+args[0], flag.ContinueOnError)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(secretUsage)
	}
	switch args[0] {
	case "ls":
		return h.secretList(ctx)
	case "check":
		return h.secretCheck(ctx)
	case "rewrap":
		return h.secretRewrap(ctx)
	}
	return errors.New(secretUsage)
}

// secretOwner is the row that uses a secret: what `secret ls` names, and
// what `secret check` recomputes the audience from. A secret is never
// opened for the audience stored beside it, which is exactly what a
// database writer controls (§17.2).
type secretOwner struct {
	what string // "transport cloudflare"
	kind string // the secrets.kind it must have
	// audience is recomputed from the owning row.
	audience func() (string, error)
}

// secretOwners maps secret ids to their owners. Every table that
// references secrets contributes its rows here, and only here: a new
// owner kind (channels, M3.5's account sources) is one more block below,
// and ls, check and rewrap follow.
func (h *host) secretOwners(ctx context.Context) (map[string]secretOwner, error) {
	out := map[string]secretOwner{}
	trs, err := h.db.Transports(ctx, true)
	if err != nil {
		return nil, err
	}
	for _, t := range trs {
		if t.SecretID == "" {
			continue
		}
		out[t.SecretID] = secretOwner{
			what:     "transport " + t.Name,
			kind:     store.SecretSMTPPassword,
			audience: func() (string, error) { return secret.SMTPS(t.Username, t.Host, t.Port) },
		}
	}
	chs, err := h.db.Channels(ctx, true)
	if err != nil {
		return nil, err
	}
	for _, c := range chs {
		if c.SecretID == "" {
			continue
		}
		out[c.SecretID] = secretOwner{
			what:     "channel " + c.Name,
			kind:     store.SecretTelegramBotToken,
			audience: c.Audience, // telegram:bot<id>, from the row
		}
	}
	return out, nil
}

func (h *host) secretList(ctx context.Context) error {
	secs, err := h.db.Secrets(ctx)
	if err != nil {
		return err
	}
	owners, err := h.secretOwners(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(h.out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tKIND\tOWNER\tAUDIENCE\tWRAPPER\tKEY\tSEALED")
	for _, s := range secs {
		owner := "- (no owner)"
		if o, ok := owners[s.ID]; ok {
			owner = o.what
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Kind, shown(owner), shown(s.Audience),
			s.Wrapper, s.KeyRef, s.SealedAt.UTC().Format(time.RFC3339))
	}
	return tw.Flush()
}

// secretCheck opens every secret for the audience recomputed from its
// owner, as sending would, and reports only the outcome. Opening without
// a key configured reports every secret locked. It fails when any secret
// is not ok, so it can run from a script.
func (h *host) secretCheck(ctx context.Context) error {
	s, err := h.sealer()
	if err != nil {
		return err
	}
	if s == nil {
		h.note("YUUKA_KEK_FILE is not set: every secret is locked.")
	}
	secs, err := h.db.Secrets(ctx)
	if err != nil {
		return err
	}
	owners, err := h.secretOwners(ctx)
	if err != nil {
		return err
	}
	bad := 0
	tw := tabwriter.NewWriter(h.out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tKIND\tOWNER\tKEY\tRESULT")
	for _, sec := range secs {
		o, ok := owners[sec.ID]
		var res string
		switch {
		case !ok:
			res = "no owner: nothing uses it, so there is no destination to check it against"
		case sec.Kind != o.kind:
			res = fmt.Sprintf("wrong kind: %s, but its owner needs %s", sec.Kind, o.kind)
		default:
			res = checkOpen(ctx, s, sec.Record, o.audience)
		}
		if res != "ok" {
			bad++
		}
		owner := "-"
		if ok {
			owner = o.what
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", sec.ID, sec.Kind, shown(owner), sec.Wrapper+":"+sec.KeyRef, res)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if bad > 0 {
		return fmt.Errorf("%d of %d secrets are not ok", bad, len(secs))
	}
	return nil
}

// checkOpen opens r for the audience its owner gives and says how that
// went. The plaintext is wiped at once and never leaves this function.
func checkOpen(ctx context.Context, s *secret.Sealer, r secret.Record, audience func() (string, error)) string {
	aud, err := audience()
	if err != nil {
		return "audience mismatch: its owner names no valid destination (" + err.Error() + ")"
	}
	v, err := s.Open(ctx, r, aud)
	if err != nil {
		return openFailure(err)
	}
	v.Close()
	return "ok"
}

// secretRewrap moves every secret's data key to the current wrapper and
// key, for a KEK rotation (§17.3) or, later, a move to another wrapper.
// Payloads are not touched. A secret whose key is already current is left
// alone; each one moved is written at the revision read and recorded.
func (h *host) secretRewrap(ctx context.Context) error {
	s, err := h.sealer()
	if err != nil {
		return err
	}
	if s == nil {
		return errors.New("YUUKA_KEK_FILE is not set: there is no key to rewrap under; see docs/operate/secrets.md")
	}
	secs, err := h.db.Secrets(ctx)
	if err != nil {
		return err
	}
	owners, err := h.secretOwners(ctx)
	if err != nil {
		return err
	}
	var moved, current, failed int
	var key string
	for _, sec := range secs {
		// Rewrapping a current secret wraps its DEK again under the same
		// key; comparing the result is how one learns it was current,
		// whatever the wrapper.
		out, err := s.Rewrap(ctx, sec.Record)
		if err != nil {
			failed++
			h.note("%s: %s", sec.ID, openFailure(err))
			continue
		}
		key = out.Wrapper + ":" + out.KeyRef
		if out.Wrapper == sec.Wrapper && out.KeyRef == sec.KeyRef {
			current++
			continue
		}
		args := map[string]any{
			"secret_id": sec.ID, "kind": sec.Kind, "audience": sec.Audience,
			"wrapper": out.Wrapper, "key_ref": out.KeyRef, "from_wrapper": sec.Wrapper, "from_key_ref": sec.KeyRef,
			"revision": sec.Revision,
		}
		if o, ok := owners[sec.ID]; ok {
			args["owner"] = o.what
		}
		_, err = h.db.UpdateSecret(ctx, out, sec.Revision, h.action("secret.rewrap", args))
		if errors.Is(err, store.ErrStale) || errors.Is(err, store.ErrNotFound) {
			failed++
			h.note("%s: changed while this command ran, and was not written; run it again", sec.ID)
			continue
		}
		if err != nil {
			return err
		}
		moved++
		h.note("%s: %s:%s -> %s", sec.ID, sec.Wrapper, sec.KeyRef, key)
	}
	if err := h.say("rewrapped %d, already current %d, failed %d", moved, current, failed); err != nil {
		return err
	}
	if failed > 0 {
		return fmt.Errorf("%d secrets were not rewrapped", failed)
	}
	if key != "" {
		h.note("Every secret is under %s now; KEK files other than the current one can go.", key)
	}
	return nil
}
