package manage

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"time"

	"github.com/yuuka-mail/yuuka/internal/imap"
	"github.com/yuuka-mail/yuuka/internal/r2"
	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/ulid"
)

// Pull accounts (§9.5, §10.1.3, §17.8). Adding one, giving a source a new
// password or destination, resuming it, and anything that lets more mail
// in or more principals read it widen (§13.10); pausing, removing and
// shrinking tighten. What widens is decided here (SourceWidens,
// AccessWidens, DefaultReadWidens), so the CLI and the REST API ask for
// the same re-verification and send the same alerts.

// Source defaults. They are the caller's to fill in (store.SourceConfig
// states every field), so that what is configured is what is stored.
const (
	// IMAPPort is implicit TLS (RFC 8314), the only mode the client has.
	IMAPPort = 993
	// GmailHost is the Gmail preset's server.
	GmailHost = "imap.gmail.com"
	// GmailDailyBudget stays under Gmail's daily IMAP download limit
	// (§10.1.3), so backfilling a large mailbox does not lock it out of
	// its own mail for the rest of the day.
	GmailDailyBudget = 2 << 30
	// DefaultPollSeconds is how often a source is checked (§10.1.3).
	DefaultPollSeconds = 60
	// MaxMessageBytes is the largest message a source may fetch whole:
	// raw objects are read into memory up to r2.MaxObjectSize, so a
	// larger one could be stored but never parsed again. Larger mail is
	// kept header only (§4.4).
	MaxMessageBytes = r2.MaxObjectSize
)

// NewSourceConfig is a source configuration with the defaults of preset
// ("" or store.PresetGmail): the Gmail preset's host and daily budget,
// port 993, AUTH PLAIN, the largest message size, and the default
// folders. Address, Username and, without a preset, Host are left for
// the caller.
func NewSourceConfig(preset string) store.SourceConfig {
	c := store.SourceConfig{
		Port: IMAPPort, Auth: store.SourceAuthPlain, Preset: preset,
		MaxMessageBytes: MaxMessageBytes, PollSeconds: DefaultPollSeconds,
	}
	if preset == store.PresetGmail {
		c.Host = GmailHost
		c.DailyByteBudget = GmailDailyBudget
		// TrustedAuthservID stays empty: Gmail's authserv-id is not
		// measured yet (§10.1.5), and empty trusts nothing, which is safe.
	}
	return c
}

// CheckSourceConfig is store.CheckSourceConfig plus MaxMessageBytes, with
// what it refuses as *Invalid.
func CheckSourceConfig(c store.SourceConfig) (store.SourceConfig, error) {
	c, err := store.CheckSourceConfig(c)
	if err != nil {
		return c, &Invalid{Reason: err.Error()}
	}
	if c.MaxMessageBytes > MaxMessageBytes {
		return c, &Invalid{Reason: fmt.Sprintf("source %s: max message size %d bytes: at most %d, the largest raw object yuuka reads back",
			c.Address, c.MaxMessageBytes, MaxMessageBytes)}
	}
	return c, nil
}

