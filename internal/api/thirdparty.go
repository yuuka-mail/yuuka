package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/auth"
	"github.com/yuuka-mail/yuuka/internal/manage"
	"github.com/yuuka-mail/yuuka/internal/notify"
	"github.com/yuuka-mail/yuuka/internal/openapi"
	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/transport"
)

// Transports, channels and bindings (§17.6), through the functions the
// host CLI calls (internal/manage), so the checks, what is recorded and
// what counts as widening are written once. Every route is a person's
// session only; writing a secret or adding or moving a destination also
// takes an elevated one and tells the administrators and the person
// concerned. Removing, revoking and changing subscriptions tighten.

// Secrets is what the API needs to seal, open and use third-party
// secrets. A zero Secrets is the locked state (§17.3): everything that
// needs a key answers 503, everything else works.
type Secrets struct {
	// Sealer seals and opens; nil is locked.
	Sealer *secret.Sealer
	// GetMe asks Telegram who a bot token belongs to.
	GetMe manage.GetMe
	// Probe connects to a transport without sending mail
	// (transport.SMTP.Probe).
	Probe func(ctx context.Context, cfg transport.Config) error
	// Channels sends test messages; nil without channels from the
	// database.
	Channels ChannelTester
}

// ChannelTester sends a test message to the live bindings of a channel,
// or to one binding (notify.Dispatcher.Test).
type ChannelTester interface {
	Test(ctx context.Context, name, binding string) (int, error)
}

// SecurityAlerter sends a security alert to every administrator and the
// person it is about (notify.Dispatcher). An Alerter that is not one
// tells the administrators alone.
type SecurityAlerter interface {
	Security(ctx context.Context, a notify.Alert) error
}

// WithSecrets lets a seal, open and use third-party secrets through s.
func (a *API) WithSecrets(s Secrets) *API {
	a.secrets = s
	return a
}

func (a *API) locked() bool { return a.secrets.Sealer == nil }

// lockedProblem is the answer to a change that needs the key while the
// server has none (§17.3).
func lockedProblem(what string) error {
	return openapi.Errorf(http.StatusServiceUnavailable, openapi.CodeSecretsLocked,
		"%s needs the key that seals third-party secrets, and the server has none (YUUKA_KEK_FILE); "+
			"receiving mail and everything else works meanwhile", what)
}

// staleProblem is the answer to a change made at a revision that is no
// longer the row's.
func staleProblem(what string) error {
	return openapi.Errorf(http.StatusConflict, openapi.CodeConflict,
		"%s changed since it was read, and nothing was written; reload it and try again", what)
}

// alertAbout tells the administrators and subject ("" for none) that v
// widened something, as widened does, leaving the binding except out.
func (a *API) alertAbout(ctx context.Context, v *auth.Viewer, subject, except, what string) {
	if a.alert == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	text := fmt.Sprintf("yuuka: %s %s in the WebUI (session %s). If this was not you, someone holds that session and one of your passkeys: undo it in the settings.", v, what, v.CredentialID)
	var err error
	if sa, ok := a.alert.(SecurityAlerter); ok {
		err = sa.Security(ctx, notify.Alert{Subject: subject, Except: except, Text: text})
	} else {
		err = a.alert.Notify(ctx, text)
	}
	if err != nil {
		a.log.Error("api: alert failed", "err", err)
	}
}

// secretStates is the state of every sealed secret in use, by id, as
// opening it now would find it.
func (a *API) secretStates(ctx context.Context) (map[string]SecretStatus, map[string]manage.Owner, error) {
	secs, err := a.db.Secrets(ctx)
	if err != nil {
		return nil, nil, err
	}
	owners, err := manage.Owners(ctx, a.db)
	if err != nil {
		return nil, nil, err
	}
	out := make(map[string]SecretStatus, len(secs))
	for _, s := range secs {
		o, ok := owners[s.ID]
		if !ok {
			continue // nothing uses it
		}
		st := SecretStatus{SealedAt: s.SealedAt, State: "ok"}
		switch err := manage.Open(ctx, a.secrets.Sealer, s.Record, o); {
		case err == nil:
		case a.locked():
			st.State, st.Reason = "locked", "no_key"
		case errors.Is(err, secret.ErrLocked):
			st.State, st.Reason = "locked", "key_missing"
		default:
			st.State, st.Reason = "locked", "set_again"
		}
		out[s.ID] = st
	}
	return out, owners, nil
}

func secretOf(states map[string]SecretStatus, id string) *SecretStatus {
	if s, ok := states[id]; ok {
		return &s
	}
	return nil
}

