package app

import (
	"context"
	"fmt"
	"time"

	"github.com/pillar/gostack/adapters/valkey"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/modules/cache"
	"github.com/pillar/gostack/modules/ratelimit"
)

// initValkey builds App.Cache and App.Limits from cache.driver and
// rate_limit.driver, connecting to Valkey when either needs it. Startup fails
// when Valkey is configured but unreachable (a misconfiguration should not
// start silently degraded); at runtime an outage is handled per use: cache
// errors are misses and each limiter applies its failure policy. /readyz does
// not depend on Valkey, so an outage does not take every instance out of the
// load balancer.
func (a *App) initValkey(ctx context.Context, cfg config.Config) error {
	if cfg.UsesValkey() {
		v := cfg.Valkey
		var pw string
		if v.PasswordRef != "" {
			var err error
			if pw, err = config.ResolveSecret(v.PasswordRef); err != nil {
				return fmt.Errorf("valkey.password_ref: %w", err)
			}
		}
		// The client's own cap is generous; each use passes its shorter timeout.
		a.Valkey = valkey.New(valkey.Options{Addr: v.Address, Username: v.Username, Password: pw, DB: v.DB,
			DialTimeout: v.DialTimeout, Timeout: 5 * time.Second, PoolSize: v.PoolSize})
		pctx, cancel := context.WithTimeout(ctx, v.DialTimeout+time.Second)
		err := a.Valkey.Ping(pctx)
		cancel()
		if err != nil {
			return fmt.Errorf("valkey %s: %w", v.Address, err)
		}
	}
	switch cfg.Cache.Driver {
	case "valkey":
		a.Cache = cache.WithMetrics(&cache.Valkey{Client: a.Valkey, Prefix: cfg.Valkey.KeyPrefix,
			MaxValueBytes: cfg.Cache.MaxValueBytes, Timeout: cfg.Cache.Timeout}, a.Metrics, "valkey")
	default:
		m := cache.NewMemory(cfg.Cache.MaxBytes, cfg.Cache.MaxValueBytes)
		a.Cache = cache.WithMetrics(m, a.Metrics, "memory")
		a.Metrics.Gauge("cache_memory_bytes", func() float64 { b, _, _ := m.Stats(); return float64(b) })
		a.Metrics.Gauge("cache_memory_entries", func() float64 { _, n, _ := m.Stats(); return float64(n) })
		a.Metrics.Gauge("cache_memory_evictions_total", func() float64 { _, _, n := m.Stats(); return float64(n) })
	}
	switch cfg.RateLimit.Driver {
	case "valkey":
		a.Limits = ratelimit.GlobalFactory(ratelimit.GlobalOptions{Client: a.Valkey, Prefix: cfg.Valkey.KeyPrefix,
			Timeout: cfg.RateLimit.Timeout, Metrics: a.Metrics, Log: a.Log})
	default:
		a.Limits = ratelimit.LocalFactory(a.Metrics)
	}
	return nil
}
