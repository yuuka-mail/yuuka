package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/yuuka-mail/yuuka/internal/imap"
	"github.com/yuuka-mail/yuuka/internal/manage"
	"github.com/yuuka-mail/yuuka/internal/notify"
	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
)

const accountUsage = "usage: yuuka account add NAME --imap [--preset gmail] --address A [--username U] --password-file PATH|- " +
	"[--host H] [--port 993] [--trusted-authserv-id ID] [--backfill all|none|DAYS] [--mailboxes A,B] " +
	"[--max-message-size 32MiB] [--daily-budget 2GiB|0] [--poll 60s] [--read <kind>:<name>,...] | " +
	"yuuka account set NAME [--address A] [--username U] [--password-file PATH|-] [--host H] [--port P] " +
	"[--trusted-authserv-id ID] [--backfill B] [--mailboxes A,B] [--max-message-size S] [--daily-budget S] [--poll D] | " +
	"yuuka account ls [--all] | yuuka account test NAME | yuuka account sync NAME [--retry-failed] | " +
	"yuuka account pause|resume|rm NAME | " +
	"yuuka account access NAME [--principal <kind>:<name> --read|--no-read|--default] [--default-read yes|no]"

// testTimeout bounds `account test` as a whole: connect, login, LIST and
// a STATUS per folder.
const testTimeout = 2 * time.Minute

// accountCmd is `yuuka account`: the accounts mail comes in through, the
// IMAP sources of pull accounts with their sealed passwords, and who may
// read each account (§9.5, §13.8, §17.8). Adding a source, giving it a new
// password or destination, resuming it, letting it pull more and letting
// someone read it widen: they are recorded and announced as security
// alerts. Sealing or opening a password needs YUUKA_KEK_FILE; the rest
// does not.
func (h *host) accountCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(accountUsage)
	}
	if err := refuseSecretArgs(args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "add", "set":
		add := args[0] == "add"
		fs := flag.NewFlagSet("account "+args[0], flag.ContinueOnError)
		// Literal flag names, one call each: docs_test.go finds them this way.
		var imapFlag *bool
		var preset, read *string
		if add {
			imapFlag = fs.Bool("imap", false, "pull the mailbox over IMAP (the only kind so far)")
			preset = fs.String("preset", "", "gmail: "+manage.GmailHost+", \\All only, a daily budget of 2GiB")
			read = fs.String("read", "", "comma-separated <kind>:<name> who may read the account (default: the only human)")
		}
		address := fs.String("address", "", "the mailbox's own address; its mail is delivered to it and it is claimed as a prefix")
		username := fs.String("username", "", "IMAP user name (default: the address)")
		passwordFile := fs.String("password-file", "", "file holding the password, or - for standard input; never the password itself")
		host := fs.String("host", "", "IMAP server; implicit TLS only (default with --preset gmail: "+manage.GmailHost+")")
		port := fs.Int("port", manage.IMAPPort, "port")
		authservID := fs.String("trusted-authserv-id", "", "whose Authentication-Results this mail trusts; empty trusts none")
		backfill := fs.String("backfill", "all", "how far back the first sync goes: all, none, or a number of days")
		mailboxes := fs.String("mailboxes", "", "comma-separated folders to sync (default: \\All with gmail, else all but junk, trash and drafts)")
		maxSize := fs.String("max-message-size", formatSize(manage.MaxMessageBytes), "larger mail is kept header only")
		budget := fs.String("daily-budget", "", "bytes fetched per day, e.g. 2GiB; 0 for no limit (default: 2GiB with gmail, else 0)")
		poll := fs.Duration("poll", manage.DefaultPollSeconds*time.Second, "how often to look for new mail")
		if err := parseInterspersed(fs, args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New(accountUsage)
		}
		f := sourceFlags{
			address: *address, username: *username, passwordFile: *passwordFile, host: *host, port: *port,
			authservID: *authservID, backfill: *backfill, mailboxes: *mailboxes, maxSize: *maxSize, budget: *budget, poll: *poll,
		}
		if add {
			f.imap, f.preset, f.read = *imapFlag, *preset, *read
		}
		f.given = map[string]bool{}
		fs.Visit(func(fl *flag.Flag) { f.given[fl.Name] = true })
		if add {
			return h.accountAdd(ctx, fs.Arg(0), f)
		}
		return h.accountSet(ctx, fs.Arg(0), f)
	case "ls":
		fs := flag.NewFlagSet("account ls", flag.ContinueOnError)
		all := fs.Bool("all", false, "include removed accounts")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New(accountUsage)
		}
		return h.accountList(ctx, *all)
	case "sync":
		fs := flag.NewFlagSet("account sync", flag.ContinueOnError)
		retry := fs.Bool("retry-failed", false, "also fetch the messages that failed before")
		if err := parseInterspersed(fs, args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New(accountUsage)
		}
		return h.accountSync(ctx, fs.Arg(0), *retry)
	case "test", "pause", "resume", "rm":
		fs := flag.NewFlagSet("account "+args[0], flag.ContinueOnError)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New(accountUsage)
		}
		return h.accountSource(ctx, args[0], fs.Arg(0))
	case "access":
		fs := flag.NewFlagSet("account access", flag.ContinueOnError)
		principal := fs.String("principal", "", "<kind>:<name> whose access changes")
		read := fs.Bool("read", false, "the principal may read the account")
		noRead := fs.Bool("no-read", false, "the principal may not read the account")
		def := fs.Bool("default", false, "the principal gets the account's default again")
		defaultRead := fs.String("default-read", "", "yes or no: whether principals without a setting of their own may read it")
		if err := parseInterspersed(fs, args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New(accountUsage)
		}
		return h.accountAccess(ctx, fs.Arg(0), *principal, *read, *noRead, *def, *defaultRead)
	}
	return errors.New(accountUsage)
}