func (a *API) secretsStatus(ctx context.Context, _ *auth.Viewer, _ struct{}) (SecretsStatus, error) {
	states, owners, err := a.secretStates(ctx)
	if err != nil {
		return SecretsStatus{}, err
	}
	trs, err := a.db.Transports(ctx, true)
	if err != nil {
		return SecretsStatus{}, err
	}
	chs, err := a.db.Channels(ctx, true)
	if err != nil {
		return SecretsStatus{}, err
	}
	out := SecretsStatus{Locked: a.locked(), Secrets: []ManagedSecret{}}
	for _, t := range trs {
		if st, ok := states[t.SecretID]; ok && owners[t.SecretID].Transport == t.Name {
			out.Secrets = append(out.Secrets, ManagedSecret{
				Owner:        SecretOwner{Kind: "transport", Name: t.Name, Destination: manage.Destination(transportConfig(t))},
				SecretStatus: st,
			})
		}
	}
	for _, c := range chs {
		if st, ok := states[c.SecretID]; ok && owners[c.SecretID].Channel == c.Name {
			out.Secrets = append(out.Secrets, ManagedSecret{
				Owner:        SecretOwner{Kind: "channel", Name: c.Name, Destination: botName(c)},
				SecretStatus: st,
			})
		}
	}
	return out, nil
}

func transportConfig(t store.Transport) store.TransportConfig {
	return store.TransportConfig{Name: t.Name, Host: t.Host, Port: t.Port, Auth: t.Auth, Username: t.Username, Caps: t.Caps}
}

// botName is a channel's bot as people know it: @name (id).
func botName(c store.Channel) string {
	if u := c.Settings["username"]; u != "" {
		return "@" + u + " (" + c.ExternalID + ")"
	}
	return c.ExternalID
}

// ---------------------------------------------------------------------------
// Transports

func (a *API) adminTransports(ctx context.Context, _ *auth.Viewer, _ struct{}) (TransportList, error) {
	trs, err := a.db.Transports(ctx, true)
	if err != nil {
		return TransportList{}, err
	}
	ids, err := a.db.Identities(ctx, false)
	if err != nil {
		return TransportList{}, err
	}
	states, _, err := a.secretStates(ctx)
	if err != nil {
		return TransportList{}, err
	}
	out := TransportList{Transports: make([]ManagedTransport, len(trs)), Locked: a.locked()}
	for k, t := range trs {
		m := ManagedTransport{
			ID: t.ID, Name: t.Name, State: "configured", Caps: []TransportCap{},
			Secret: secretOf(states, t.SecretID), Revision: t.Revision,
			CreatedAt: t.CreatedAt, ConfiguredAt: t.ConfiguredAt, DisabledAt: t.DisabledAt, UsedBy: []string{},
		}
		switch {
		case t.DisabledAt != nil:
			m.State = "removed"
		case t.ConfiguredAt == nil:
			m.State = "placeholder"
		}
		if t.ConfiguredAt != nil {
			m.Host, m.Port, m.Username = t.Host, t.Port, t.Username
			for _, c := range t.Caps {
				m.Caps = append(m.Caps, TransportCap(c))
			}
		}
		for _, i := range manage.NamedBy(ids, t.Name) {
			m.UsedBy = append(m.UsedBy, i.Address())
		}
		out.Transports[k] = m
	}
	return out, nil
}

// transportAt reads transport name and checks it is still at revision
// (0: there is no such transport yet). It returns nil for a name that
// has no row.
func (a *API) transportAt(ctx context.Context, name string, revision int64) (*store.Transport, error) {
	cur, err := a.db.TransportByName(ctx, name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if revision != 0 {
			return nil, openapi.Errorf(http.StatusNotFound, openapi.CodeNotFound, "no transport %s", name)
		}
		return nil, nil
	case err != nil:
		return nil, err
	case cur.Revision != revision:
		return nil, staleProblem("transport " + name)
	}
	return &cur, nil
}

