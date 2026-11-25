package ratelimit

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/pillar/gostack/adapters/valkey"
	"github.com/pillar/gostack/core/observability"
)

// bucketLua is the same token bucket as the local Limiter, evaluated
// atomically inside Valkey (scripts run one at a time) with the server clock
// (TIME), so instance clock skew does not matter. The hash stores tokens (t),
// last refill in µs (l) and the burst it was computed with (b), so a raised
// budget is credited immediately and a lowered one caps the bucket, exactly
// like Limiter.SetPerMinute. The key expires once the bucket would be full
// again (+1s). ARGV: refill interval per token (µs), burst.
// Returns {1, 0} when allowed, {0, retry_after_µs} when denied.
const bucketLua = `
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000000 + tonumber(t[2])
local interval = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local v = redis.call('HMGET', KEYS[1], 't', 'l', 'b')
local tokens, last, oldb = tonumber(v[1]), tonumber(v[2]), tonumber(v[3])
if not tokens or not last or not oldb then tokens, last, oldb = burst, now, burst end
if burst > oldb then tokens = tokens + (burst - oldb) end
if now > last then
  tokens = tokens + (now - last) / interval
  last = now
end
if tokens > burst then tokens = burst end
local ok, retry = 0, 0
if tokens >= 1 then
  tokens = tokens - 1
  ok = 1
else
  retry = math.ceil((1 - tokens) * interval)
end
redis.call('HSET', KEYS[1], 't', string.format('%.17g', tokens), 'l', string.format('%.0f', last), 'b', ARGV[2])
redis.call('PEXPIRE', KEYS[1], math.ceil((burst - tokens) * interval / 1000) + 1000)
return {ok, retry}
`

var bucketScript = valkey.NewScript(bucketLua)

// FailClosedRetry is the Retry-After returned when a FailClosed limiter
// cannot reach its backend.
const FailClosedRetry = time.Second

// Global is a limiter shared by every instance using the same Valkey server and
// key prefix: the combined allowance across instances is perMinute per key.
// Each check is one EVALSHA bounded by Timeout; on error (timeout, connection
// refused, server error) it applies its OnFailure policy, counts
// ratelimit_global_errors_total{limiter,on_failure} and logs at most one
// warning per 10s.
type Global struct {
	client    *valkey.Client
	prefix    string // <app prefix>rl:<name>:
	name      string
	perMinute atomic.Int64
	onFailure OnFailure
	timeout   time.Duration
	fallback  *Limiter // FailLocal only
	denied    *atomic.Int64
	errs      *atomic.Int64
	log       *slog.Logger
	lastWarn  atomic.Int64
}

// GlobalOptions are shared by all limiters built by GlobalFactory.
type GlobalOptions struct {
	Client  *valkey.Client
	Prefix  string        // application key prefix, e.g. "mygame:"
	Timeout time.Duration // per check (default 50ms)
	Metrics *observability.Registry
	Log     *slog.Logger
}

// NewGlobal builds one global limiter.
func NewGlobal(o GlobalOptions, name string, perMinute int, onFailure OnFailure) *Global {
	if o.Timeout <= 0 {
		o.Timeout = 50 * time.Millisecond
	}
	if o.Metrics == nil {
		o.Metrics = observability.NewRegistry()
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	g := &Global{client: o.Client, prefix: o.Prefix + "rl:" + name + ":", name: name, onFailure: onFailure,
		timeout: o.Timeout, log: o.Log,
		denied: o.Metrics.Counter("ratelimit_global_denied_total", `limiter="`+name+`"`),
		errs:   o.Metrics.Counter("ratelimit_global_errors_total", fmt.Sprintf(`limiter=%q,on_failure=%q`, name, onFailure))}
	g.perMinute.Store(int64(max(perMinute, 1)))
	if onFailure == FailLocal {
		g.fallback = PerMinute(max(perMinute, 1), LocalMaxKeys)
	}
	return g
}

// GlobalFactory returns a Factory of Valkey-backed limiters.
func GlobalFactory(o GlobalOptions) Factory {
	return func(name string, perMinute int, onFailure OnFailure) Allower {
		return NewGlobal(o, name, perMinute, onFailure)
	}
}

// SetPerMinute changes the budget from the next check of each key: raising
// credits the increase, lowering caps the stored tokens (as Limiter does).
func (g *Global) SetPerMinute(n int) {
	if n < 1 {
		return
	}
	g.perMinute.Store(int64(n))
	if g.fallback != nil {
		g.fallback.SetPerMinute(n)
	}
}

// Allow implements Allower.
func (g *Global) Allow(key string) (bool, time.Duration) {
	ok, retry, err := g.check(context.Background(), key)
	if err == nil {
		if !ok {
			g.denied.Add(1)
		}
		return ok, retry
	}
	g.errs.Add(1)
	if now := time.Now().Unix(); now-g.lastWarn.Load() >= 10 && g.lastWarn.Swap(now) != now {
		g.log.Warn("global rate limiter unavailable; applying failure policy", "limiter", g.name,
			"on_failure", g.onFailure.String(), "err", err)
	}
	switch g.onFailure {
	case FailOpen:
		return true, 0
	case FailClosed:
		return false, FailClosedRetry
	}
	return g.fallback.Allow(key)
}

// check runs the bucket script; it reports backend errors instead of a decision.
func (g *Global) check(ctx context.Context, key string) (bool, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	n := g.perMinute.Load()
	interval := max(int64(time.Minute/time.Microsecond)/n, 1)
	r, err := bucketScript.Run(ctx, g.client, []string{g.prefix + key}, strconv.FormatInt(interval, 10), strconv.FormatInt(n, 10))
	if err != nil {
		return false, 0, err
	}
	if len(r.Array) != 2 {
		return false, 0, fmt.Errorf("ratelimit: unexpected script reply %+v", r)
	}
	if r.Array[0].Int == 1 {
		return true, 0, nil
	}
	return false, time.Duration(r.Array[1].Int) * time.Microsecond, nil
}
