package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/imap"
	"github.com/yuuka-mail/yuuka/internal/manage"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
)

// Accounts (§9.5, §13.8, §17.8). Any principal may list the accounts it
// reads, by name, kind and address: what account: in a search takes.
// Everything else about them is a person's configuration, through the
// functions `yuuka account` calls (internal/manage), so the checks, what
// is recorded and what counts as widening are written once. What widens
// is known only once the change is compared with what is there (adding
// a folder widens, dropping one tightens), so those routes ask for an
// elevated session in the handler, as setChain does; resuming always
// widens and asks before the handler.

// SourceProber connects to a pull account's server as pulling would
// (manage.ProbeSource), fetching nothing.
type SourceProber func(ctx context.Context, s store.Source, password string) (*manage.SourceProbe, error)

func actAs(v *auth.Viewer) manage.Act {
	return func(tool string, args map[string]any) store.Action { return action(v, tool, args) }
}

// readableAccounts is GET /api/v1/accounts: the accounts v reads.
func (a *API) readableAccounts(ctx context.Context, v *auth.Viewer, _ struct{}) (AccountList, error) {
	as, err := a.db.VisibleAccounts(ctx, v)
	if err != nil {
		return AccountList{}, err
	}
	out := AccountList{Accounts: make([]ReadableAccount, len(as))}
	for i, x := range as {
		out.Accounts[i] = ReadableAccount{Name: x.Name, Kind: accountKind(x.Kind), Address: x.Address}
	}
	return out, nil
}

func (a *API) adminAccounts(ctx context.Context, _ *auth.Viewer, _ struct{}) (AdminAccountList, error) {
	accts, err := a.db.Accounts(ctx)
	if err != nil {
		return AdminAccountList{}, err
	}
	srcs, err := a.db.Sources(ctx, true)
	if err != nil {
		return AdminAccountList{}, err
	}
	bySource := make(map[string]store.Source, len(srcs))
	for _, s := range srcs {
		bySource[s.AccountID] = s
	}
	states, _, err := a.secretStates(ctx)
	if err != nil {
		return AdminAccountList{}, err
	}
	out := AdminAccountList{Accounts: make([]ManagedAccount, len(accts)), Locked: a.locked()}
	for i, acct := range accts {
		m := ManagedAccount{
			ID: acct.ID, Name: acct.Name, Kind: accountKind(acct.Kind), DefaultRead: acct.DefaultRead,
			CreatedAt: acct.CreatedAt, Access: []AccessEntry{},
		}
		rows, err := a.db.AccountAccess(ctx, acct.Name)
		if err != nil {
			return AdminAccountList{}, err
		}
		for _, r := range rows {
			m.Access = append(m.Access, AccessEntry{Principal: r.PrincipalKind + ":" + r.PrincipalName, CanRead: r.CanRead})
		}
		if s, ok := bySource[acct.ID]; ok {
			if m.Source, err = a.managedSource(ctx, s, states); err != nil {
				return AdminAccountList{}, err
			}
		}
		out.Accounts[i] = m
	}
	return out, nil
}

func sourceState(s store.Source) SourceState {
	switch {
	case s.DisabledAt != nil:
		return "removed"
	case s.PauseReason == store.PauseAuthFailed:
		return "login_refused"
	case s.PausedAt != nil:
		return "paused"
	}
	return "enabled"
}