// configureTransport writes a transport's whole configuration as `yuuka
// transport add` and `set` do (manage.ConfigureTransport), in the
// person's name. The route takes an elevated session: it writes a
// secret, or sets where mail goes.
func (a *API) configureTransport(ctx context.Context, v *auth.Viewer, p TransportParams) (ManagedTransport, error) {
	b := p.Body
	if b.Caps == nil {
		// They decide what is stripped from every message: no default.
		return ManagedTransport{}, invalid("caps: required, possibly empty ([] for Cloudflare Email Sending)")
	}
	cur, err := a.transportAt(ctx, p.Name, b.Revision)
	if err != nil {
		return ManagedTransport{}, err
	}
	c := store.TransportConfig{Name: p.Name, Host: strings.TrimSpace(b.Host), Port: b.Port, Username: b.Username, Caps: []string{}}
	c.Auth = store.TransportAuthNone
	if c.Username != "" {
		c.Auth = store.TransportAuthPlain
	}
	for _, x := range b.Caps {
		c.Caps = append(c.Caps, string(x))
	}
	var pw []byte
	if b.Password != "" {
		if a.locked() {
			return ManagedTransport{}, lockedProblem("Setting a password")
		}
		pw = []byte(b.Password)
	}
	tool := "transport.set"
	if cur == nil || cur.ConfiguredAt == nil || cur.DisabledAt != nil {
		tool = "transport.add"
	}
	t, sealed, err := manage.ConfigureTransport(ctx, a.db, a.secrets.Sealer, cur,
		manage.TransportWrite{Config: c, Password: pw, Tool: tool}, func(tool string, args map[string]any) store.Action {
			return action(v, tool, args)
		}, time.Now())
	clear(pw)
	if inv, ok := errors.AsType[*manage.Invalid](err); ok {
		return ManagedTransport{}, invalid("%s", inv.Reason)
	}
	if pn, ok := errors.AsType[*manage.PasswordNeededError](err); ok {
		if pn.Moved {
			return ManagedTransport{}, invalid("password: this moves transport %s to %s, and a password is only ever sent where it was set for; give it again", p.Name, pn.Destination)
		}
		return ManagedTransport{}, invalid("password: a transport with a user name authenticates, and needs the password")
	}
	switch {
	case errors.Is(err, manage.ErrPasswordUnused):
		return ManagedTransport{}, invalid("password: give a user name too; without one the transport does not authenticate")
	case errors.Is(err, manage.ErrLocked):
		return ManagedTransport{}, lockedProblem("Setting a password")
	case errors.Is(err, store.ErrStale), errors.Is(err, store.ErrDuplicate):
		return ManagedTransport{}, staleProblem("transport " + p.Name)
	case err != nil:
		return ManagedTransport{}, err
	}
	verb := "changed transport " + t.Name
	if tool == "transport.add" {
		verb = "configured transport " + t.Name
	}
	if sealed != nil {
		verb += ", with a new password,"
	}
	a.alertAbout(ctx, v, v.PrincipalID, "", fmt.Sprintf("%s to send through %s, caps %s", verb, manage.Destination(transportConfig(t)), capsText(t.Caps)))
	return a.transportNamed(ctx, t.Name)
}

func capsText(caps []string) string {
	if len(caps) == 0 {
		return "none"
	}
	return strings.Join(caps, ",")
}

// transportNamed is transport name as the list shows it.
func (a *API) transportNamed(ctx context.Context, name string) (ManagedTransport, error) {
	list, err := a.adminTransports(ctx, nil, struct{}{})
	if err != nil {
		return ManagedTransport{}, err
	}
	for _, t := range list.Transports {
		if t.Name == name {
			return t, nil
		}
	}
	return ManagedTransport{}, fmt.Errorf("transport %s vanished after it was written", name)
}

// disableTransport removes a transport as `yuuka transport rm` does: a
// tightening, in one step.
func (a *API) disableTransport(ctx context.Context, v *auth.Viewer, p RemoveParams) (TransportRemoved, error) {
	cur, err := a.transportAt(ctx, p.Name, p.Body.Revision)
	if err != nil {
		return TransportRemoved{}, err
	}
	if cur.DisabledAt != nil {
		return TransportRemoved{}, openapi.Errorf(http.StatusNotFound, openapi.CodeNotFound, "transport %s is removed already", p.Name)
	}
	err = manage.DisableTransport(ctx, a.db, *cur, func(tool string, args map[string]any) store.Action { return action(v, tool, args) })
	if errors.Is(err, store.ErrStale) || errors.Is(err, store.ErrNotFound) {
		return TransportRemoved{}, staleProblem("transport " + p.Name)
	}
	if err != nil {
		return TransportRemoved{}, err
	}
	ids, err := a.db.Identities(ctx, false)
	if err != nil {
		return TransportRemoved{}, err
	}
	out := TransportRemoved{StillNamedBy: []string{}}
	for _, i := range manage.NamedBy(ids, p.Name) {
		out.StillNamedBy = append(out.StillNamedBy, i.Address())
	}
	return out, nil
}

