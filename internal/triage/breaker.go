package triage

import (
	"sync"
	"time"
)

// breaker stops calling a scorer that keeps failing. After tripAfter
// failures in a row it opens for a cool-down; then one probe goes
// through, and its failure doubles the cool-down, up to coolMax, while
// any answer closes it. The state is in memory: a restart starts
// closed, which costs at most tripAfter failed calls.
type breaker struct {
	mu        sync.Mutex
	fails     int
	openUntil time.Time // zero while closed
	cool      time.Duration
	probing   bool
}

const (
	tripAfter = 5
	coolMin   = 30 * time.Second
	coolMax   = 10 * time.Minute
)

// allow says whether a call may go out now. While open, the first call
// after the cool-down is let through as the probe.
func (b *breaker) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.openUntil.IsZero():
		return true
	case b.probing || now.Before(b.openUntil):
		return false
	}
	b.probing = true
	return true
}

// isOpen: calls are not going out now, nor is a probe due.
func (b *breaker) isOpen(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.openUntil.IsZero() && (b.probing || now.Before(b.openUntil))
}

func (b *breaker) success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails, b.openUntil, b.cool, b.probing = 0, time.Time{}, 0, false
}

func (b *breaker) failure(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails++
	switch {
	case b.probing:
		b.probing = false
		b.cool = min(b.cool*2, coolMax)
		b.openUntil = now.Add(b.cool)
	case b.openUntil.IsZero() && b.fails >= tripAfter:
		b.cool = coolMin
		b.openUntil = now.Add(b.cool)
	}
}

// release ends an allowed call that taught nothing about the scorer
// (cancelled, or failed on our side), so that a probe that was not one
// does not hold the breaker open.
func (b *breaker) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
}
