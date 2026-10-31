package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/yuuka-mail/yuuka/internal/rules"
	"github.com/yuuka-mail/yuuka/internal/store"
)

const ruleUsage = "usage: yuuka rule ls [--all] | yuuka rule show <id> | " +
	"yuuka rule add --spec <file>|-|<json> [--name N] [--priority P] [--disabled] | " +
	"yuuka rule set <id> [--spec <file>|-|<json>] [--name N] [--priority P] [--enable|--disable] | " +
	"yuuka rule rm <id> | yuuka rule apply <id> [--since T] [--dry-run] | yuuka rule revert <id> [--since T]"

// ruleCmd is `yuuka rule`: the rules that annotate mail as it arrives
// (§8.5). The format is internal/rules'; the store validates it.
func (h *host) ruleCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(ruleUsage)
	}
	switch args[0] {
	case "ls":
		fs := flag.NewFlagSet("rule ls", flag.ContinueOnError)
		all := fs.Bool("all", false, "include deleted rules")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New(ruleUsage)
		}
		return h.ruleList(ctx, *all)
	case "show", "rm":
		fs := flag.NewFlagSet("rule "+args[0], flag.ContinueOnError)
		id, err := idAndFlags(fs, args[1:])
		if err != nil {
			return err
		}
		if args[0] == "show" {
			return h.ruleShow(ctx, id)
		}
		return h.ruleRemove(ctx, id)
	case "add":
		fs := flag.NewFlagSet("rule add", flag.ContinueOnError)
		spec := fs.String("spec", "", `the rule, {"v":1,"match":…,"actions":…}: a file, - for stdin, or the JSON itself`)
		name := fs.String("name", "", "a name for people")
		priority := fs.Int("priority", 0, "evaluation order, lowest first; ties by id")
		disabled := fs.Bool("disabled", false, "store it without running it")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || *spec == "" {
			return errors.New(ruleUsage)
		}
		b, err := h.readSpec(*spec)
		if err != nil {
			return err
		}
		return h.ruleAdd(ctx, store.NewRule{Name: *name, Priority: *priority, Spec: b, Disabled: *disabled})
	case "set":
		fs := flag.NewFlagSet("rule set", flag.ContinueOnError)
		spec := fs.String("spec", "", "a new rule: a file, - for stdin, or the JSON itself")
		name := fs.String("name", "", "a new name")
		priority := fs.Int("priority", 0, "a new priority")
		enable := fs.Bool("enable", false, "run it again")
		disable := fs.Bool("disable", false, "stop running it; its past effects stay")
		id, err := idAndFlags(fs, args[1:])
		if err != nil {
			return err
		}
		given := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
		if len(given) == 0 || *enable && *disable {
			return errors.New(ruleUsage)
		}
		var u store.RuleUpdate
		if given["spec"] {
			if u.Spec, err = h.readSpec(*spec); err != nil {
				return err
			}
		}
		if given["name"] {
			u.Name = name
		}
		if given["priority"] {
			u.Priority = priority
		}
		if *enable || *disable {
			on := *enable
			u.Enabled = &on
		}
		return h.ruleSet(ctx, id, u)
	case "apply", "revert":
		fs := flag.NewFlagSet("rule "+args[0], flag.ContinueOnError)
		since := fs.String("since", "", "only mail received at or after this time (RFC 3339 or YYYY-MM-DD; default all)")
		var dry *bool
		if args[0] == "apply" {
			dry = fs.Bool("dry-run", false, "count what it would change; write nothing")
		}
		id, err := idAndFlags(fs, args[1:])
		if err != nil {
			return err
		}
		t, err := parseSince(*since)
		if err != nil {
			return err
		}
		if args[0] == "revert" {
			return h.ruleRevert(ctx, id, t)
		}
		return h.ruleApply(ctx, id, t, *dry)
	}
	return errors.New(ruleUsage)
}

// idAndFlags parses a rule id and fs's flags, in either order. The id
// may be written rule:<id> and in any case.
func idAndFlags(fs *flag.FlagSet, args []string) (string, error) {
	id := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	} else if id == "" || fs.NArg() != 0 {
		return "", errors.New(ruleUsage)
	}
	return strings.ToUpper(strings.TrimPrefix(id, "rule:")), nil
}

// readSpec reads --spec: JSON as given, - for stdin, else a file.
func (h *host) readSpec(s string) ([]byte, error) {
	switch {
	case strings.HasPrefix(strings.TrimSpace(s), "{"):
		return []byte(s), nil
	case s == "-":
		in := h.in
		if in == nil {
			in = os.Stdin
		}
		return io.ReadAll(io.LimitReader(in, 1<<20))
	}
	return os.ReadFile(s)
}

// ruleState is how lists show a rule.
func ruleState(r store.Rule) string {
	switch {
	case r.DeletedAt != nil:
		return "deleted"
	case len(r.Unsupported) > 0:
		return "unsupported"
	case r.Enabled:
		return "enabled"
	}
	return "disabled"
}