// managedSource is s with how far each folder got and what failed.
func (a *API) managedSource(ctx context.Context, s store.Source, states map[string]SecretStatus) (*ManagedSource, error) {
	out := &ManagedSource{
		State: sourceState(s), PausedAt: s.PausedAt, Address: s.Address, Host: s.Host, Port: s.Port, Username: s.Username,
		Preset: SourcePreset(s.Preset), Mailboxes: s.Mailboxes, TrustedAuthservID: s.TrustedAuthservID,
		MaxMessageBytes: s.MaxMessageBytes, BackfillSince: s.BackfillSince, DailyByteBudget: s.DailyByteBudget,
		PollSeconds: s.PollSeconds, Secret: secretOf(states, s.SecretID), Revision: s.Revision,
		CreatedAt: s.CreatedAt, DisabledAt: s.DisabledAt, Folders: []SourceFolder{}, Failures: []PullFailureView{},
	}
	boxes, err := a.db.SourceMailboxes(ctx, s.AccountID)
	if err != nil {
		return nil, err
	}
	for _, m := range boxes {
		out.Folders = append(out.Folders, SourceFolder{
			Name: m.Name, SpecialUse: m.SpecialUse, UIDValidity: m.UIDValidity, High: m.High, Low: m.Low,
			UIDNext: m.UIDNextSeen, Messages: m.MessagesSeen, Rematching: m.RescanUIDValidity != 0,
			DraftsSkipped: m.DraftsSkipped, LastRoundAt: m.LastRoundAt,
		})
	}
	fails, err := a.db.PullFailures(ctx, s.AccountID, false)
	if err != nil {
		return nil, err
	}
	for _, f := range fails {
		out.Failures = append(out.Failures, PullFailureView{
			Folder: f.Mailbox, UID: f.UID, Size: f.Size, Detail: f.Detail, Attempts: f.Attempts, FirstAt: f.FirstAt, LastAt: f.LastAt,
		})
	}
	return out, nil
}

// accountNamed is account name as the list shows it.
func (a *API) accountNamed(ctx context.Context, name string) (ManagedAccount, error) {
	list, err := a.adminAccounts(ctx, nil, struct{}{})
	if err != nil {
		return ManagedAccount{}, err
	}
	for _, m := range list.Accounts {
		if m.Name == name {
			return m, nil
		}
	}
	return ManagedAccount{}, fmt.Errorf("account %s vanished after it was written", name)
}

// sourceAt reads the enabled source of pull account name and checks it
// is still at revision (0: no source yet, a new account). It returns nil
// for a name without a source.
func (a *API) sourceAt(ctx context.Context, name string, revision int64) (*store.Source, error) {
	cur, err := a.enabledSource(ctx, name)
	switch {
	case errors.Is(err, store.ErrNotFound) && revision == 0:
		return nil, nil
	case err != nil:
		return nil, err
	case cur.Revision != revision:
		return nil, staleProblem("account " + name)
	}
	return &cur, nil
}

// enabledSource reads the source of pull account name: store.ErrNotFound
// (404) for a name with none, 409 once removed.
func (a *API) enabledSource(ctx context.Context, name string) (store.Source, error) {
	cur, err := a.db.SourceByName(ctx, name)
	if err != nil {
		return cur, err
	}
	if cur.DisabledAt != nil {
		return cur, openapi.Errorf(http.StatusConflict, openapi.CodeConflict, "account %s is removed; its name is not reused", name)
	}
	return cur, nil
}

// sourceConfig is the configuration b describes: the preset's defaults,
// with what b gives over them.
func sourceConfig(b AccountBody) (store.SourceConfig, error) {
	c := manage.NewSourceConfig(string(b.Preset))
	c.Address = strings.TrimSpace(b.Address)
	if c.Address == "" {
		return c, invalid("address: required, the mailbox's own address")
	}
	if b.Host != "" {
		h, err := secret.CanonicalHost(strings.TrimSpace(b.Host))
		if err != nil {
			return c, invalid("host: %v", err)
		}
		c.Host = h
	}
	if c.Host == "" {
		return c, invalid("host: required without a preset")
	}
	if b.Port != 0 {
		c.Port = b.Port
	}
	c.Username = b.Username
	if c.Username == "" {
		// As the address is stored; a server that cares about case is
		// given the user name.
		c.Username = strings.ToLower(c.Address)
	}
	c.TrustedAuthservID = strings.TrimSpace(b.TrustedAuthservID)
	if len(b.Mailboxes) > 0 {
		c.Mailboxes = b.Mailboxes
	}
	c.BackfillSince = b.BackfillSince
	if b.MaxMessageBytes != 0 {
		c.MaxMessageBytes = b.MaxMessageBytes
	}
	if b.DailyByteBudget != nil {
		c.DailyByteBudget = *b.DailyByteBudget
	}
	if b.PollSeconds != 0 {
		c.PollSeconds = b.PollSeconds
	}
	c, err := manage.CheckSourceConfig(c)
	if inv, ok := errors.AsType[*manage.Invalid](err); ok {
		return c, invalid("%s", inv.Reason)
	}
	return c, err
}