type sourceFlags struct {
	imap                                              bool
	preset, read                                      string
	address, username, passwordFile, host, authservID string
	backfill, mailboxes, maxSize, budget              string
	port                                              int
	poll                                              time.Duration
	// given are the flags on the command line; set changes only those.
	given map[string]bool
}

// apply writes the given flags over c. now dates --backfill DAYS.
func (f sourceFlags) apply(c *store.SourceConfig, now time.Time) error {
	if f.given["address"] {
		c.Address = f.address
	}
	if f.given["username"] {
		c.Username = f.username
	}
	if f.given["host"] {
		h, err := secret.CanonicalHost(f.host)
		if err != nil {
			return fmt.Errorf("--host: %w", err)
		}
		c.Host = h
	}
	if f.given["port"] {
		c.Port = f.port
	}
	if f.given["trusted-authserv-id"] {
		c.TrustedAuthservID = f.authservID
	}
	if f.given["backfill"] {
		b, err := parseBackfill(f.backfill, now)
		if err != nil {
			return err
		}
		c.BackfillSince = b
	}
	if f.given["mailboxes"] {
		// Empty goes back to the default; a folder name may hold spaces,
		// so only the list's own whitespace is trimmed.
		c.Mailboxes = splitList(f.mailboxes)
	}
	if f.given["max-message-size"] {
		n, err := parseSize(f.maxSize)
		if err != nil {
			return fmt.Errorf("--max-message-size: %w", err)
		}
		c.MaxMessageBytes = n
	}
	if f.given["daily-budget"] {
		n, err := parseSize(f.budget)
		if err != nil {
			return fmt.Errorf("--daily-budget: %w", err)
		}
		c.DailyByteBudget = n
	}
	if f.given["poll"] {
		if f.poll%time.Second != 0 {
			return fmt.Errorf("--poll %s: whole seconds only", f.poll)
		}
		c.PollSeconds = int(f.poll / time.Second)
	}
	return nil
}

// parseBackfill reads --backfill: all (nil), none (now), or a number of
// days back from now.
func parseBackfill(s string, now time.Time) (*time.Time, error) {
	switch s {
	case "all":
		return nil, nil
	case "none":
		return &now, nil
	}
	days, err := strconv.Atoi(s)
	if err != nil || days < 1 || days > 100*366 {
		return nil, fmt.Errorf("--backfill %q: want all, none, or a number of days", s)
	}
	t := now.AddDate(0, 0, -days)
	return &t, nil
}

