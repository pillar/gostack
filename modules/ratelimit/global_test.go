package ratelimit

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pillar/gostack/adapters/valkey"
	"github.com/pillar/gostack/adapters/valkeytest"
	"github.com/pillar/gostack/core/observability"
)

func globalOpts(t *testing.T, addr, prefix string) GlobalOptions {
	c := valkey.New(valkey.Options{Addr: addr, Timeout: 2 * time.Second, PoolSize: 32})
	t.Cleanup(func() { c.Close() })
	return GlobalOptions{Client: c, Prefix: prefix, Timeout: time.Second, Metrics: observability.NewRegistry(), Log: slog.New(slog.DiscardHandler)}
}

func TestGlobalBudgetRetryAndTTL(t *testing.T) {
	addr := valkeytest.Require(t)
	o := globalOpts(t, addr, valkeytest.Prefix(t))
	g := NewGlobal(o, "t", 60, FailClosed) // 1/s refill, burst 60
	for i := range 60 {
		if ok, _ := g.Allow("k"); !ok {
			t.Fatalf("request %d denied within burst", i)
		}
	}
	ok, retry := g.Allow("k")
	if ok || retry <= 0 || retry > time.Second {
		t.Fatalf("over budget: ok=%v retry=%v", ok, retry)
	}
	if ok, _ := g.Allow("other"); !ok {
		t.Fatal("keys are not independent")
	}
	if g.denied.Load() != 1 || g.errs.Load() != 0 {
		t.Fatalf("denied=%d errors=%d", g.denied.Load(), g.errs.Load())
	}
	// The key expires once the bucket would be full again (~60s + 1s slack).
	r, err := o.Client.Do(context.Background(), "PTTL", o.Prefix+"rl:t:k")
	if err != nil || r.Int < 59_000 || r.Int > 62_000 {
		t.Fatalf("pttl %+v %v", r, err)
	}
	time.Sleep(retry + 50*time.Millisecond)
	if ok, _ := g.Allow("k"); !ok {
		t.Fatal("not refilled after Retry-After")
	}
	// A fast limiter's key disappears shortly after its bucket refills.
	f := NewGlobal(o, "fast", 6000, FailClosed) // 10ms interval
	f.Allow("x")
	time.Sleep(1200 * time.Millisecond)
	if r, _ := o.Client.Do(context.Background(), "EXISTS", o.Prefix+"rl:fast:x"); r.Int != 0 {
		t.Fatal("limiter key did not expire")
	}
}

// Many goroutines on two limiter instances (separate clients, as two app
// processes would have) sharing one key never exceed the combined budget.
func TestGlobalConcurrentInstancesShareBudget(t *testing.T) {
	addr := valkeytest.Require(t)
	prefix := valkeytest.Prefix(t)
	const budget = 50
	a := NewGlobal(globalOpts(t, addr, prefix), "shared", budget, FailClosed)
	b := NewGlobal(globalOpts(t, addr, prefix), "shared", budget, FailClosed)
	var allowed atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for i := range 400 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l := a
			if i%2 == 1 {
				l = b
			}
			if ok, _ := l.Allow("ip"); ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	// Refill is 50/min: at most one extra token per 1.2s elapsed.
	maxAllowed := budget + int64(time.Since(start)/(1200*time.Millisecond))
	if n := allowed.Load(); n < budget || n > maxAllowed {
		t.Fatalf("allowed %d across two instances, want %d..%d", n, budget, maxAllowed)
	}
	if e := a.errs.Load() + b.errs.Load(); e != 0 {
		t.Fatalf("%d backend errors", e)
	}
	// Different limiter names do not share keys.
	c := NewGlobal(globalOpts(t, addr, prefix), "other", budget, FailClosed)
	if ok, _ := c.Allow("ip"); !ok {
		t.Fatal("limiter names collide")
	}
}

func TestGlobalSetPerMinute(t *testing.T) {
	addr := valkeytest.Require(t)
	g := NewGlobal(globalOpts(t, addr, valkeytest.Prefix(t)), "r", 2, FailClosed)
	g.Allow("k")
	g.Allow("k")
	if ok, _ := g.Allow("k"); ok {
		t.Fatal("budget 2 exceeded")
	}
	count := func() int {
		n := 0
		for range 200 {
			if ok, _ := g.Allow("k"); ok {
				n++
			}
		}
		return n
	}
	g.SetPerMinute(5) // raising credits the +3 immediately
	if n := count(); n != 3 {
		t.Fatalf("after raising 2->5 allowed %d, want 3", n)
	}
	g.SetPerMinute(100)
	g.Allow("k")      // +95 credited, one used
	g.SetPerMinute(4) // lowering caps the stored tokens at the new burst
	if n := count(); n != 4 {
		t.Fatalf("after lowering to 4 allowed %d", n)
	}
	g.SetPerMinute(0) // ignored
	if g.perMinute.Load() != 4 {
		t.Fatal("invalid rate applied")
	}
}

