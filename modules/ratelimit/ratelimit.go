// Package ratelimit provides a process-local token bucket limiter.
//
// Semantics: LOCAL. Limits apply per process only; with multiple instances the
// effective limit multiplies. It must not be presented as a global limiter.
package ratelimit

import (
	"net"
	"sync"
	"time"
)

// Limiter allows `burst` events immediately and refills at `rate` per second per key.
// Memory is bounded by maxKeys; when full, refilled (idle) buckets are evicted
// and, failing that, a random ~1% of buckets is dropped. Dropping resets those
// keys to a full burst, which is preferable to denying every new client once an
// attacker fills the table (fail-closed would be a trivial denial of service).
type Limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	maxKeys int
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// PerMinute returns a limiter allowing n events per minute with burst n.
func PerMinute(n, maxKeys int) *Limiter {
	return New(float64(n)/60, n, maxKeys)
}

func New(ratePerSec float64, burst, maxKeys int) *Limiter {
	return &Limiter{rate: ratePerSec, burst: float64(burst), maxKeys: maxKeys, buckets: map[string]*bucket{}, now: time.Now}
}

// SetPerMinute retunes a PerMinute limiter live (config hot reload): rate n per
// minute, burst n. Lowering caps existing buckets at the new burst; raising
// credits the burst increase to existing buckets, so both take effect
// immediately. n < 1 is ignored (config validation rejects it).
func (l *Limiter) SetPerMinute(n int) {
	if n < 1 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delta := float64(n) - l.burst
	l.rate, l.burst = float64(n)/60, float64(n)
	for _, b := range l.buckets {
		b.tokens = min(b.tokens+max(delta, 0), l.burst)
	}
}

// Allow consumes one token for key. When denied, retryAfter is the wait until a token is available.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, found := l.buckets[key]
	if !found {
		if len(l.buckets) >= l.maxKeys {
			l.evict(now)
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

// evict drops buckets that have fully refilled (equivalent to absent).
func (l *Limiter) evict(now time.Time) {
	full := time.Duration(l.burst / l.rate * float64(time.Second))
	for k, b := range l.buckets {
		if now.Sub(b.last) >= full {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) < l.maxKeys {
		return
	}
	drop := max(l.maxKeys/100, 1)
	for k := range l.buckets { // map iteration order is randomised
		delete(l.buckets, k)
		if drop--; drop == 0 {
			return
		}
	}
}

// IPKey aggregates IPv6 addresses to their /64 (one subscriber allocation) so a
// single client cannot mint unlimited limiter keys; IPv4 is used as is.
func IPKey(ip string) string {
	p := net.ParseIP(ip)
	if p == nil || p.To4() != nil {
		return ip
	}
	return p.Mask(net.CIDRMask(64, 128)).String() + "/64"
}