// configureAccount writes a pull account's whole configuration as `yuuka
// account add` and `set` do. Adding one, a new password or destination,
// and anything SourceWidens names take an elevated session; a change
// that only tightens (a folder fewer, a lower budget) does not.
func (a *API) configureAccount(ctx context.Context, v *auth.Viewer, p AccountParams) (ManagedAccount, error) {
	b := p.Body
	cur, err := a.sourceAt(ctx, p.Name, b.Revision)
	if err != nil {
		return ManagedAccount{}, err
	}
	c, err := sourceConfig(b)
	if err != nil {
		return ManagedAccount{}, err
	}
	if b.Password != "" && a.locked() {
		return ManagedAccount{}, lockedProblem("Setting a password")
	}
	now := time.Now()
	if cur == nil {
		return a.addAccount(ctx, v, p.Name, c, b.Password, now)
	}
	if b.Password == "" && manage.SourceMoves(cur.SourceConfig, c) {
		return ManagedAccount{}, invalid("password: this moves account %s to %s, and a password is only ever sent where it was set for; give it again",
			p.Name, manage.SourceDestination(c))
	}
	if widens := manage.SourceWidens(cur.SourceConfig, c, b.Password != ""); len(widens) > 0 && !v.Elevated(now) {
		return ManagedAccount{}, elevationRequired(fmt.Sprintf(
			"This changes %s of account %s: more mail comes in, or its password goes somewhere new.", strings.Join(widens, ", "), p.Name),
			"yuuka account set "+p.Name+" ...")
	}
	pw := []byte(b.Password)
	if b.Password == "" {
		pw = nil
	}
	ch, err := manage.ConfigureSource(ctx, a.db, a.secrets.Sealer, *cur, manage.SourceWrite{Config: c, Password: pw}, actAs(v), now)
	clear(pw)
	if moved, ok := errors.AsType[*manage.SourceMovedError](err); ok {
		return ManagedAccount{}, invalid("password: %v", moved)
	}
	switch {
	case errors.Is(err, manage.ErrUnchanged):
		return a.accountNamed(ctx, p.Name)
	case errors.Is(err, manage.ErrLocked):
		return ManagedAccount{}, lockedProblem("Setting a password")
	case errors.Is(err, store.ErrStale), errors.Is(err, store.ErrNotFound):
		return ManagedAccount{}, staleProblem("account " + p.Name)
	case errors.Is(err, store.ErrDuplicate):
		return ManagedAccount{}, openapi.Errorf(http.StatusConflict, openapi.CodeConflict, "another account already pulls %s", c.Address)
	case errors.Is(err, store.ErrNeedsSecret):
		return ManagedAccount{}, invalid("password: %v", store.ErrNeedsSecret)
	case err != nil:
		return ManagedAccount{}, err
	}
	if ch.Source.Address != cur.Address {
		a.claimAddress(ctx, v, ch.Source)
	}
	if len(ch.Widens) > 0 {
		a.alertAbout(ctx, v, v.PrincipalID, "", fmt.Sprintf("changed pull account %s (%s): %s, pulling from %s",
			p.Name, ch.Source.Address, strings.Join(ch.Widens, ", "), manage.SourceDestination(ch.Source.SourceConfig)))
	}
	return a.accountNamed(ctx, p.Name)
}

