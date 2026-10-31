package outbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yuuka-mail/yuuka/internal/secret"
	"github.com/yuuka-mail/yuuka/internal/store"
	"github.com/yuuka-mail/yuuka/internal/transport"
)

// Which transports the executor can send through (§10.2.2, §17.5). Every
// claim resolves every transport from the database: there is no reload
// signal and no notification, so a change takes effect at the next claim.
// What is cached is the client built from a configured transport, keyed
// by its revision and its secret's, so a secret is opened once per
// revision rather than once per message. Pauses are kept by transport id
// and end when either revision changes: a fixed password is tried at the
// next claim, without a restart and without waiting the pause out.

// lockedReport is how often the executor logs, as an error, the
// transports it cannot use because of their secrets (§17.3), and the
// messages that wait for want of a usable transport.
const lockedReport = 10 * time.Minute

// SecurityAlerter raises a security alert (§8.4) when a transport's
// secret is not what was sealed: it fails to authenticate (tampered or
// corrupted), or it was sealed for another destination than the row now
// names, which the store never writes, so the database was written around
// it (§17.2). The transport is locked either way. It is called once per
// revision of the transport and its secret, from a worker, and must
// return promptly.
type SecurityAlerter interface {
	TransportSecretAlert(ctx context.Context, transport string, err error)
}

// version is what a client built from a transport row is valid for.
type version struct {
	rev, secretRev int64
}

// resolved is one transport as a claim sees it.
type resolved struct {
	id, name string
	version  version
	// tr is nil when the transport cannot be used; why then says why,
	// in words fit for outbox.error, which names no secret, wrapper or key.
	tr  Transport
	why string
	// locked: it cannot be used because of its secret. detail says more,
	// for the log only.
	locked bool
	detail string
}

// routes is every transport resolved for one claim.
type routes struct {
	byID map[string]resolved
	// usable are the ones to claim through: usable and not paused.
	usable []store.UsableTransport
	// live are the ids of the usable ones, paused or not. A row whose
	// chain has one waits for it rather than being parked.
	live []string
	// why says, by id, why each of the others cannot be used.
	why map[string]string
}

// resolve reads every transport and resolves it: a configured row through
// its secret, a placeholder through the environment's configuration of
// the same name (§17.7), a disabled one not at all.
func (s *Service) resolve(ctx context.Context) (*routes, error) {
	ts, err := s.cfg.DB.SendingTransports(ctx)
	if err != nil {
		return nil, err
	}
	rs := &routes{byID: make(map[string]resolved, len(ts)), why: map[string]string{}}
	var locked []string
	for _, t := range ts {
		r := s.resolveOne(ctx, t)
		rs.byID[r.id] = r
		if r.tr == nil {
			rs.why[r.id] = r.why
			if r.locked {
				locked = append(locked, r.name+": "+r.detail)
			}
			continue
		}
		rs.live = append(rs.live, r.id)
		if !s.paused(r) {
			rs.usable = append(rs.usable, store.UsableTransport{ID: r.id, Revision: r.version.rev, SecretRevision: r.version.secretRev})
		}
	}
	s.forget(rs.byID)
	s.reportLocked(locked)
	return rs, nil
}

func (s *Service) resolveOne(ctx context.Context, t store.SendingTransport) resolved {
	r := resolved{id: t.ID, name: t.Name, version: version{t.Revision, t.SecretRevision()}}
	switch {
	case t.DisabledAt != nil:
		r.why = "disabled"
		return r
	case t.ConfiguredAt == nil:
		// The deprecation period of §17.7: the environment may still
		// configure a placeholder's name. Once the row is configured, the
		// row is what is used.
		if tr, ok := s.cfg.Transports[t.Name]; ok {
			r.tr = tr
			return r
		}
		r.why = "not configured"
		return r
	}
	s.mu.Lock()
	c, ok := s.clients[t.ID]
	s.mu.Unlock()
	if ok && c.version == r.version {
		return c
	}
	r, keep := s.build(ctx, t, r)
	if keep {
		s.mu.Lock()
		s.clients[t.ID] = r
		s.mu.Unlock()
	}
	return r
}

// build makes the client of configured transport t, opening its secret
// for the destination recomputed from the row, never for the audience
// stored with the secret (§17.2). keep says whether the result holds for
// as long as the revisions do: a missing key can come back (a remote
// wrapper reachable again), a secret that does not authenticate cannot.
func (s *Service) build(ctx context.Context, t store.SendingTransport, r resolved) (resolved, bool) {
	cfg := transport.Config{Name: t.Name, Host: t.Host, Port: t.Port, Caps: capsOf(t.Caps)}
	if t.Auth == store.TransportAuthPlain {
		if t.Secret == nil {
			r.why = "has no password; set it again"
			return r, true
		}
		aud, err := secret.SMTPS(t.Username, t.Host, t.Port)
		if err != nil {
			r.why = "misconfigured: " + err.Error()
			return r, true
		}
		v, err := s.cfg.Sealer.Open(ctx, t.Secret.Record, aud)
		if err != nil {
			return s.unopened(ctx, t, r, err)
		}
		cfg.Username, cfg.Password = t.Username, string(v.Reveal())
		v.Close()
	}
	tr, err := s.cfg.NewTransport(cfg)
	if err != nil {
		r.why = "misconfigured: " + err.Error()
		return r, true
	}
	r.tr = tr
	return r, true
}