// testTransport connects as `yuuka transport test` does: the password
// goes only where it was sealed for, and no mail is sent. Not recorded,
// like the host command.
func (a *API) testTransport(ctx context.Context, _ *auth.Viewer, p NameParams) (TransportProbe, error) {
	cur, err := a.db.TransportByName(ctx, p.Name)
	if err != nil {
		return TransportProbe{}, err
	}
	cfg, err := manage.ProbeConfig(ctx, a.db, a.secrets.Sealer, cur)
	switch {
	case errors.Is(err, manage.ErrRemoved), errors.Is(err, manage.ErrPlaceholder):
		return TransportProbe{}, openapi.Errorf(http.StatusConflict, openapi.CodeConflict, "transport %s is %v", p.Name, err)
	case errors.Is(err, manage.ErrLocked):
		return TransportProbe{}, lockedProblem("Testing a transport with a password")
	case err != nil:
		// No password, or one that does not open: what testing found.
		return TransportProbe{Error: err.Error()}, nil
	}
	if a.secrets.Probe == nil {
		return TransportProbe{}, openapi.Errorf(http.StatusServiceUnavailable, openapi.CodeUnavailable, "this server does not send mail")
	}
	if err := a.secrets.Probe(ctx, cfg); err != nil {
		return TransportProbe{Error: err.Error()}, nil
	}
	return TransportProbe{OK: true}, nil
}

// setChain replaces an identity's transport chain as `yuuka identity
// set --transports` does. Taking a fallback away or reordering behind
// the same primary tightens, in one step; anything else widens and
// takes an elevated session (§17.6).
func (a *API) setChain(ctx context.Context, v *auth.Viewer, p ChainParams) (ManagedIdentity, error) {
	names := p.Body.Transports
	for _, n := range names {
		if !store.ValidName(n) {
			return ManagedIdentity{}, invalid("transports: %q is not a transport name", n)
		}
	}
	i, err := a.db.IdentityByID(ctx, p.ID)
	if err != nil {
		return ManagedIdentity{}, err
	}
	if i.DisabledAt != nil {
		return ManagedIdentity{}, openapi.Errorf(http.StatusConflict, openapi.CodeConflict, "%s is disabled; nothing sends as it", i.Address())
	}
	widens := manage.ChainWidens(i.Transports, names)
	if widens && !v.Elevated(time.Now()) {
		return ManagedIdentity{}, elevationRequired(fmt.Sprintf(
			"Sending %s's mail through %s lets it go where it could not before.", i.Address(), strings.Join(names, ", then ")),
			"yuuka identity set "+shellWord(i.Address())+" --transports "+strings.Join(names, ","))
	}
	err = a.db.ReplaceIdentityTransports(ctx, i.ID, i.Transports, names, action(v, "identity.route", map[string]any{
		"identity_id": i.ID, "address": i.Address(), "transports": names, "was": i.Transports,
	}))
	switch {
	case errors.Is(err, store.ErrStale):
		return ManagedIdentity{}, staleProblem("the chain of " + i.Address())
	case errors.Is(err, store.ErrNoTransport):
		return ManagedIdentity{}, invalid("transports: %v", errors.Unwrap(err))
	case errors.Is(err, store.ErrNotFound):
		return ManagedIdentity{}, err
	case err != nil:
		// The chain's own shape: empty, too long, a name twice.
		return ManagedIdentity{}, invalid("transports: %v", errors.Unwrap(err))
	}
	if widens {
		subject := ""
		if i.OwnerKind == "human" {
			subject = i.OwnerID
		}
		a.alertAbout(ctx, v, subject, "", fmt.Sprintf("made %s send through %s (was %s)",
			i.Address(), strings.Join(names, ", then "), strings.Join(i.Transports, ", then ")))
	}
	list, err := a.adminIdentities(ctx, v, struct{}{})
	if err != nil {
		return ManagedIdentity{}, err
	}
	for _, m := range list.Identities {
		if m.ID == i.ID {
			return m, nil
		}
	}
	return ManagedIdentity{}, fmt.Errorf("identity %s vanished", i.ID)
}

// ---------------------------------------------------------------------------
// Channels