// addAccount adds pull account name as `yuuka account add` does, readable
// by v alone at first, and claims its address. Always a widening.
func (a *API) addAccount(ctx context.Context, v *auth.Viewer, name string, c store.SourceConfig, password string, now time.Time) (ManagedAccount, error) {
	if password == "" {
		return ManagedAccount{}, invalid("password: a new pull account needs the mailbox's password")
	}
	if a.locked() {
		return ManagedAccount{}, lockedProblem("Adding a pull account")
	}
	if !v.Elevated(now) {
		return ManagedAccount{}, elevationRequired(
			"Adding a pull account lets another mailbox's mail in and sends its password to "+manage.SourceDestination(c)+".",
			"yuuka account add "+name+" --imap ...")
	}
	pw := []byte(password)
	src, _, err := manage.AddAccount(ctx, a.db, a.secrets.Sealer, manage.AccountAdd{Name: name, Config: c, Password: pw, Reader: v.PrincipalID}, actAs(v), now)
	clear(pw)
	if inv, ok := errors.AsType[*manage.Invalid](err); ok {
		return ManagedAccount{}, invalid("%s", inv.Reason)
	}
	switch {
	case errors.Is(err, manage.ErrLocked):
		return ManagedAccount{}, lockedProblem("Adding a pull account")
	case errors.Is(err, store.ErrDuplicate):
		return ManagedAccount{}, openapi.Errorf(http.StatusConflict, openapi.CodeConflict,
			"account %s was not added: the name is taken (a removed account keeps its name), or another account already pulls %s", name, c.Address)
	case err != nil:
		return ManagedAccount{}, err
	}
	a.claimAddress(ctx, v, src)
	a.alertAbout(ctx, v, v.PrincipalID, "", fmt.Sprintf("added pull account %s (%s), pulling from %s and readable by %s",
		name, src.Address, manage.SourceDestination(src.SourceConfig), v))
	return a.accountNamed(ctx, name)
}

// claimAddress claims a source's address as a prefix (manage.ClaimAddress).
// A failure is logged, not returned: the account is there either way,
// and claiming the prefix finishes the job.
func (a *API) claimAddress(ctx context.Context, v *auth.Viewer, s store.Source) {
	if _, err := manage.ClaimAddress(ctx, a.db, s, actAs(v)); err != nil {
		a.log.Error("api: claim the address of a pull account", "account", s.AccountName, "err", err)
	}
}

// pauseAccount pauses a source as `yuuka account pause` does: a
// tightening. Paused by a person already, it is left alone.
func (a *API) pauseAccount(ctx context.Context, v *auth.Viewer, p AccountRevisionParams) (ManagedAccount, error) {
	cur, err := a.revisionSource(ctx, p)
	if err != nil {
		return ManagedAccount{}, err
	}
	if cur.PauseReason != store.PauseOperator {
		if err := manage.PauseSource(ctx, a.db, cur, actAs(v)); err != nil {
			return ManagedAccount{}, a.sourceWriteError(p.Name, err)
		}
	}
	return a.accountNamed(ctx, p.Name)
}

// resumeAccount ends a pause, whatever its reason, as `yuuka account
// resume` does: a widening, since the password is sent again, and after
// a refused login it is the one the server refused.
func (a *API) resumeAccount(ctx context.Context, v *auth.Viewer, p AccountRevisionParams) (ManagedAccount, error) {
	cur, err := a.revisionSource(ctx, p)
	if err != nil {
		return ManagedAccount{}, err
	}
	if cur.PausedAt != nil {
		if err := manage.ResumeSource(ctx, a.db, cur, actAs(v)); err != nil {
			return ManagedAccount{}, a.sourceWriteError(p.Name, err)
		}
		a.alertAbout(ctx, v, v.PrincipalID, "", fmt.Sprintf("resumed pull account %s (%s)", p.Name, cur.Address))
	}
	return a.accountNamed(ctx, p.Name)
}

