package ratelimit

import (
	"sync/atomic"
	"time"

	"github.com/pillar/gostack/core/observability"
)

// Allower is a per-key request budget of n events per minute (burst n).
type Allower interface {
	// Allow consumes one event for key; when denied, retryAfter is the wait
	// until the next event would be allowed. It never blocks for long: a
	// global limiter bounds its backend call by a short timeout.
	Allow(key string) (ok bool, retryAfter time.Duration)
	// SetPerMinute retunes the budget (config hot reload); n < 1 is ignored.
	SetPerMinute(n int)
}

// OnFailure is what a global limiter does when its backend (Valkey) cannot
// answer within the timeout. A local limiter never fails.
type OnFailure int

const (
	// FailLocal falls back to a process-local bucket with the same budget, i.e.
	// the guarantee degrades to "limit per instance" (what profile scale had
	// before the global limiter) instead of unlimited or unavailable.
	FailLocal OnFailure = iota
	// FailOpen allows the request.
	FailOpen
	// FailClosed denies the request (429 with a short Retry-After).
	FailClosed
)

func (p OnFailure) String() string {
	switch p {
	case FailOpen:
		return "open"
	case FailClosed:
		return "closed"
	}
	return "local"
}

// Factory builds the limiter named name (unique per use; it scopes keys and
// labels metrics) with a budget of perMinute and the given failure policy.
type Factory func(name string, perMinute int, onFailure OnFailure) Allower

// LocalMaxKeys bounds each local limiter's key table.
const LocalMaxKeys = 100_000

// LocalFactory returns process-local limiters. reg may be nil; otherwise
// denials are counted as ratelimit_local_denied_total{limiter="name"}.
func LocalFactory(reg *observability.Registry) Factory {
	return func(name string, perMinute int, _ OnFailure) Allower {
		l := PerMinute(perMinute, LocalMaxKeys)
		if reg == nil {
			return l
		}
		return &observedLocal{Limiter: l, denied: reg.Counter("ratelimit_local_denied_total", `limiter="`+name+`"`)}
	}
}

type observedLocal struct {
	*Limiter
	denied *atomic.Int64
}

func (o *observedLocal) Allow(key string) (bool, time.Duration) {
	ok, retry := o.Limiter.Allow(key)
	if !ok {
		o.denied.Add(1)
	}
	return ok, retry
}

// OrLocal returns f, or an unobserved local factory when f is nil.
func OrLocal(f Factory) Factory {
	if f != nil {
		return f
	}
	return LocalFactory(nil)
}