func (a *API) adminChannels(ctx context.Context, _ *auth.Viewer, _ struct{}) (ChannelList, error) {
	cs, err := a.db.Channels(ctx, true)
	if err != nil {
		return ChannelList{}, err
	}
	bs, err := a.db.Bindings(ctx, false)
	if err != nil {
		return ChannelList{}, err
	}
	states, _, err := a.secretStates(ctx)
	if err != nil {
		return ChannelList{}, err
	}
	live := map[string]int{}
	for _, b := range bs {
		live[b.ChannelID]++
	}
	out := ChannelList{Channels: make([]ManagedChannel, len(cs)), Locked: a.locked()}
	for k, c := range cs {
		out.Channels[k] = ManagedChannel{
			ID: c.ID, Name: c.Name, Kind: c.Kind, BotID: c.ExternalID, BotUsername: c.Settings["username"],
			Secret: secretOf(states, c.SecretID), Revision: c.Revision, CreatedAt: c.CreatedAt, DisabledAt: c.DisabledAt,
			Bindings: live[c.ID],
		}
	}
	return out, nil
}

// channelAt reads channel name and checks it is still at revision (0:
// none yet); nil for a name without a row.
func (a *API) channelAt(ctx context.Context, name string, revision int64) (*store.Channel, error) {
	cur, err := a.db.ChannelByName(ctx, name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if revision != 0 {
			return nil, openapi.Errorf(http.StatusNotFound, openapi.CodeNotFound, "no channel %s", name)
		}
		return nil, nil
	case err != nil:
		return nil, err
	case cur.Revision != revision:
		return nil, staleProblem("channel " + name)
	}
	return &cur, nil
}

// setChannel gives a channel its bot token as `yuuka channel add` does
// (manage.AddChannel). The token comes in the body only, is sealed for
// the bot Telegram names, and is never answered or recorded.
func (a *API) setChannel(ctx context.Context, v *auth.Viewer, p ChannelParams) (ManagedChannel, error) {
	if a.locked() || a.secrets.GetMe == nil {
		return ManagedChannel{}, lockedProblem("Adding a channel")
	}
	cur, err := a.channelAt(ctx, p.Name, p.Body.Revision)
	if err != nil {
		return ManagedChannel{}, err
	}
	token := strings.TrimSpace(p.Body.Token)
	c, bot, err := manage.AddChannel(ctx, a.db, a.secrets.Sealer, a.secrets.GetMe, p.Name, token, p.Body.Revision,
		func(tool string, args map[string]any) store.Action { return action(v, tool, args) }, time.Now())
	if inv, ok := errors.AsType[*manage.Invalid](err); ok {
		return ManagedChannel{}, invalid("%s", inv.Reason)
	}
	if _, ok := errors.AsType[*manage.AskError](err); ok {
		// Telegram refused the token or could not be reached; its error
		// never carries the token (notify.Telegram).
		return ManagedChannel{}, openapi.Errorf(http.StatusBadGateway, openapi.CodeUnavailable, "%v", err)
	}
	if _, ok := errors.AsType[*manage.BotMismatchError](err); ok {
		return ManagedChannel{}, invalid("token: %v", err)
	}
	switch {
	case errors.Is(err, manage.ErrNotToken):
		return ManagedChannel{}, invalid("token: %v", err)
	case errors.Is(err, store.ErrChannelBot):
		return ManagedChannel{}, openapi.Errorf(http.StatusConflict, openapi.CodeConflict,
			"channel %s is bot %s: its bindings are chats with that bot. Remove the channel and add it again for another bot", p.Name, cur.ExternalID)
	case errors.Is(err, store.ErrDuplicate):
		return ManagedChannel{}, openapi.Errorf(http.StatusConflict, openapi.CodeConflict,
			"another enabled channel has bot @%s, or channel %s was added meanwhile", bot.Username, p.Name)
	case errors.Is(err, store.ErrStale):
		return ManagedChannel{}, staleProblem("channel " + p.Name)
	case err != nil:
		return ManagedChannel{}, err
	}
	what := "added the notification channel " + c.Name + " (bot @" + bot.Username + ")"
	if cur != nil && cur.DisabledAt == nil {
		what = "gave the notification channel " + c.Name + " (bot @" + bot.Username + ") a new token"
	}
	a.alertAbout(ctx, v, v.PrincipalID, "", what)
	list, err := a.adminChannels(ctx, v, struct{}{})
	if err != nil {
		return ManagedChannel{}, err
	}
	for _, m := range list.Channels {
		if m.ID == c.ID {
			return m, nil
		}
	}
	return ManagedChannel{}, fmt.Errorf("channel %s vanished after it was written", c.Name)
}