// unopened locks transport t whose secret did not open with err (§17.3).
func (s *Service) unopened(ctx context.Context, t store.SendingTransport, r resolved, err error) (resolved, bool) {
	r.locked = true
	rec := t.Secret.Record
	r.detail = fmt.Sprintf("%v (secret %s, wrapper %s, key %s)", err, rec.ID, rec.Wrapper, rec.KeyRef)
	switch {
	case errors.Is(err, secret.ErrLocked):
		r.why = "locked: the key its password was sealed under is not loaded"
		if s.cfg.Sealer == nil {
			r.why = "locked: no key to open its password (YUUKA_KEK_FILE)"
		}
		return r, false
	case errors.Is(err, secret.ErrTampered), errors.Is(err, secret.ErrAudienceMismatch):
		if errors.Is(err, secret.ErrTampered) {
			r.why = "locked: its password failed authentication; set it again"
		} else {
			r.why = "locked: its password was sealed for another destination; set it again"
		}
		s.log.Error("transport locked: its secret is not what was sealed for it; set it again and find out who "+
			"wrote the database", "transport", t.Name, "detail", r.detail)
		if s.cfg.Alerter != nil {
			s.cfg.Alerter.TransportSecretAlert(ctx, t.Name, err)
		}
		return r, true
	case errors.Is(err, secret.ErrUnsupportedVersion):
		r.why = "locked: its password was sealed by a newer yuuka"
		return r, true
	}
	// A wrapper's transient error: try again at the next claim.
	r.why = "locked: its password cannot be opened right now"
	return r, false
}

// paused reports whether r is paused. A pause ends when the transport or
// its secret changes revision.
func (s *Service) paused(r resolved) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pauses[r.id]
	if ok && p.version != r.version {
		delete(s.pauses, r.id)
		return false
	}
	return ok && p.until.After(s.cfg.Now())
}

// forget drops the clients and pauses of transports that are gone or have
// changed revision.
func (s *Service) forget(byID map[string]resolved) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.clients {
		if r, ok := byID[id]; !ok || r.version != c.version {
			delete(s.clients, id)
		}
	}
	for id, p := range s.pauses {
		if r, ok := byID[id]; !ok || r.version != p.version {
			delete(s.pauses, id)
		}
	}
}

// reportLocked logs the locked transports as an error when the set first
// becomes non-empty and every lockedReport while it stays so (§17.3).
func (s *Service) reportLocked(locked []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now()
	if len(locked) == 0 {
		s.lockedAt = time.Time{}
		return
	}
	if !s.lockedAt.IsZero() && now.Sub(s.lockedAt) < lockedReport {
		return
	}
	s.lockedAt = now
	slices.Sort(locked)
	s.log.Error("transports locked: mail that only they can send waits in the queue; "+
		"set YUUKA_KEK_FILE to the key their passwords were sealed under, or set the passwords again",
		"locked", strings.Join(locked, "; "))
}

// pick returns the first member of c's chain whose caps allow raw as it
// is, so the exit gets the stored bytes (§10.2.2). The chain's caps
// decided them at enqueue; a member may have narrowed its caps since, or
// joined the chain, or been a placeholder then.
func (rs *routes) pick(c *store.Claimed, raw []byte) (resolved, bool) {
	for _, m := range c.Chain {
		r := rs.byID[m.ID]
		if r.tr != nil && bytes.Equal(transport.StripHeaders(raw, r.tr.Caps()), raw) {
			return r, true
		}
	}
	return resolved{}, false
}

// Caps are the caps a message from identity identityID is prepared with:
// the strictest across the members of its chain that are configured and
// enabled (§10.2.2). A header field that any of them does not let the
// caller set is not set, so the bytes can go through whichever member
// sends them. Placeholders the environment configures count (§17.7), and
// so do locked transports: caps are not secret, and a locked primary is
// what the chain is for. StatusQuery is always false: it changes no
// bytes, and whether a send can be reconciled depends on the transport
// that made it. ErrUnconfigured if no member counts: which header fields
// to strip cannot be decided, so the bytes cannot be prepared.
func (s *Service) Caps(ctx context.Context, identityID string) (transport.Caps, error) {
	chain, err := s.cfg.DB.IdentityTransports(ctx, identityID)
	if err != nil {
		return transport.Caps{}, err
	}
	var out transport.Caps
	n := 0
	names := make([]string, 0, len(chain))
	for _, t := range chain {
		names = append(names, t.Name)
		var c transport.Caps
		switch {
		case t.DisabledAt != nil:
			continue
		case t.ConfiguredAt != nil:
			c = capsOf(t.Caps)
		default:
			tr, ok := s.cfg.Transports[t.Name]
			if !ok {
				continue
			}
			c = tr.Caps()
		}
		if n == 0 {
			out = c
		} else {
			out.CallerMessageID = out.CallerMessageID && c.CallerMessageID
			out.CallerReturnPath = out.CallerReturnPath && c.CallerReturnPath
			out.CallerDKIM = out.CallerDKIM && c.CallerDKIM
		}
		n++
	}
	if n == 0 {
		return transport.Caps{}, fmt.Errorf("%w: identity %s: none of %s", ErrUnconfigured, identityID, strings.Join(names, ", "))
	}
	out.StatusQuery = false
	return out, nil
}

// capsOf reads transports.caps.
func capsOf(names []string) transport.Caps {
	return transport.Caps{
		CallerMessageID:  slices.Contains(names, "message-id"),
		CallerReturnPath: slices.Contains(names, "return-path"),
		CallerDKIM:       slices.Contains(names, "dkim"),
		StatusQuery:      slices.Contains(names, "status-query"),
	}
}