// SourceDestination describes where a source connects, as its audience
// would, without the scheme: user@host:port.
func SourceDestination(c store.SourceConfig) string {
	return c.Username + "@" + net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// SourceMoves reports whether c sends the password somewhere other than
// cur does: another host, port, auth or user name.
func SourceMoves(cur, c store.SourceConfig) bool {
	return cur.Host != c.Host || cur.Port != c.Port || cur.Auth != c.Auth || cur.Username != c.Username
}

// SourceMovedError: the write moves the source to another destination
// without a new password. A password is only ever sent where it was set
// for (§17.4), so it has to be given again.
type SourceMovedError struct{ Destination string }

func (e *SourceMovedError) Error() string {
	return "this moves the source to " + e.Destination + ": a password is only ever sent where it was set for, so give it again"
}

// ErrUnchanged: the write would store what is there already.
var ErrUnchanged = errors.New("nothing to change")

// ErrNeedsPassword: a new pull account needs its password.
var ErrNeedsPassword = errors.New("a pull account needs the mailbox's password")

// What a source write can widen (§13.10), as SourceWidens names it.
const (
	WidenDestination = "destination"
	WidenPassword    = "password"
	WidenAddress     = "address"
	WidenPreset      = "preset"
	WidenAuthservID  = "authserv-id"
	WidenMailboxes   = "mailboxes"
	WidenBackfill    = "backfill"
	WidenMaxSize     = "max-message-size"
	WidenBudget      = "daily-budget"
	WidenPoll        = "poll"
)

// SourceWidens lists what writing c over cur widens (§13.10), nothing
// when it only tightens or changes nothing:
//
//   - a new destination or password: where mail comes from and where the
//     password goes
//   - another address: whose mail it is, which prefix it is claimed under
//     and which identity a reply resolves to (§9.3)
//   - another preset: which folders are the default
//   - another trusted authserv-id, cleared included: it decides which
//     header is fact (§10.1.1); clearing it stops dmarc=fail mail from
//     being downgraded
//   - more folders, older backfill, larger messages, a larger or no
//     budget, a shorter poll: more mail, visible to everyone who reads
//     the account
//
// A change of the folder list to or from the default widens, since what
// the default holds is the server's to say.
func SourceWidens(cur, c store.SourceConfig, newPassword bool) []string {
	var out []string
	add := func(on bool, what string) {
		if on {
			out = append(out, what)
		}
	}
	add(SourceMoves(cur, c), WidenDestination)
	add(newPassword, WidenPassword)
	add(cur.Address != c.Address, WidenAddress)
	add(cur.Preset != c.Preset, WidenPreset)
	add(cur.TrustedAuthservID != c.TrustedAuthservID, WidenAuthservID)
	add((cur.Mailboxes == nil) != (c.Mailboxes == nil) || slices.ContainsFunc(c.Mailboxes, func(m string) bool {
		return !slices.Contains(cur.Mailboxes, m)
	}), WidenMailboxes)
	add(cur.BackfillSince != nil && (c.BackfillSince == nil || c.BackfillSince.Before(*cur.BackfillSince)), WidenBackfill)
	add(c.MaxMessageBytes > cur.MaxMessageBytes, WidenMaxSize)
	add(cur.DailyByteBudget != 0 && (c.DailyByteBudget == 0 || c.DailyByteBudget > cur.DailyByteBudget), WidenBudget)
	add(c.PollSeconds < cur.PollSeconds, WidenPoll)
	return out
}

// sameSource reports whether a and b configure the same source.
func sameSource(a, b store.SourceConfig) bool {
	sameTime := a.BackfillSince == nil && b.BackfillSince == nil ||
		a.BackfillSince != nil && b.BackfillSince != nil && a.BackfillSince.Equal(*b.BackfillSince)
	return a.Address == b.Address && !SourceMoves(a, b) && a.Preset == b.Preset &&
		(a.Mailboxes == nil) == (b.Mailboxes == nil) && slices.Equal(a.Mailboxes, b.Mailboxes) &&
		a.TrustedAuthservID == b.TrustedAuthservID && a.MaxMessageBytes == b.MaxMessageBytes && sameTime &&
		a.DailyByteBudget == b.DailyByteBudget && a.PollSeconds == b.PollSeconds
}

// SealIMAPPassword seals pw for c's destination under a fresh id. It
// does not keep pw; the caller clears it.
func SealIMAPPassword(ctx context.Context, s *secret.Sealer, c store.SourceConfig, pw []byte, now time.Time) (*secret.Record, error) {
	if s == nil {
		return nil, ErrLocked
	}
	aud, err := secret.IMAPS(c.Username, c.Host, c.Port)
	if err != nil {
		return nil, fmt.Errorf("source %s: %w", c.Address, err)
	}
	r, err := s.Seal(ctx, ulid.New(now), store.SecretIMAPPassword, aud, pw)
	if err != nil {
		return nil, fmt.Errorf("source %s: seal the password: %w", c.Address, err)
	}
	return &r, nil
}

// SourceAuditArgs are what an action on a pull account records (§17.6):
// the account, where its source connects, what it trusts and how much it
// pulls, and which sealed secret, never the password, its hash or
// anything else derived from it.
func SourceAuditArgs(name string, c store.SourceConfig, sealed *secret.Record) map[string]any {
	a := map[string]any{
		"name": name, "address": c.Address, "host": c.Host, "port": c.Port, "auth": c.Auth, "username": c.Username,
		"preset": c.Preset, "mailboxes": c.Mailboxes, "authserv_id": c.TrustedAuthservID,
		"max_message_bytes": c.MaxMessageBytes, "daily_byte_budget": c.DailyByteBudget, "poll_seconds": c.PollSeconds,
	}
	if c.BackfillSince != nil {
		a["backfill_since"] = c.BackfillSince.UTC().Format(time.RFC3339)
	}
	if aud, err := secret.IMAPS(c.Username, c.Host, c.Port); err == nil {
		a["audience"] = aud
	}
	if sealed != nil {
		a["secret_id"] = sealed.ID
		a["kind"] = sealed.Kind
		a["wrapper"] = sealed.Wrapper
		a["key_ref"] = sealed.KeyRef
	}
	return a
}

// SourceStore is what configuring pull accounts needs.
type SourceStore interface {
	AddPullAccount(ctx context.Context, name string, c store.SourceConfig, sealed secret.Record, reader string, a store.Action) (store.Source, error)
	ConfigureSource(ctx context.Context, name string, c store.SourceConfig, revision int64, sealed *secret.Record, a store.Action) (store.Source, error)
	DisableSource(ctx context.Context, name string, revision int64, a store.Action) error
	PauseSource(ctx context.Context, name string, revision int64, reason string, a store.Action) error
	ResumeSource(ctx context.Context, name string, revision int64, a store.Action) error
	SetPrefixState(ctx context.Context, zone, prefix string, state int, a store.Action) (store.PrefixChange, error)
}

// AccountAdd is a new pull account.
type AccountAdd struct {
	Name   string
	Config store.SourceConfig
	// Password is not kept: the caller clears it.
	Password []byte
	// Reader is the principal id that may read the account from the
	// start: whoever adds it (§13.8).
	Reader string
}

// AddAccount creates the pull account w (store.AddPullAccount): the
// password sealed for its destination, and the action account.add with
// SourceAuditArgs and the reader. A widening. The caller claims the
// address next (ClaimAddress).
//
// Errors: *Invalid, ErrNeedsPassword, ErrLocked, and the store's
// (ErrDuplicate: the name was ever used, or an enabled source has the
// address).
func AddAccount(ctx context.Context, st SourceStore, sealer *secret.Sealer, w AccountAdd, act Act, now time.Time) (store.Source, *secret.Record, error) {
	if !store.ValidName(w.Name) {
		return store.Source{}, nil, &Invalid{Reason: fmt.Sprintf("account name %q: want [A-Za-z0-9][A-Za-z0-9._-]*, at most 64", w.Name)}
	}
	c, err := CheckSourceConfig(w.Config)
	if err != nil {
		return store.Source{}, nil, err
	}
	if len(w.Password) == 0 {
		return store.Source{}, nil, ErrNeedsPassword
	}
	sealed, err := SealIMAPPassword(ctx, sealer, c, w.Password, now)
	if err != nil {
		return store.Source{}, nil, err
	}
	args := SourceAuditArgs(w.Name, c, sealed)
	args["reader_id"] = w.Reader
	s, err := st.AddPullAccount(ctx, w.Name, c, *sealed, w.Reader, act("account.add", args))
	return s, sealed, err
}

// ClaimAddress claims the source's address as a prefix (§9.3), so that
// its mail is not downgraded as mail to a discovered prefix would be,
// and leak and reply-identity logic apply to it as to the deployment's
// own addresses. The address being claimed already is not an error
// (store.PrefixUnchanged). Recorded as prefix.claim with the account.
func ClaimAddress(ctx context.Context, st SourceStore, s store.Source, act Act) (store.PrefixChange, error) {
	return st.SetPrefixState(ctx, s.Zone(), s.Prefix(), store.PrefixClaimed,
		act("prefix.claim", map[string]any{"account": s.AccountName, "account_id": s.AccountID}))
}

// SourceWrite is a whole configuration to write over a source.
type SourceWrite struct {
	Config store.SourceConfig
	// Password is the new password; nil keeps the one sealed now. It is
	// not kept: the caller clears it.
	Password []byte
}

// SourceChange is what ConfigureSource wrote.
type SourceChange struct {
	Source store.Source
	// Sealed is the new password's record, nil when it was kept.
	Sealed *secret.Record
	// Widens is what the write widened (SourceWidens); empty when it only
	// tightened. A widening asks for re-verification and an alert.
	Widens []string
}

// ConfigureSource writes w over cur, the enabled source as the caller
// read it, at cur's revision: a change made since is store.ErrStale and
// nothing is written. A new destination needs a new password
// (*SourceMovedError); a new password seals a new secret under a new id
// (§17.4). Recorded as account.set with SourceAuditArgs, what it was and
// what widened.
//
// Errors: *Invalid, *SourceMovedError, ErrUnchanged, ErrLocked (a
// password and no key), and the store's.
func ConfigureSource(ctx context.Context, st SourceStore, sealer *secret.Sealer, cur store.Source, w SourceWrite, act Act, now time.Time) (SourceChange, error) {
	c, err := CheckSourceConfig(w.Config)
	if err != nil {
		return SourceChange{}, err
	}
	if w.Password == nil {
		if SourceMoves(cur.SourceConfig, c) {
			return SourceChange{}, &SourceMovedError{Destination: SourceDestination(c)}
		}
		if sameSource(cur.SourceConfig, c) {
			return SourceChange{}, ErrUnchanged
		}
	}
	out := SourceChange{Widens: SourceWidens(cur.SourceConfig, c, w.Password != nil)}
	if w.Password != nil {
		if out.Sealed, err = SealIMAPPassword(ctx, sealer, c, w.Password, now); err != nil {
			return SourceChange{}, err
		}
	}
	args := SourceAuditArgs(cur.AccountName, c, out.Sealed)
	args["account_id"] = cur.AccountID
	args["revision"] = cur.Revision
	args["was"] = SourceAuditArgs(cur.AccountName, cur.SourceConfig, nil)
	args["widens"] = out.Widens
	out.Source, err = st.ConfigureSource(ctx, cur.AccountName, c, cur.Revision, out.Sealed, act("account.set", args))
	return out, err
}

// sourceArgs identify a source in the actions that do not configure it.
func sourceArgs(cur store.Source) map[string]any {
	a := map[string]any{
		"account_id": cur.AccountID, "name": cur.AccountName, "address": cur.Address, "host": cur.Host, "port": cur.Port,
		"username": cur.Username, "revision": cur.Revision,
	}
	if cur.SecretID != "" {
		a["secret_id"] = cur.SecretID
	}
	if cur.PauseReason != "" {
		a["pause_reason"] = cur.PauseReason
	}
	return a
}

// RemoveSource removes cur, as read, at its revision: the source
// disabled, its password erased, the account and its mail kept, the
// name never reused (store.DisableSource). A tightening, recorded as
// account.rm.
func RemoveSource(ctx context.Context, st SourceStore, cur store.Source, act Act) error {
	return st.DisableSource(ctx, cur.AccountName, cur.Revision, act("account.rm", sourceArgs(cur)))
}

// PauseSource pauses cur for the operator, at its revision. A
// tightening, recorded as account.pause; a source the operator paused
// already is left alone.
func PauseSource(ctx context.Context, st SourceStore, cur store.Source, act Act) error {
	return st.PauseSource(ctx, cur.AccountName, cur.Revision, store.PauseOperator, act("account.pause", sourceArgs(cur)))
}

// ResumeSource ends cur's pause, whatever its reason, at its revision. A
// widening, recorded as account.resume: the password is sent again, and
// after auth_failed that is the one the server refused.
func ResumeSource(ctx context.Context, st SourceStore, cur store.Source, act Act) error {
	return st.ResumeSource(ctx, cur.AccountName, cur.Revision, act("account.resume", sourceArgs(cur)))
}

// ---------------------------------------------------------------------------
// Who reads an account

// AccessWidens reports whether moving a principal's account_access row
// from was to to (nil: no row, so the account's default applies) lets it
// read an account it could not read before (§13.8, §13.10).
func AccessWidens(defaultRead bool, was, to *bool) bool {
	eff := func(row *bool) bool {
		if row == nil {
			return defaultRead
		}
		return *row
	}
	return !eff(was) && eff(to)
}

// DefaultReadWidens reports whether changing an account's default_read
// from was to to widens: it does when it becomes readable.
func DefaultReadWidens(was, to bool) bool { return !was && to }

// AccessStore is what changing who reads an account needs.
type AccessStore interface {
	SetAccountAccess(ctx context.Context, name, principalID string, defaultRead bool, was, canRead *bool, a store.Action) error
	SetAccountDefaultRead(ctx context.Context, name string, was, defaultRead bool, a store.Action) error
}

// RowOf is p's row among rows, nil when it has none.
func RowOf(rows []store.AccessRow, principalID string) *bool {
	for _, r := range rows {
		if r.PrincipalID == principalID {
			v := r.CanRead
			return &v
		}
	}
	return nil
}

// SetAccess sets p's row for account acct to canRead (nil: delete it,
// so the default applies), given acct and its rows as read, and records
// account.access. widens says whether it let p read what it could not
// (AccessWidens), which asks for re-verification and an alert. It is
// written only while the account's default and p's row are still as
// read (store.ErrStale otherwise), so widens holds for what was written.
func SetAccess(ctx context.Context, st AccessStore, acct store.Account, rows []store.AccessRow, p store.Principal,
	canRead *bool, act Act,
) (widens bool, err error) {
	was := RowOf(rows, p.ID)
	widens = AccessWidens(acct.DefaultRead, was, canRead)
	args := map[string]any{
		"account_id": acct.ID, "name": acct.Name, "principal_id": p.ID, "principal": p.Kind + ":" + p.Name,
		"can_read": accessArg(canRead), "was": accessArg(was), "default_read": acct.DefaultRead, "widens": widens,
	}
	return widens, st.SetAccountAccess(ctx, acct.Name, p.ID, acct.DefaultRead, was, canRead, act("account.access", args))
}

// SetDefaultRead sets whether principals without a row may read acct,
// and records account.access, while its default is still as read
// (store.ErrStale otherwise). widens: see DefaultReadWidens.
func SetDefaultRead(ctx context.Context, st AccessStore, acct store.Account, to bool, act Act) (widens bool, err error) {
	widens = DefaultReadWidens(acct.DefaultRead, to)
	args := map[string]any{
		"account_id": acct.ID, "name": acct.Name, "default_read": to, "was": acct.DefaultRead, "widens": widens,
	}
	return widens, st.SetAccountDefaultRead(ctx, acct.Name, acct.DefaultRead, to, act("account.access", args))
}

func accessArg(row *bool) any {
	if row == nil {
		return "default"
	}
	return *row
}

// ---------------------------------------------------------------------------
// Testing a source

// Why a source cannot be tested.
var ErrSourceRemoved = errors.New("removed")

// SourcePassword opens cur's password for the destination recomputed
// from the row, as pulling would (§17.2). Errors: ErrSourceRemoved,
// ErrLocked, *OpenError.
func SourcePassword(ctx context.Context, st SecretReader, sealer *secret.Sealer, cur store.Source) (string, error) {
	if cur.DisabledAt != nil || cur.SecretID == "" {
		return "", ErrSourceRemoved
	}
	if sealer == nil {
		return "", ErrLocked
	}
	sec, err := st.Secret(ctx, cur.SecretID)
	if err != nil {
		return "", err
	}
	aud, err := cur.Audience()
	if err != nil {
		return "", err
	}
	v, err := sealer.Open(ctx, sec.Record, aud)
	if err != nil {
		return "", &OpenError{Err: err}
	}
	defer v.Close()
	return string(v.Reveal()), nil
}

// SyncFolders picks, from a LIST, the folders a source syncs (§10.1): its
// configured mailboxes by their UTF-8 names; without any, \All for the
// Gmail preset, and otherwise every selectable folder but \Junk, \Trash,
// \Drafts and the virtual \All and \Flagged: those hold messages that
// are also in other folders, and under location keys each copy would be
// pulled again as another delivery. missing names the configured folders
// the LIST lacks, or \All when a Gmail mailbox has none.
func SyncFolders(c store.SourceConfig, boxes []imap.Mailbox) (sync []imap.Mailbox, missing []string) {
	switch {
	case c.Mailboxes != nil:
		for _, want := range c.Mailboxes {
			i := slices.IndexFunc(boxes, func(m imap.Mailbox) bool { return m.Selectable() && m.DisplayName() == want })
			if i < 0 {
				missing = append(missing, want)
				continue
			}
			sync = append(sync, boxes[i])
		}
	case c.Preset == store.PresetGmail:
		i := slices.IndexFunc(boxes, func(m imap.Mailbox) bool { return m.Selectable() && m.HasAttr(`\All`) })
		if i < 0 {
			return nil, []string{`\All`}
		}
		sync = append(sync, boxes[i])
	default:
		for _, m := range boxes {
			if m.Selectable() && !slices.ContainsFunc([]string{`\Junk`, `\Trash`, `\Drafts`, `\All`, `\Flagged`}, m.HasAttr) {
				sync = append(sync, m)
			}
		}
	}
	return sync, missing
}

// SpecialUse is the folder's RFC 6154 attribute, "" when it has none.
func SpecialUse(m imap.Mailbox) string {
	for _, a := range []string{`\All`, `\Archive`, `\Drafts`, `\Flagged`, `\Junk`, `\Sent`, `\Trash`} {
		if m.HasAttr(a) {
			return a
		}
	}
	return ""
}

// FolderStatus is one folder a source syncs, as STATUS saw it.
type FolderStatus struct {
	Name       string // UTF-8
	SpecialUse string
	imap.MailboxStatus
}

// SourceProbe is what a test of a source found.
type SourceProbe struct {
	Folders []FolderStatus
	// Missing are configured folders the server does not have.
	Missing []string
	// Problems are what would stop a sync even though login worked.
	Problems []string
}

// ProbeSource connects to cur's server as pulling would, with base for
// what is not the source's own (certificate roots, timeouts): implicit
// TLS, login (AUTHENTICATE PLAIN when offered, else LOGIN), LIST, and
// STATUS of each folder it syncs. It fetches no mail and changes nothing
// on the server. Login and connection failures are *imap.Error.
func ProbeSource(ctx context.Context, base imap.Config, cur store.Source, password string) (*SourceProbe, error) {
	cfg := base
	cfg.Host, cfg.Port = cur.Host, cur.Port
	c, err := imap.Dial(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	if c.Caps() == nil {
		if _, err := c.Capability(ctx); err != nil {
			return nil, err
		}
	}
	if c.Caps().Has("AUTH=PLAIN") {
		err = c.AuthenticatePlain(ctx, cur.Username, password)
	} else {
		err = c.Login(ctx, cur.Username, password)
	}
	if err != nil {
		return nil, err
	}
	p := &SourceProbe{}
	if cur.Preset == store.PresetGmail && !c.Caps().Has("X-GM-EXT-1") {
		p.Problems = append(p.Problems, "the server does not offer X-GM-EXT-1, which the Gmail preset needs: is this Gmail?")
	}
	boxes, err := c.List(ctx, imap.ListOptions{SpecialUse: true})
	if err != nil {
		return nil, err
	}
	folders, missing := SyncFolders(cur.SourceConfig, boxes)
	p.Missing = missing
	if len(folders) == 0 && len(missing) == 0 {
		p.Problems = append(p.Problems, "no folder to sync")
	}
	for _, m := range folders {
		st, err := c.Status(ctx, m.Name, "MESSAGES", "UIDNEXT", "UIDVALIDITY")
		if err != nil {
			return nil, err
		}
		p.Folders = append(p.Folders, FolderStatus{Name: m.DisplayName(), SpecialUse: SpecialUse(m), MailboxStatus: *st})
	}
	_ = c.Logout(ctx)
	return p, nil
}
