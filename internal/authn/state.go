package authn

import (
	"crypto/rand"
	"errors"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

// errBusy: too many ceremonies in flight.
var errBusy = errors.New("too many pending ceremonies")

// ceremony is the server side of one WebAuthn exchange between begin and
// finish: above all the challenge, which must be the one we issued and
// must be usable once.
type ceremony struct {
	register bool
	data     webauthn.SessionData
	expires  time.Time

	// Registration only: whose passkey, and what authorized it — an
	// enrollment token or an existing session.
	principalID  string
	tokenID      string
	tokenReason  string
	credentialID string
	label        string
}

// ceremonies keeps pending ceremonies in memory, keyed by the value of an
// HttpOnly cookie. M1: a restart only means the user retries; if yuuka
// ever runs as several replicas this moves to Postgres.
type ceremonies struct {
	mu  sync.Mutex
	m   map[string]ceremony
	max int
}

func newCeremonies(maxPending int) *ceremonies {
	return &ceremonies{m: map[string]ceremony{}, max: maxPending}
}

func (c *ceremonies) put(cer ceremony, now time.Time) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.max {
		for k, v := range c.m {
			if !now.Before(v.expires) {
				delete(c.m, k)
			}
		}
		if len(c.m) >= c.max {
			return "", errBusy
		}
	}
	id := rand.Text()
	c.m[id] = cer
	return id, nil
}

// take removes and returns a ceremony: whatever the outcome of finish, a
// challenge is never accepted twice.
func (c *ceremonies) take(id string, now time.Time) (ceremony, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cer, ok := c.m[id]
	delete(c.m, id)
	if !ok || !now.Before(cer.expires) {
		return ceremony{}, false
	}
	return cer, true
}

// limiter is a token bucket per key: burst requests at once, then one
// per every.
type limiter struct {
	mu      sync.Mutex
	burst   float64
	every   time.Duration
	now     func() time.Time
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	at     time.Time
}

// maxBuckets bounds memory; past it, full (idle) buckets are dropped.
const maxBuckets = 10000

func newLimiter(burst int, every time.Duration, now func() time.Time) *limiter {
	return &limiter{burst: float64(burst), every: every, now: now, buckets: map[string]*bucket{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= maxBuckets {
			l.prune(now)
		}
		b = &bucket{tokens: l.burst, at: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+float64(now.Sub(b.at))/float64(l.every))
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *limiter) prune(now time.Time) {
	for k, b := range l.buckets {
		if b.tokens+float64(now.Sub(b.at))/float64(l.every) >= l.burst {
			delete(l.buckets, k)
		}
	}
}
