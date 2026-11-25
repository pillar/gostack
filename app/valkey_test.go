package app

import (
	"context"
	"strings"
	"testing"
	"time"
)

// App.Cache follows cache.driver (memory on SQLite; valkey when the PG+Valkey
// suite enables it) and its metrics state the driver; limiter metrics state
// the scope.
func TestCacheAndLimiterWiring(t *testing.T) {
	c := testConfig(t)
	a := newTestApp(t, c)
	ctx := context.Background()
	if err := a.Cache.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := a.Cache.Get(ctx, "k"); err != nil || !ok || string(v) != "v" {
		t.Fatalf("%q %v %v", v, ok, err)
	}
	if a.Limits == nil {
		t.Fatal("no limiter factory")
	}
	l := a.Limits("app_test", 1, 0)
	l.Allow("x")
	l.Allow("x")
	var sb strings.Builder
	a.Metrics.Render(&sb)
	out := sb.String()
	want := `cache_hits_total{driver="` + c.Cache.Driver + `"} 1`
	scope := map[string]string{"local": "ratelimit_local_denied_total", "valkey": "ratelimit_global_denied_total"}[c.RateLimit.Driver]
	if !strings.Contains(out, want) || !strings.Contains(out, scope+`{limiter="app_test"} 1`) {
		t.Fatalf("metrics missing %q / %s:\n%s", want, scope, out)
	}
	if c.UsesValkey() != (a.Valkey != nil) {
		t.Fatal("valkey client wiring mismatch")
	}
}