// disableChannel removes a channel as `yuuka channel rm` does: its token
// erased, its bindings revoked. A tightening.
func (a *API) disableChannel(ctx context.Context, v *auth.Viewer, p RemoveParams) (struct{}, error) {
	cur, err := a.channelAt(ctx, p.Name, p.Body.Revision)
	if err != nil {
		return struct{}{}, err
	}
	if cur.DisabledAt != nil {
		return struct{}{}, openapi.Errorf(http.StatusNotFound, openapi.CodeNotFound, "channel %s is removed already", p.Name)
	}
	err = manage.DisableChannel(ctx, a.db, *cur, func(tool string, args map[string]any) store.Action { return action(v, tool, args) })
	if errors.Is(err, store.ErrStale) || errors.Is(err, store.ErrNotFound) {
		return struct{}{}, staleProblem("channel " + p.Name)
	}
	return struct{}{}, err
}

// testChannel sends a test message to every live binding of a channel,
// as `yuuka channel test` does. Their chats were confirmed when bound,
// so this widens nothing and is not recorded.
func (a *API) testChannel(ctx context.Context, _ *auth.Viewer, p NameParams) (ChannelTestResult, error) {
	c, err := a.db.ChannelByName(ctx, p.Name)
	if err != nil {
		return ChannelTestResult{}, err
	}
	if c.DisabledAt != nil {
		return ChannelTestResult{}, openapi.Errorf(http.StatusConflict, openapi.CodeConflict, "channel %s is removed", p.Name)
	}
	if a.locked() || a.secrets.Channels == nil {
		return ChannelTestResult{}, lockedProblem("Sending through a channel")
	}
	n, err := a.secrets.Channels.Test(ctx, p.Name, "")
	out := ChannelTestResult{Sent: n}
	if err != nil {
		out.Error = err.Error()
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// The viewer's own bindings (§13.7): a person binds only their own chats
// in the WebUI; binding for someone else is done on the host.

func myBinding(b store.Binding, bots map[string]string) MyBinding {
	out := MyBinding{
		ID: b.ID, Channel: b.ChannelName, BotUsername: bots[b.ChannelID], Chat: b.Address, ButtonUser: b.ActorID,
		Label: b.Label, Events: make([]Event, len(b.Events)), CreatedAt: b.CreatedAt, LastUsedAt: b.LastUsedAt,
	}
	for k, e := range b.Events {
		out.Events[k] = Event(e)
	}
	return out
}

// botNames maps channel ids to their bots' user names.
func (a *API) botNames(ctx context.Context) (map[string]string, []store.Channel, error) {
	cs, err := a.db.Channels(ctx, true)
	if err != nil {
		return nil, nil, err
	}
	out := make(map[string]string, len(cs))
	for _, c := range cs {
		out[c.ID] = c.Settings["username"]
	}
	return out, cs, nil
}

func (a *API) bindings(ctx context.Context, v *auth.Viewer, _ struct{}) (BindingList, error) {
	bs, err := a.db.Bindings(ctx, false)
	if err != nil {
		return BindingList{}, err
	}
	bots, cs, err := a.botNames(ctx)
	if err != nil {
		return BindingList{}, err
	}
	out := BindingList{Bindings: []MyBinding{}, Channels: []BindableChannel{}, Locked: a.locked()}
	for _, b := range bs {
		if b.PrincipalID == v.PrincipalID {
			out.Bindings = append(out.Bindings, myBinding(b, bots))
		}
	}
	for _, c := range cs {
		if c.DisabledAt == nil {
			out.Channels = append(out.Channels, BindableChannel{Name: c.Name, BotUsername: c.Settings["username"]})
		}
	}
	return out, nil
}

// ownBinding is live binding id if it is v's; anyone else's is not
// found, as if it did not exist.
func (a *API) ownBinding(ctx context.Context, v *auth.Viewer, id string) (store.Binding, error) {
	b, err := a.db.BindingByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) || err == nil && (b.PrincipalID != v.PrincipalID || b.RevokedAt != nil) {
		return b, openapi.Errorf(http.StatusNotFound, openapi.CodeNotFound, "no binding %s of yours", id)
	}
	return b, err
}

// revokeBinding revokes one of the viewer's bindings as `yuuka channel
// unbind` does. A tightening: it takes effect from the next event on.
func (a *API) revokeBinding(ctx context.Context, v *auth.Viewer, p IDParams) (struct{}, error) {
	if _, err := a.ownBinding(ctx, v, p.ID); err != nil {
		return struct{}{}, err
	}
	err := a.db.RevokeBinding(ctx, p.ID, action(v, "channel.unbind", map[string]any{"binding_id": p.ID}))
	if errors.Is(err, store.ErrNotFound) {
		return struct{}{}, openapi.Errorf(http.StatusNotFound, openapi.CodeNotFound, "no binding %s of yours", p.ID)
	}
	return struct{}{}, err
}

// setEvents changes what one of the viewer's bindings receives, as
// `yuuka channel subscribe` and `unsubscribe` do. Not a widening: the
// chat was confirmed when bound, and mail.new is filtered by what its
// person may see (§13.10).
func (a *API) setEvents(ctx context.Context, v *auth.Viewer, p EventsParams) (MyBinding, error) {
	b, err := a.ownBinding(ctx, v, p.ID)
	if err != nil {
		return MyBinding{}, err
	}
	var add, remove []string
	want := make([]string, len(p.Body.Events))
	for k, e := range p.Body.Events {
		want[k] = string(e)
		if !slices.Contains(b.Events, want[k]) && !slices.Contains(add, want[k]) {
			add = append(add, want[k])
		}
	}
	for _, e := range b.Events {
		if !slices.Contains(want, e) {
			remove = append(remove, e)
		}
	}
	for _, x := range []struct {
		events []string
		on     bool
		tool   string
	}{{add, true, "channel.subscribe"}, {remove, false, "channel.unsubscribe"}} {
		if len(x.events) == 0 {
			continue
		}
		err := a.db.Subscribe(ctx, b.ID, x.events, x.on, action(v, x.tool, map[string]any{"binding_id": b.ID, "events": x.events}))
		if errors.Is(err, store.ErrNotFound) {
			return MyBinding{}, openapi.Errorf(http.StatusNotFound, openapi.CodeNotFound, "no binding %s of yours", p.ID)
		}
		if err != nil {
			return MyBinding{}, err
		}
	}
	if b, err = a.db.BindingByID(ctx, b.ID); err != nil {
		return MyBinding{}, err
	}
	bots, _, err := a.botNames(ctx)
	if err != nil {
		return MyBinding{}, err
	}
	return myBinding(b, bots), nil
}

func bindCodeView(bc store.BindCode, c store.Channel, now time.Time) BindCodeView {
	out := BindCodeView{
		ID: bc.ID, Channel: c.Name, BotUsername: c.Settings["username"], State: "waiting", ExpiresAt: bc.ExpiresAt,
		BindingID: bc.BindingID,
	}
	if bc.RedeemedAt != nil {
		out.State = "redeemed"
		out.Seen = &SeenIdentity{
			Label: bc.SeenLabel, UserID: bc.SeenActorID, ChatID: bc.SeenAddress, ChatKind: bc.SeenChatKind, At: *bc.RedeemedAt,
		}
	}
	switch {
	case bc.BindingID != "":
		out.State = "confirmed"
	case !now.Before(bc.ExpiresAt):
		out.State = "expired"
	}
	return out
}

// channelByID is enabled or removed channel id.
func (a *API) channelByID(ctx context.Context, id string) (store.Channel, error) {
	cs, err := a.db.Channels(ctx, true)
	if err != nil {
		return store.Channel{}, err
	}
	for _, c := range cs {
		if c.ID == id {
			return c, nil
		}
	}
	return store.Channel{}, fmt.Errorf("channel %s: %w", id, store.ErrNotFound)
}

// startBinding is §13.7 step 1 for the viewer's own chat: a code, shown
// this once, and the link that sends it to the bot. It widens (a new
// destination for the viewer's notifications, and a button user), so it
// takes an elevated session; the code itself grants nothing.
func (a *API) startBinding(ctx context.Context, v *auth.Viewer, p BindStartParams) (BindStart, error) {
	c, err := a.db.ChannelByName(ctx, p.Body.Channel)
	if errors.Is(err, store.ErrNotFound) || err == nil && c.DisabledAt != nil {
		return BindStart{}, invalid("channel: no enabled channel %q", p.Body.Channel)
	}
	if err != nil {
		return BindStart{}, err
	}
	if a.locked() {
		// serve's poller receives the code, and it does not run without
		// the key: the code could never arrive.
		return BindStart{}, lockedProblem("Binding a chat")
	}
	me, err := a.db.Principal(ctx, v.PrincipalID)
	if err != nil {
		return BindStart{}, err
	}
	code, err := manage.NewBindCode()
	if err != nil {
		return BindStart{}, err
	}
	bc, err := a.db.NewBindCode(ctx, c.ID, v.PrincipalID, v.CredentialID, code,
		action(v, "channel.bind.begin", manage.BindBeginArgs(c.Name, me)))
	if errors.Is(err, store.ErrNotFound) {
		return BindStart{}, invalid("channel: no enabled channel %q", p.Body.Channel)
	}
	if err != nil {
		return BindStart{}, err
	}
	out := BindStart{BindCode: bindCodeView(bc, c, time.Now()), Code: code}
	out.Link, out.GroupLink = manage.BindLinks(c.Settings["username"], code)
	return out, nil
}

// ownBindCode is bind code id if it is for v; anyone else's is not
// found.
func (a *API) ownBindCode(ctx context.Context, v *auth.Viewer, id string) (store.BindCode, store.Channel, error) {
	bc, err := a.db.BindCodeByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) || err == nil && bc.PrincipalID != v.PrincipalID {
		return bc, store.Channel{}, openapi.Errorf(http.StatusNotFound, openapi.CodeNotFound, "no bind code %s of yours", id)
	}
	if err != nil {
		return bc, store.Channel{}, err
	}
	c, err := a.channelByID(ctx, bc.ChannelID)
	return bc, c, err
}