var sizeUnits = []struct {
	name string
	n    int64
}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}}

// parseSize reads a byte count with an optional binary unit: 0, 4096,
// 512KiB, 32MiB, 2GiB.
func parseSize(s string) (int64, error) {
	num, mult := s, int64(1)
	for _, u := range sizeUnits {
		if v, ok := strings.CutSuffix(s, u.name); ok {
			num, mult = v, u.n
			break
		}
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 0 || n > (1<<50)/mult {
		return 0, fmt.Errorf("%q: want a number of bytes, optionally with KiB, MiB or GiB", s)
	}
	return n * mult, nil
}

// formatSize is n in the largest binary unit that divides it.
func formatSize(n int64) string {
	for _, u := range sizeUnits {
		if n != 0 && n%u.n == 0 {
			return strconv.FormatInt(n/u.n, 10) + u.name
		}
	}
	return strconv.FormatInt(n, 10)
}

// principals resolves a comma-separated list of kind:name, each an
// enabled principal that exists.
func (h *host) principals(ctx context.Context, list string) ([]store.Principal, error) {
	var out []store.Principal
	for _, s := range splitList(list) {
		kind, name, err := parsePrincipal(s)
		if err != nil {
			return nil, err
		}
		p, err := h.db.PrincipalByName(ctx, kind, name)
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("--read: no principal %s", s)
		}
		if err != nil {
			return nil, err
		}
		if p.DisabledAt != nil {
			return nil, fmt.Errorf("--read: %s is disabled", s)
		}
		if !slices.ContainsFunc(out, func(q store.Principal) bool { return q.ID == p.ID }) {
			out = append(out, p)
		}
	}
	return out, nil
}

// accountAdd is `account add`: a new pull account, its password sealed,
// readable by --read (or the only human), its address claimed. Nothing is
// pulled here; a running serve starts on it.
func (h *host) accountAdd(ctx context.Context, name string, f sourceFlags) error {
	if !f.imap {
		return errors.New("account add: say what kind of account it is: --imap (the only kind so far)")
	}
	if f.preset != "" && f.preset != store.PresetGmail {
		return fmt.Errorf("--preset %q: want %s or none", f.preset, store.PresetGmail)
	}
	if !f.given["address"] {
		return errors.New("--address is required: the mailbox's own address")
	}
	if !f.given["host"] && f.preset == "" {
		return errors.New("--host is required without a --preset")
	}
	if f.passwordFile == "" {
		return errors.New("--password-file PATH, or - to read it from standard input, is required")
	}
	now := h.now()
	c := manage.NewSourceConfig(f.preset)
	if err := f.apply(&c, now); err != nil {
		return err
	}
	if !f.given["username"] {
		// As the address is stored; a server that cares about case is
		// given --username.
		c.Username = strings.ToLower(c.Address)
	}
	c, err := manage.CheckSourceConfig(c)
	if err != nil {
		return err
	}
	readers, err := h.readers(ctx, f.read)
	if err != nil {
		return err
	}
	sealer, err := h.sealer()
	if err != nil {
		return err
	}
	if sealer == nil {
		return errors.New("YUUKA_KEK_FILE is not set, so the password cannot be sealed; see docs/operate/secrets.md")
	}
	pw, err := h.readSecret(f.passwordFile)
	if err != nil {
		return err
	}
	src, sealed, err := manage.AddAccount(ctx, h.db, sealer, manage.AccountAdd{Name: name, Config: c, Password: pw, Reader: readers[0].ID}, h.action, now)
	clear(pw)
	switch {
	case errors.Is(err, store.ErrDuplicate):
		return fmt.Errorf("account %s was not added: the name is taken (a removed account keeps its name), "+
			"or another account already pulls %s; see `yuuka account ls --all`", name, shown(c.Address))
	case err != nil:
		return err
	}
	h.note("Password sealed for %s under %s key %s.", sealed.Audience, sealed.Wrapper, sealed.KeyRef)
	h.claimNote(ctx, src)
	var who []string
	for _, p := range readers {
		who = append(who, p.Kind+":"+p.Name)
	}
	if len(readers) > 1 {
		acct, err := h.db.AccountByName(ctx, name)
		if err != nil {
			return err
		}
		rows := []store.AccessRow{{PrincipalID: readers[0].ID, CanRead: true}}
		yes := true
		for _, p := range readers[1:] {
			if _, err := manage.SetAccess(ctx, h.db, acct, rows, p, &yes, h.action); err != nil {
				return fmt.Errorf("account %s was added, but %s:%s may not read it yet: %w", name, p.Kind, p.Name, err)
			}
		}
	}
	h.alertWidening(ctx, notify.Alert{Text: fmt.Sprintf("yuuka: pull account %s (%s) was added %s: %s, readable by %s.",
		name, src.Address, h.by(), manage.SourceDestination(src.SourceConfig), strings.Join(who, ", "))})
	if c.TrustedAuthservID == "" {
		h.note("No --trusted-authserv-id: every Authentication-Results header of this mail counts as unverified, " +
			"so sender checks report nothing. See docs/operate/accounts.md for setting it.")
	}
	h.note("Check the login without pulling anything: yuuka account test %s", name)
	h.note("A running `yuuka serve` starts pulling it; `yuuka account ls` shows how far it got.")
	return h.say("added pull account %s (%s): %s, readable by %s", name, src.AccountID,
		shown(manage.SourceDestination(src.SourceConfig)), strings.Join(who, ", "))
}