// actionList is a rule's actions in short: label:shop,archive.
func actionList(as []rules.Action) string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Kind
		if p := a.Label + a.Class + a.Level; p != "" {
			out[i] += ":" + p
		}
	}
	return strings.Join(out, ",")
}

func (h *host) ruleList(ctx context.Context, all bool) error {
	rs, err := h.db.Rules(ctx, all)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(h.out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tPRIORITY\tSTATE\tREVISION\tNAME\tACTIONS")
	for _, r := range rs {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%d\t%s\t%s\n", r.ID, r.Priority, ruleState(r), r.Revision,
			shown(r.Name), shown(actionList(r.Spec.Actions)))
	}
	return tw.Flush()
}

func (h *host) ruleShow(ctx context.Context, id string) error {
	r, err := h.db.RuleByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no rule %s", id)
	}
	if err != nil {
		return err
	}
	spec, err := json.Marshal(r.Spec)
	if err != nil {
		return err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, spec, "", "  "); err != nil {
		return err
	}
	lines := []string{
		"id:        " + r.ID,
		"principal: rule:" + r.ID,
		"name:      " + shown(r.Name),
		fmt.Sprintf("priority:  %d", r.Priority),
		"state:     " + ruleState(r),
		fmt.Sprintf("revision:  %d", r.Revision),
		"created:   " + r.CreatedAt.UTC().Format(time.RFC3339),
		"updated:   " + r.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if r.ProposalID != "" {
		lines = append(lines, "proposal:  "+r.ProposalID)
	}
	for _, u := range r.Unsupported {
		lines = append(lines, "unsupported: "+shown(u))
	}
	lines = append(lines, "", pretty.String())
	return h.say("%s", strings.Join(lines, "\n"))
}

func (h *host) warn(warns []rules.Warning) {
	for _, w := range warns {
		h.note("warning (%s): %s", w.Code, w.Message)
	}
}

func (h *host) ruleAdd(ctx context.Context, n store.NewRule) error {
	r, warns, err := h.db.CreateRule(ctx, n, h.action("rule.add", map[string]any{"name": n.Name, "priority": n.Priority, "disabled": n.Disabled}))
	if err != nil {
		return err
	}
	h.warn(warns)
	if err := h.say("added rule %s (%s)", r.ID, ruleState(r)); err != nil {
		return err
	}
	h.note("It runs on mail that arrives from now on; `yuuka rule apply %s --dry-run` shows what it would do to past mail.", r.ID)
	return nil
}

func (h *host) ruleSet(ctx context.Context, id string, u store.RuleUpdate) error {
	args := map[string]any{"spec": u.Spec != nil}
	if u.Name != nil {
		args["name"] = *u.Name
	}
	if u.Priority != nil {
		args["priority"] = *u.Priority
	}
	if u.Enabled != nil {
		args["enabled"] = *u.Enabled
	}
	r, warns, err := h.db.UpdateRule(ctx, id, u, h.action("rule.set", args))
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no rule %s (deleted rules cannot be changed)", id)
	}
	if err != nil {
		return err
	}
	h.warn(warns)
	return h.say("rule %s is at revision %d (%s)", r.ID, r.Revision, ruleState(r))
}

func (h *host) ruleRemove(ctx context.Context, id string) error {
	err := h.db.DeleteRule(ctx, id, h.action("rule.rm", nil))
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no rule %s, or it is deleted already", id)
	}
	if err != nil {
		return err
	}
	if err := h.say("deleted rule %s", id); err != nil {
		return err
	}
	h.note("What it did stays; `yuuka rule revert %s` takes it back.", id)
	return nil
}

func (h *host) ruleApply(ctx context.Context, id string, since time.Time, dry bool) error {
	st, err := h.db.ApplyRule(ctx, id, since, dry, h.action("rule.apply", nil))
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no rule %s, or it is deleted", id)
	}
	if err != nil {
		return err
	}
	verb := "changed"
	if dry {
		verb = "would change"
	}
	if err := h.say("%d messages looked at, %d matched, %s %d", st.Messages, st.Matched, verb, st.Changed); err != nil {
		return err
	}
	if st.Overruled > 0 {
		h.note("%d kept what a person or an agent decided.", st.Overruled)
	}
	if st.Failed > 0 {
		h.note("%d could not be evaluated in time and did not match.", st.Failed)
	}
	return nil
}

func (h *host) ruleRevert(ctx context.Context, id string, since time.Time) error {
	st, err := h.db.RevertRule(ctx, id, since, h.action("rule.revert", nil))
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no rule %s", id)
	}
	if err != nil {
		return err
	}
	if err := h.say("%d messages, %d changes reverted", st.Messages, st.Reverted); err != nil {
		return err
	}
	if st.Superseded > 0 {
		h.note("%d were changed again since and stay as they are.", st.Superseded)
	}
	return nil
}