// bindCode is §13.7 step 2 as the viewer sees it: whether the code
// reached the bot, and who Telegram says sent it.
func (a *API) bindCode(ctx context.Context, v *auth.Viewer, p IDParams) (BindCodeView, error) {
	bc, c, err := a.ownBindCode(ctx, v, p.ID)
	if err != nil {
		return BindCodeView{}, err
	}
	return bindCodeView(bc, c, time.Now()), nil
}

// confirmBinding is §13.7 step 3: the viewer saw who sent the code and
// agrees. It makes the binding; the alert goes to the viewer's other
// bindings and to the administrators, never to the new chat, which may
// be someone else's.
func (a *API) confirmBinding(ctx context.Context, v *auth.Viewer, p BindConfirmParams) (MyBinding, error) {
	bc, c, err := a.ownBindCode(ctx, v, p.ID)
	if err != nil {
		return MyBinding{}, err
	}
	me, err := a.db.Principal(ctx, v.PrincipalID)
	if err != nil {
		return MyBinding{}, err
	}
	events := store.Events
	if p.Body.Events != nil {
		events = make([]string, len(p.Body.Events))
		for k, e := range p.Body.Events {
			events[k] = string(e)
		}
	}
	b, err := a.db.ConfirmBindCode(ctx, bc.ID, strings.TrimSpace(p.Body.Label), events,
		action(v, "channel.bind.confirm", manage.BindConfirmArgs(c.Name, me, bc, events)))
	switch {
	case errors.Is(err, store.ErrNotRedeemed):
		return MyBinding{}, openapi.Errorf(http.StatusConflict, openapi.CodeConflict, "nobody has sent the code to the bot yet")
	case errors.Is(err, store.ErrBindCode):
		return MyBinding{}, openapi.Errorf(http.StatusConflict, openapi.CodeConflict, "the code expired or was used; start again")
	case errors.Is(err, store.ErrDuplicate):
		return MyBinding{}, openapi.Errorf(http.StatusConflict, openapi.CodeConflict, "this chat is bound on %s already", c.Name)
	case errors.Is(err, store.ErrNotFound):
		return MyBinding{}, openapi.Errorf(http.StatusConflict, openapi.CodeConflict, "channel %s was removed", c.Name)
	case err != nil:
		return MyBinding{}, err
	}
	if a.secrets.Channels != nil {
		if _, err := a.secrets.Channels.Test(context.WithoutCancel(ctx), c.Name, b.ID); err != nil {
			a.log.Warn("api: could not tell the new binding", "binding", b.ID, "err", err)
		}
	}
	a.alertAbout(ctx, v, v.PrincipalID, b.ID, fmt.Sprintf("bound a Telegram chat (%s, chat %s) to %s:%s on channel %s",
		bc.SeenLabel, bc.SeenAddress, me.Kind, me.Name, c.Name))
	bots, _, err := a.botNames(ctx)
	if err != nil {
		return MyBinding{}, err
	}
	return myBinding(b, bots), nil
}

// declineBinding drops a code whose sender the viewer does not
// recognise, or gave up on: nothing was granted, so nothing is recorded.
func (a *API) declineBinding(ctx context.Context, v *auth.Viewer, p IDParams) (struct{}, error) {
	bc, _, err := a.ownBindCode(ctx, v, p.ID)
	if err != nil {
		return struct{}{}, err
	}
	if bc.BindingID != "" {
		return struct{}{}, openapi.Errorf(http.StatusConflict, openapi.CodeConflict, "the code was confirmed; revoke the binding instead")
	}
	return struct{}{}, a.db.DropBindCode(ctx, bc.ID)
}