// readers are who may read a new account: --read, else the only human.
// The first is whoever "adds" it (store.AddPullAccount).
func (h *host) readers(ctx context.Context, list string) ([]store.Principal, error) {
	if list != "" {
		ps, err := h.principals(ctx, list)
		if err == nil && len(ps) == 0 {
			err = errors.New("--read: name at least one <kind>:<name>")
		}
		return ps, err
	}
	hs, err := h.db.PrincipalsOfKind(ctx, "human")
	if err != nil {
		return nil, err
	}
	if len(hs) != 1 {
		return nil, fmt.Errorf("%d humans: say who may read the account with --read human:<name>", len(hs))
	}
	return hs, nil
}

// claimNote claims the source's address (manage.ClaimAddress) and says
// what that did. A failure is reported, not returned: the account is
// there either way, and `prefix claim` finishes the job.
func (h *host) claimNote(ctx context.Context, src store.Source) {
	addr := shown(src.Address)
	got, err := manage.ClaimAddress(ctx, h.db, src, h.action)
	switch {
	case err != nil:
		h.note("%s could not be claimed (%v); until it is, its mail is downgraded: yuuka prefix claim %s", addr, err, addr)
	case got == store.PrefixUnchanged:
		h.note("%s was claimed already.", addr)
	default:
		h.note("Claimed %s, so its mail is shown and notifies like mail to your own addresses.", addr)
	}
}

// enabledSource reads the enabled source of the pull account name.
func (h *host) enabledSource(ctx context.Context, name string) (store.Source, error) {
	cur, err := h.db.SourceByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		if _, aerr := h.db.AccountByName(ctx, name); aerr == nil {
			return cur, fmt.Errorf("account %s is not a pull account; it has no source to change", name)
		}
		return cur, fmt.Errorf("no account %s; `yuuka account ls` lists them", name)
	}
	if err != nil {
		return cur, err
	}
	if cur.DisabledAt != nil {
		return cur, fmt.Errorf("account %s is removed; its name is not reused", name)
	}
	return cur, nil
}