// A raised budget, as on a hot reload, is honoured by the other instance too.
func TestGlobalSetPerMinuteAcrossInstances(t *testing.T) {
	addr := valkeytest.Require(t)
	p := valkeytest.Prefix(t)
	a := NewGlobal(globalOpts(t, addr, p), "r", 1, FailClosed)
	b := NewGlobal(globalOpts(t, addr, p), "r", 1, FailClosed)
	a.Allow("k")
	if ok, _ := b.Allow("k"); ok {
		t.Fatal("second instance exceeded the shared budget")
	}
	a.SetPerMinute(3)
	b.SetPerMinute(3)
	if ok, _ := b.Allow("k"); !ok {
		t.Fatal("raised budget not applied")
	}
}

// With the backend unreachable each policy behaves as documented, quickly, and
// the failures are counted.
func TestGlobalFailurePolicies(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	o := globalOpts(t, addr, "p:")
	o.Timeout = 50 * time.Millisecond
	open := NewGlobal(o, "open", 1, FailOpen)
	closed := NewGlobal(o, "closed", 1, FailClosed)
	local := NewGlobal(o, "local", 3, FailLocal)
	start := time.Now()
	for range 5 {
		if ok, _ := open.Allow("k"); !ok {
			t.Fatal("fail-open denied")
		}
		if ok, retry := closed.Allow("k"); ok || retry != FailClosedRetry {
			t.Fatalf("fail-closed: %v %v", ok, retry)
		}
	}
	n := 0
	for range 5 {
		if ok, _ := local.Allow("k"); ok {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("fail-local allowed %d, want the local budget 3", n)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("15 checks against a dead backend took %v", d)
	}
	for name, l := range map[string]*Global{"open": open, "closed": closed, "local": local} {
		if l.errs.Load() != 5 {
			t.Errorf("%s: errors counted %d", name, l.errs.Load())
		}
	}
	if o.Metrics.Counter("ratelimit_global_errors_total", `limiter="closed",on_failure="closed"`).Load() != 5 {
		t.Fatal("metric series missing")
	}
}

// A server that accepts but never answers costs at most the limiter timeout.
func TestGlobalBlackholeTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	o := globalOpts(t, ln.Addr().String(), "p:")
	o.Timeout = 30 * time.Millisecond
	g := NewGlobal(o, "bh", 10, FailOpen)
	start := time.Now()
	if ok, _ := g.Allow("k"); !ok {
		t.Fatal("fail-open denied")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("blocked %v", d)
	}
}

// Restarting the (non-persistent) server: checks fail per policy while it is
// down, then recover without intervention (the script is reloaded via EVAL on
// NOSCRIPT). Budgets restart from full because state was in memory only.
// Needs GOSTACK_TEST_VALKEY_SERVER=/path/to/valkey-server.
func TestGlobalServerRestart(t *testing.T) {
	bin := os.Getenv("GOSTACK_TEST_VALKEY_SERVER")
	if bin == "" {
		t.Skip("GOSTACK_TEST_VALKEY_SERVER not set")
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	dir := t.TempDir()
	start := func() *exec.Cmd {
		cmd := exec.Command(bin, "--port", fmt.Sprint(port), "--bind", "127.0.0.1", "--save", "", "--appendonly", "no", "--dir", dir,
			"--logfile", filepath.Join(dir, "log"))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		c := valkey.New(valkey.Options{Addr: fmt.Sprintf("127.0.0.1:%d", port)})
		defer c.Close()
		for i := 0; c.Ping(context.Background()) != nil; i++ {
			if i > 100 {
				t.Fatal("valkey did not start")
			}
			time.Sleep(20 * time.Millisecond)
		}
		return cmd
	}
	cmd := start()
	o := globalOpts(t, fmt.Sprintf("127.0.0.1:%d", port), "r:")
	o.Timeout = 100 * time.Millisecond
	g := NewGlobal(o, "x", 2, FailClosed)
	g.Allow("k")
	g.Allow("k")
	if ok, _ := g.Allow("k"); ok {
		t.Fatal("budget exceeded")
	}
	cmd.Process.Kill()
	cmd.Wait()
	if ok, retry := g.Allow("k"); ok || retry != FailClosedRetry {
		t.Fatalf("down: %v %v", ok, retry)
	}
	if g.errs.Load() == 0 {
		t.Fatal("outage not counted")
	}
	cmd = start()
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	if ok, _ := g.Allow("k"); !ok {
		t.Fatal("did not recover after restart")
	}
}