// disableAccount removes a source as `yuuka account rm` does: its
// password erased, the account and its mail kept, the name never
// reused. A tightening.
func (a *API) disableAccount(ctx context.Context, v *auth.Viewer, p AccountRevisionParams) (ManagedAccount, error) {
	cur, err := a.revisionSource(ctx, p)
	if err != nil {
		return ManagedAccount{}, err
	}
	if err := manage.RemoveSource(ctx, a.db, cur, actAs(v)); err != nil {
		return ManagedAccount{}, a.sourceWriteError(p.Name, err)
	}
	return a.accountNamed(ctx, p.Name)
}

func (a *API) revisionSource(ctx context.Context, p AccountRevisionParams) (store.Source, error) {
	cur, err := a.enabledSource(ctx, p.Name)
	if err != nil {
		return cur, err
	}
	if cur.Revision != p.Body.Revision {
		return cur, staleProblem("account " + p.Name)
	}
	return cur, nil
}

func (a *API) sourceWriteError(name string, err error) error {
	if errors.Is(err, store.ErrStale) || errors.Is(err, store.ErrNotFound) {
		return staleProblem("account " + name)
	}
	return err
}

// testAccount connects as `yuuka account test` does: the password goes
// only where it was sealed for, no mail is fetched, and it is not
// recorded, like the host command.
func (a *API) testAccount(ctx context.Context, _ *auth.Viewer, p AccountNameParams) (AccountProbe, error) {
	cur, err := a.enabledSource(ctx, p.Name)
	if err != nil {
		return AccountProbe{}, err
	}
	out := AccountProbe{Folders: []ProbeFolder{}, Missing: []string{}, Problems: []string{}}
	pw, err := manage.SourcePassword(ctx, a.db, a.secrets.Sealer, cur)
	switch {
	case errors.Is(err, manage.ErrLocked):
		return AccountProbe{}, lockedProblem("Testing a pull account")
	case err != nil:
		// A password that does not open: what testing found.
		out.Error = err.Error()
		return out, nil
	}
	if a.secrets.ProbeSource == nil {
		return AccountProbe{}, openapi.Errorf(http.StatusServiceUnavailable, openapi.CodeUnavailable, "this server does not pull mail")
	}
	probe, err := a.secrets.ProbeSource(ctx, cur, pw)
	if err != nil {
		out.Error = err.Error()
		if ie, ok := errors.AsType[*imap.Error](err); ok && ie.Kind == imap.KindAuth {
			out.LoginRefused = true
		}
		return out, nil
	}
	for _, f := range probe.Folders {
		out.Folders = append(out.Folders, ProbeFolder{
			Name: f.Name, SpecialUse: f.SpecialUse, Messages: f.Messages, UIDNext: f.UIDNext, UIDValidity: f.UIDValidity,
		})
	}
	out.Missing = append(out.Missing, probe.Missing...)
	out.Problems = append(out.Problems, probe.Problems...)
	out.OK = len(out.Missing) == 0 && len(out.Problems) == 0
	return out, nil
}

// syncAccount asks serve for a round as `yuuka account sync` does: a
// request in the database the account's worker takes. Nothing is pulled
// here, and a paused source takes it when resumed.
func (a *API) syncAccount(ctx context.Context, _ *auth.Viewer, p SyncParams) (struct{}, error) {
	if _, err := a.enabledSource(ctx, p.Name); err != nil {
		return struct{}{}, err
	}
	return struct{}{}, a.db.RequestSync(ctx, p.Name, p.Body.RetryFailed, time.Now())
}