// accountSet is `account set`: the given flags over the source as read,
// written at the revision read. Another destination needs the password
// again.
func (h *host) accountSet(ctx context.Context, name string, f sourceFlags) error {
	if len(f.given) == 0 {
		return errors.New("account set: nothing to change; " + accountUsage)
	}
	cur, err := h.enabledSource(ctx, name)
	if err != nil {
		return err
	}
	now := h.now()
	c := cur.SourceConfig
	if err := f.apply(&c, now); err != nil {
		return err
	}
	if c, err = manage.CheckSourceConfig(c); err != nil {
		return err
	}
	if f.passwordFile == "" && manage.SourceMoves(cur.SourceConfig, c) {
		return fmt.Errorf("this moves account %s to %s: a password is only ever sent where it was set for, "+
			"so give it again with --password-file PATH or -", name, shown(manage.SourceDestination(c)))
	}
	var pw []byte
	var sealer *secret.Sealer
	if f.passwordFile != "" {
		if sealer, err = h.sealer(); err != nil {
			return err
		}
		if sealer == nil {
			return errors.New("YUUKA_KEK_FILE is not set, so the password cannot be sealed; see docs/operate/secrets.md")
		}
		if pw, err = h.readSecret(f.passwordFile); err != nil {
			return err
		}
	}
	ch, err := manage.ConfigureSource(ctx, h.db, sealer, cur, manage.SourceWrite{Config: c, Password: pw}, h.action, now)
	clear(pw)
	switch {
	case errors.Is(err, manage.ErrUnchanged):
		return h.say("account %s already is as given; nothing changed", name)
	case errors.Is(err, store.ErrStale), errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("account %s changed while this command ran, and nothing was written; "+
			"look at `yuuka account ls` and run it again", name)
	case errors.Is(err, store.ErrDuplicate):
		return fmt.Errorf("another account already pulls %s", shown(c.Address))
	case errors.Is(err, store.ErrNeedsSecret):
		return fmt.Errorf("account %s: %w; give it with --password-file PATH or -", name, store.ErrNeedsSecret)
	case err != nil:
		return err
	}
	if ch.Sealed != nil {
		h.note("Password sealed for %s under %s key %s.", ch.Sealed.Audience, ch.Sealed.Wrapper, ch.Sealed.KeyRef)
		if cur.PauseReason == store.PauseAuthFailed {
			h.note("The source was paused because its login was refused; the new password ends that pause.")
		}
	}
	if ch.Source.Address != cur.Address {
		h.claimNote(ctx, ch.Source)
		h.note("%s stays claimed; `yuuka prefix unclaim` it if it is not yours.", shown(cur.Address))
	}
	if ch.Source.TrustedAuthservID != cur.TrustedAuthservID {
		h.note("The trusted authserv-id applies to mail pulled from now on. Mail already here keeps the verdicts it had " +
			"until `yuuka reparse` recomputes them (`--since` limits how far back).")
	}
	if len(ch.Widens) > 0 {
		h.alertWidening(ctx, notify.Alert{Text: fmt.Sprintf("yuuka: pull account %s (%s) was changed %s (%s): %s.",
			name, ch.Source.Address, h.by(), strings.Join(ch.Widens, ", "), manage.SourceDestination(ch.Source.SourceConfig))})
	}
	return h.say("changed account %s: %s", name, shown(manage.SourceDestination(ch.Source.SourceConfig)))
}

// accountSource is test, pause, resume and rm: one enabled source, read
// and then acted on at the revision read.
func (h *host) accountSource(ctx context.Context, verb, name string) error {
	cur, err := h.enabledSource(ctx, name)
	if err != nil {
		return err
	}
	stale := func(err error) error {
		if errors.Is(err, store.ErrStale) || errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("account %s changed while this command ran, and nothing was written; run it again", name)
		}
		return err
	}
	switch verb {
	case "test":
		return h.accountTest(ctx, cur)
	case "pause":
		if cur.PauseReason == store.PauseOperator {
			return h.say("account %s is paused already", name)
		}
		if err := manage.PauseSource(ctx, h.db, cur, h.action); err != nil {
			return stale(err)
		}
		return h.say("paused account %s; nothing is pulled until `yuuka account resume %s`", name, name)
	case "resume":
		if cur.PausedAt == nil {
			return h.say("account %s is not paused", name)
		}
		if cur.PauseReason == store.PauseAuthFailed {
			h.note("Its login was refused. Resuming logs in with the same password, and repeated wrong passwords can lock " +
				"the mailbox: if the password changed, give the new one with `yuuka account set " + name + " --password-file -` instead.")
		}
		if err := manage.ResumeSource(ctx, h.db, cur, h.action); err != nil {
			return stale(err)
		}
		h.alertWidening(ctx, notify.Alert{Text: fmt.Sprintf("yuuka: pull account %s (%s) was resumed %s.", name, cur.Address, h.by())})
		return h.say("resumed account %s; a running `yuuka serve` pulls it again", name)
	case "rm":
		if err := manage.RemoveSource(ctx, h.db, cur, h.action); err != nil {
			return stale(err)
		}
		h.note("%s stays claimed, and its mail stays readable as before.", shown(cur.Address))
		return h.say("removed account %s; its password is erased, its mail kept, and its name is not reused", name)
	}
	return errors.New(accountUsage)
}

