// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 muzimu217

package forward

import (
	"errors"
	"sync"
	"time"
)

// ErrTooMany is the sentinel for every limit refusal. Callers map it to
// wire.StatusTooMany.
var ErrTooMany = errors.New("forward: limit reached")

// tokenBucket is a hand-written rate limiter.
//
// Why not a library: this needs about forty lines and one behaviour. golang.org/x/time/rate
// would add a dependency for a monotonic clock wrapper this package does not
// need, and the project's rule is that a dependency must earn its place.
//
// Why it exists at all: MaxStreams bounds *concurrency*, not *rate*. An
// attacker inside that cap can open and close thousands of streams per second,
// which is exactly what scanning a /16 for an open port looks like. Without
// this layer the concurrency limit does not slow a scan down at all.
type tokenBucket struct {
	mu       sync.Mutex
	capacity float64 // burst: how many may start at once
	tokens   float64
	perSec   float64
	last     time.Time
	now      func() time.Time // injectable for tests
}

// newTokenBucket builds a bucket that allows `perSec` new streams per second
// with a burst of `burst`.
func newTokenBucket(perSec, burst float64) *tokenBucket {
	if perSec <= 0 {
		perSec = 1
	}
	if burst < 1 {
		burst = 1
	}
	return &tokenBucket{
		capacity: burst,
		tokens:   burst,
		perSec:   perSec,
		last:     time.Now(),
		now:      time.Now,
	}
}

// Allow consumes one token, reporting whether one was available.
//
// It never blocks. A forwarded connection that has to wait for a rate limit is
// worse than one that is refused: the client has already connected and is
// waiting for a reply, so an immediate StatusTooMany is the honest answer.
func (b *tokenBucket) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() * b.perSec
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Limits bounds what one peer can consume.
type Limits struct {
	// MaxPending bounds local TCP connections accepted but not yet handed a
	// mux stream. Without it, a peer that stops answering lets accepted
	// connections pile up in the kernel and in our goroutines.
	MaxPending int
	// MaxStreamsPerSec bounds how fast new forwarded streams may be created.
	// Zero disables the limit (tests and single-shot use).
	MaxStreamsPerSec int
	// HandshakeTimeout bounds how long a stream may take to send its preamble.
	// This is the defence against "connect, then say nothing": the stream would
	// otherwise hold a slot and 2x the yamux window forever.
	HandshakeTimeout time.Duration
	// PipeDrain bounds how long a half-closed pipe waits for the other side to
	// finish before it is reset. Without it, a peer that never closes leaks a
	// goroutine and 2x the window per abandoned forward.
	PipeDrain time.Duration
	// DialTimeout bounds the outbound connect.
	DialTimeout time.Duration
}

// DefaultLimits returns the production values.
func DefaultLimits() Limits {
	return Limits{
		MaxPending:       128,
		MaxStreamsPerSec: 50,
		HandshakeTimeout: 10 * time.Second,
		PipeDrain:        5 * time.Second,
		DialTimeout:      5 * time.Second,
	}
}

// normalise fills zero fields with defaults and clamps negatives to zero.
func (l Limits) normalise() Limits {
	d := DefaultLimits()
	if l.MaxPending <= 0 {
		l.MaxPending = d.MaxPending
	}
	if l.MaxStreamsPerSec < 0 {
		l.MaxStreamsPerSec = 0
	}
	if l.HandshakeTimeout <= 0 {
		l.HandshakeTimeout = d.HandshakeTimeout
	}
	if l.PipeDrain <= 0 {
		l.PipeDrain = d.PipeDrain
	}
	if l.DialTimeout <= 0 {
		l.DialTimeout = d.DialTimeout
	}
	return l
}

// pendingGate bounds concurrent in-flight local connections.
//
// It is a counting semaphore rather than a queue: when it is full the honest
// answer is "too many", because the client is already connected and waiting.
type pendingGate struct {
	slots chan struct{}
}

func newPendingGate(n int) *pendingGate {
	if n <= 0 {
		n = 1
	}
	return &pendingGate{slots: make(chan struct{}, n)}
}

// Acquire takes a slot, reporting false when none is free.
func (g *pendingGate) Acquire() bool {
	select {
	case g.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release returns a slot. It must be called exactly once per successful
// Acquire.
func (g *pendingGate) Release() { <-g.slots }

// InUse reports how many slots are taken, for tests and metrics.
func (g *pendingGate) InUse() int { return len(g.slots) }
