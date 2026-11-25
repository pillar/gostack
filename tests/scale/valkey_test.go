package scale_test

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pillar/gostack/adapters/pgtest"
	"github.com/pillar/gostack/adapters/valkeytest"
	"github.com/pillar/gostack/app"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/kits/game/gametest"
)

// Two instances on one PostgreSQL database and one Valkey: the guest-auth
// per-IP budget is enforced across both (rate_limit.driver valkey), whereas
// with driver local each instance would allow the full budget.
func TestTwoInstancesGlobalAuthLimit(t *testing.T) {
	pgtest.RequirePostgres(t)
	valkeytest.Require(t)
	const budget = 12
	a := gametest.New(t, gametest.Options{Mutate: func(c *config.Config) { c.Game.AuthRatePerMinute = budget }})
	if a.Cfg.RateLimit.Driver != "valkey" || a.Cfg.Cache.Driver != "valkey" {
		t.Fatalf("expected valkey drivers, got rate_limit=%s cache=%s", a.Cfg.RateLimit.Driver, a.Cfg.Cache.Driver)
	}
	b := a.Instance(t)
	envs := []*gametest.Env{a, b}
	start := time.Now()
	st := fanOut(4*budget, envs, func(e *gametest.Env, i int) int {
		return e.Do("POST", "/v1/games/alpha/auth/guest", "", "{}").Status
	})
	// Refill is budget/min: at most one extra token per 60s/budget elapsed.
	maxOK := budget + int(time.Since(start)/(time.Minute/budget))
	if ok := count(st, 201); ok < budget || ok > maxOK {
		t.Fatalf("guest logins across two instances: %d succeeded, want %d..%d (statuses %v)", ok, budget, maxOK, st)
	}
	t.Logf("%d requests over two instances: %d allowed, %d rate limited", len(st), count(st, 201), count(st, 429))
	if count(st, 201)+count(st, 429) != len(st) {
		t.Fatalf("unexpected statuses %v", st)
	}
	// Both instances observed the shared budget (denials happened on each).
	for i, e := range envs {
		if n := e.App.Metrics.Counter("ratelimit_global_denied_total", `limiter="game_auth"`).Load(); n == 0 {
			t.Errorf("instance %d recorded no global denials", i)
		}
		if n := e.App.Metrics.Counter("ratelimit_global_errors_total", `limiter="game_auth",on_failure="local"`).Load(); n != 0 {
			t.Errorf("instance %d: %d backend errors", i, n)
		}
	}
	// The shared cache is visible from the other instance.
	ctx := context.Background()
	if err := a.App.Cache.Set(ctx, "greeting", []byte("hi"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := b.App.Cache.Get(ctx, "greeting"); err != nil || !ok || string(v) != "hi" {
		t.Fatalf("cross-instance cache: %q %v %v", v, ok, err)
	}
}

// A configured but unreachable Valkey refuses startup instead of running
// silently degraded.
func TestValkeyUnreachableAtStartup(t *testing.T) {
	pgtest.RequirePostgres(t)
	cfg := config.Defaults()
	dir := t.TempDir()
	cfg.Database.Path, cfg.Database.AnalyticsPath = dir+"/a.db", dir+"/b.db"
	pgtest.UseScale(t, &cfg)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	cfg.Valkey.Address = ln.Addr().String()
	ln.Close()
	cfg.RateLimit.Driver, cfg.RateLimit.AcceptPerInstance = "valkey", false
	cfg.Valkey.KeyPrefix = "unreach:"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	a, err := app.New(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err == nil {
		a.Close()
		t.Fatal("started without Valkey")
	}
	if !strings.Contains(err.Error(), "valkey") {
		t.Fatalf("error does not name valkey: %v", err)
	}
}