// accountTest opens the password for the destination recomputed from the
// row, as pulling would, and logs in, lists and asks STATUS of each
// folder to sync. It fetches no mail and records nothing.
func (h *host) accountTest(ctx context.Context, cur store.Source) error {
	name := cur.AccountName
	sealer, err := h.sealer()
	if err != nil {
		return err
	}
	pw, err := manage.SourcePassword(ctx, h.db, sealer, cur)
	switch {
	case errors.Is(err, manage.ErrLocked):
		return fmt.Errorf("account %s: %s", name, manage.OpenFailure(secret.ErrLocked))
	case err != nil:
		return fmt.Errorf("account %s: %w", name, err)
	}
	ctx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	h.note("Connecting to %s (implicit TLS); no mail is fetched.", shown(manage.SourceDestination(cur.SourceConfig)))
	p, err := manage.ProbeSource(ctx, imap.Config{}, cur, pw)
	if ie, ok := errors.AsType[*imap.Error](err); ok && ie.Kind == imap.KindAuth {
		return fmt.Errorf("account %s: the login was refused (%s). Check the password; Gmail needs an app password, "+
			"see docs/operate/accounts.md. Repeated wrong passwords can lock the mailbox", name, shown(ie.Error()))
	}
	if err != nil {
		return fmt.Errorf("account %s: %s", name, shown(err.Error()))
	}
	tw := tabwriter.NewWriter(h.out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "FOLDER\tSPECIAL-USE\tMESSAGES\tUIDNEXT\tUIDVALIDITY")
	for _, f := range p.Folders {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\n", shown(f.Name), dash(f.SpecialUse), f.Messages, f.UIDNext, f.UIDValidity)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	var bad []string
	for _, m := range p.Missing {
		bad = append(bad, "no folder "+shown(m)+" on the server")
	}
	bad = append(bad, p.Problems...)
	if len(bad) > 0 {
		return fmt.Errorf("account %s logged in, but: %s", name, strings.Join(bad, "; "))
	}
	return h.say("ok: %s logged in as %s and can read %d folder(s)", name, shown(cur.Username), len(p.Folders))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return shown(s)
}

// accountSync is `account sync`: a request in the database that serve's
// worker for the account takes; the CLI never pulls (§10.1.3).
func (h *host) accountSync(ctx context.Context, name string, retry bool) error {
	cur, err := h.enabledSource(ctx, name)
	if err != nil {
		return err
	}
	if err := h.db.RequestSync(ctx, name, retry, h.now()); err != nil {
		return err
	}
	if cur.PausedAt != nil {
		h.note("Account %s is paused: the request waits until `yuuka account resume %s`.", name, name)
	}
	what := "a round"
	if retry {
		what = "a round that fetches the failed messages again"
	}
	return h.say("asked for %s of account %s; only a running `yuuka serve` acts on it, this command pulls nothing", what, name)
}

// accountList is `account ls`: every account, the source of each pull
// account with its state and secret, and how far each folder got. Never
// a password or anything derived from one.
func (h *host) accountList(ctx context.Context, all bool) error {
	accts, err := h.db.Accounts(ctx)
	if err != nil {
		return err
	}
	srcs, err := h.db.Sources(ctx, true)
	if err != nil {
		return err
	}
	byID := map[string]store.Source{}
	for _, s := range srcs {
		byID[s.AccountID] = s
	}
	states, err := h.secretStates(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(h.out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tKIND\tSTATE\tADDRESS\tSERVER\tAUTHSERV-ID\tMAILBOXES\tBACKFILL\tMAX-SIZE\tBUDGET\tPOLL\tSECRET\tDEFAULT-READ")
	var shownSrcs []store.Source
	for _, a := range accts {
		if a.DisabledAt != nil && !all {
			continue
		}
		readable := "no"
		if a.DefaultRead {
			readable = "yes"
		}
		s, ok := byID[a.ID]
		if !ok {
			state := "enabled"
			if a.DisabledAt != nil {
				state = "removed"
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t-\t-\t-\t-\t-\t-\t-\t-\t-\t%s\n", a.Name, a.Kind, state, readable)
			continue
		}
		shownSrcs = append(shownSrcs, s)
		mailboxes := "default"
		if s.Mailboxes != nil {
			mailboxes = shown(strings.Join(s.Mailboxes, ","))
		}
		backfill := "all"
		if s.BackfillSince != nil {
			backfill = "since " + s.BackfillSince.UTC().Format(time.DateOnly)
		}
		budget := "none"
		if s.DailyByteBudget != 0 {
			budget = formatSize(s.DailyByteBudget)
		}
		sec := "-"
		if s.SecretID != "" {
			sec = states[s.SecretID]
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.Name, a.Kind, sourceState(s),
			shown(s.Address), shown(manage.SourceDestination(s.SourceConfig)), dash(s.TrustedAuthservID), mailboxes, backfill,
			formatSize(s.MaxMessageBytes), budget, (time.Duration(s.PollSeconds) * time.Second).String(), sec, readable)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(shownSrcs) == 0 {
		return nil
	}
	_, _ = fmt.Fprintln(h.out)
	tw = tabwriter.NewWriter(h.out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ACCOUNT\tFOLDER\tCATCH-UP\tBACKFILL\tFAILED\tDRAFTS-SKIPPED\tLAST-ROUND\tNOTE")
	for _, s := range shownSrcs {
		boxes, err := h.db.SourceMailboxes(ctx, s.AccountID)
		if err != nil {
			return err
		}
		fails, err := h.db.PullFailures(ctx, s.AccountID, false)
		if err != nil {
			return err
		}
		if len(boxes) == 0 {
			_, _ = fmt.Fprintf(tw, "%s\t-\t-\t-\t%d\t-\t-\tnot synced yet\n", s.AccountName, len(fails))
			continue
		}
		for _, m := range boxes {
			failed := 0
			for _, f := range fails {
				if f.Mailbox == m.Name {
					failed++
				}
			}
			catchUp := "UID " + strconv.FormatUint(uint64(m.High), 10)
			if m.UIDNextSeen > 1 && m.High < m.UIDNextSeen-1 {
				catchUp += " of " + strconv.FormatUint(uint64(m.UIDNextSeen-1), 10)
			}
			backfill := "not started"
			switch {
			case m.Low == 1:
				backfill = "done"
			case m.Low > 1:
				backfill = "down to UID " + strconv.FormatUint(uint64(m.Low), 10)
			}
			note := "-"
			if m.RescanUIDValidity != 0 {
				note = "UIDVALIDITY changed: matching it again"
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\n", s.AccountName, shown(m.Name), catchUp, backfill,
				failed, m.DraftsSkipped, stamp(m.LastRoundAt), note)
		}
	}
	return tw.Flush()
}

// sourceState says whether a source is pulled, and if not, why.
func sourceState(s store.Source) string {
	switch {
	case s.DisabledAt != nil:
		return "removed"
	case s.PauseReason == store.PauseAuthFailed:
		return "paused: login refused"
	case s.PausedAt != nil:
		return "paused"
	}
	return "enabled"
}

// secretStates opens every sealed secret for its owner's audience and
// says ok or why not, by id; "locked" for all of them without a key.
func (h *host) secretStates(ctx context.Context) (map[string]string, error) {
	sealer, err := h.sealer()
	if err != nil {
		return nil, err
	}
	secs, err := h.db.Secrets(ctx)
	if err != nil {
		return nil, err
	}
	owners, err := manage.Owners(ctx, h.db)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, s := range secs {
		o, ok := owners[s.ID]
		switch {
		case !ok:
		case sealer == nil:
			out[s.ID] = "locked"
		default:
			res := manage.CheckOpen(ctx, sealer, s.Record, o)
			if r, _, cut := strings.Cut(res, ":"); cut {
				res = r
			}
			out[s.ID] = res
		}
	}
	return out, nil
}

// accountAccess is `account access`: list who may read the account, or
// change one principal's row or the account's default.
func (h *host) accountAccess(ctx context.Context, name, principal string, read, noRead, def bool, defaultRead string) error {
	acct, err := h.db.AccountByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no account %s; `yuuka account ls` lists them", name)
	}
	if err != nil {
		return err
	}
	n := 0
	for _, on := range []bool{read, noRead, def} {
		if on {
			n++
		}
	}
	switch {
	case defaultRead != "":
		if principal != "" || n != 0 {
			return errors.New("--default-read changes the account's default; change a principal in a command of its own")
		}
		var to bool
		switch defaultRead {
		case "yes":
			to = true
		case "no":
		default:
			return fmt.Errorf("--default-read %q: want yes or no", defaultRead)
		}
		widens, err := manage.SetDefaultRead(ctx, h.db, acct, to, h.action)
		if err != nil {
			return err
		}
		if widens {
			h.alertWidening(ctx, notify.Alert{Text: fmt.Sprintf(
				"yuuka: account %s was made readable %s by every principal without a setting of its own.", name, h.by())})
		}
		if to {
			return h.say("account %s: principals without a setting of their own may read it", name)
		}
		return h.say("account %s: principals without a setting of their own may not read it", name)
	case principal == "" && n == 0:
		return h.accessList(ctx, acct)
	case principal == "" || n != 1:
		return errors.New("account access: give --principal <kind>:<name> with one of --read, --no-read or --default")
	}

	var p store.Principal
	var canRead *bool
	if def {
		kind, pname, err := parsePrincipal(principal)
		if err != nil {
			return err
		}
		if p, err = h.db.PrincipalByName(ctx, kind, pname); err != nil {
			return err
		}
	} else {
		if p, err = h.grantee(ctx, principal); err != nil {
			return err
		}
		canRead = &read
	}
	rows, err := h.db.AccountAccess(ctx, name)
	if err != nil {
		return err
	}
	widens, err := manage.SetAccess(ctx, h.db, acct, rows, p, canRead, h.action)
	if err != nil {
		return err
	}
	if widens {
		h.alertWidening(ctx, notify.Alert{Text: fmt.Sprintf("yuuka: %s may now read account %s; changed %s.", principal, name, h.by())})
	}
	switch {
	case canRead == nil && acct.DefaultRead:
		return h.say("%s gets the default of account %s again: it may read it", principal, name)
	case canRead == nil:
		return h.say("%s gets the default of account %s again: it may not read it", principal, name)
	case *canRead:
		return h.say("%s may read account %s", principal, name)
	}
	return h.say("%s may not read account %s, whatever its default", principal, name)
}

// accessList lists the account's default and its rows. The system
// principal, which the host's own reads run as, reads every account and
// is not listed.
func (h *host) accessList(ctx context.Context, acct store.Account) error {
	rows, err := h.db.AccountAccess(ctx, acct.Name)
	if err != nil {
		return err
	}
	def := "no"
	if acct.DefaultRead {
		def = "yes"
	}
	tw := tabwriter.NewWriter(h.out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PRINCIPAL\tCAN-READ")
	_, _ = fmt.Fprintf(tw, "(default)\t%s\n", def)
	for _, r := range rows {
		v := "no"
		if r.CanRead {
			v = "yes"
		}
		_, _ = fmt.Fprintf(tw, "%s:%s\t%s\n", r.PrincipalKind, r.PrincipalName, v)
	}
	return tw.Flush()
}