// setAccountAccess changes one principal's setting for an account as
// `yuuka account access --principal` does. Letting it read what it could
// not widens and takes an elevated session; the rest tightens. The write
// is made only while the account's default and the principal's setting
// are as they were read, so the decision holds for what is written.
func (a *API) setAccountAccess(ctx context.Context, v *auth.Viewer, p AccessParams) (ManagedAccount, error) {
	acct, err := a.db.AccountByName(ctx, p.Name)
	if err != nil {
		return ManagedAccount{}, err
	}
	kind, name, err := store.ParsePrincipal(p.Principal)
	if err != nil {
		return ManagedAccount{}, openapi.Errorf(http.StatusBadRequest, openapi.CodeInvalidParam, "principal: %v", err)
	}
	var canRead *bool
	switch p.Body.Access {
	case "read":
		canRead = new(true)
	case "no_read":
		canRead = new(false)
	}
	pr, err := a.db.PrincipalByName(ctx, kind, name)
	exists := err == nil
	switch {
	case errors.Is(err, store.ErrNotFound) && canRead == nil:
		return ManagedAccount{}, openapi.Errorf(http.StatusNotFound, openapi.CodeNotFound, "no principal %s", p.Principal)
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return ManagedAccount{}, err
	}
	rows, err := a.db.AccountAccess(ctx, acct.Name)
	if err != nil {
		return ManagedAccount{}, err
	}
	widens := manage.AccessWidens(acct.DefaultRead, manage.RowOf(rows, pr.ID), canRead)
	if widens && !v.Elevated(time.Now()) {
		return ManagedAccount{}, elevationRequired(fmt.Sprintf("Letting %s read account %s shows it mail it could not see.", p.Principal, acct.Name),
			"yuuka account access "+acct.Name+" --principal "+p.Principal+" --read")
	}
	if !exists {
		// An agent or service is created on first use, as for a grant.
		if pr, err = a.grantee(ctx, v, p.Principal); err != nil {
			return ManagedAccount{}, err
		}
	}
	if _, err := manage.SetAccess(ctx, a.db, acct, rows, pr, canRead, actAs(v)); err != nil {
		if errors.Is(err, store.ErrStale) {
			return ManagedAccount{}, staleProblem("who may read account " + acct.Name)
		}
		return ManagedAccount{}, err
	}
	if widens {
		subject := v.PrincipalID
		if pr.Kind == "human" {
			subject = pr.ID
		}
		a.alertAbout(ctx, v, subject, "", fmt.Sprintf("let %s read account %s", p.Principal, acct.Name))
	}
	return a.accountNamed(ctx, acct.Name)
}

// setAccountDefaultRead sets whether principals without a setting of
// their own may read an account, as `yuuka account access
// --default-read` does. Making it readable widens.
func (a *API) setAccountDefaultRead(ctx context.Context, v *auth.Viewer, p DefaultReadParams) (ManagedAccount, error) {
	acct, err := a.db.AccountByName(ctx, p.Name)
	if err != nil {
		return ManagedAccount{}, err
	}
	to := p.Body.DefaultRead
	if acct.DefaultRead == to {
		return a.accountNamed(ctx, acct.Name)
	}
	widens := manage.DefaultReadWidens(acct.DefaultRead, to)
	if widens && !v.Elevated(time.Now()) {
		return ManagedAccount{}, elevationRequired(fmt.Sprintf(
			"Making account %s readable by every principal without a setting of its own shows them mail they could not see.", acct.Name),
			"yuuka account access "+acct.Name+" --default-read yes")
	}
	if _, err := manage.SetDefaultRead(ctx, a.db, acct, to, actAs(v)); err != nil {
		if errors.Is(err, store.ErrStale) {
			return ManagedAccount{}, staleProblem("who may read account " + acct.Name)
		}
		return ManagedAccount{}, err
	}
	if widens {
		a.alertAbout(ctx, v, v.PrincipalID, "", fmt.Sprintf("made account %s readable by every principal without a setting of its own", acct.Name))
	}
	return a.accountNamed(ctx, acct.Name)
}
