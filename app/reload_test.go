package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pillar/gostack/core/config"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	c := config.Defaults()
	dir := t.TempDir()
	c.Database.Path = filepath.Join(dir, "app.db")
	c.Database.AnalyticsPath = filepath.Join(dir, "analytics.db")
	c.Kits = []string{"game"}
	return c
}

func newTestApp(t *testing.T, c config.Config) *App {
	t.Helper()
	log, lv := NewLogger(io.Discard, c.Log.Level, "json")
	a, err := New(context.Background(), c, log)
	if err != nil {
		t.Fatal(err)
	}
	a.LogLevel = lv
	t.Cleanup(func() { a.Close() })
	return a
}

func TestReloadAppliesHotKeysOnly(t *testing.T) {
	c := testConfig(t)
	a := newTestApp(t, c)
	var got []config.Config
	a.OnReload(func(_, n config.Config) { got = append(got, n) })

	next := c
	next.Log.Level = "debug"
	next.HTTP.MaxConcurrent = 1
	next.Game.AuthRatePerMinute = 7
	next.HTTP.Listen = "127.0.0.1:9999"      // restart-only
	next.Database.ReadConnections = 5        // restart-only
	next.Queue.Workers = c.Queue.Workers + 1 // restart-only
	res, err := a.Reload(next)
	if err != nil {
		t.Fatal(err)
	}
	if keys := changeKeys(res.Applied); strings.Join(keys, ",") != "game.auth_rate_per_minute,http.max_concurrent,log.level" {
		t.Fatalf("applied %v", keys)
	}
	if keys := changeKeys(res.RestartRequired); strings.Join(keys, ",") != "database.read_connections,http.listen,queue.workers" {
		t.Fatalf("restart required %v", keys)
	}
	cur := a.Config()
	if cur.Log.Level != "debug" || cur.HTTP.MaxConcurrent != 1 || cur.Game.AuthRatePerMinute != 7 {
		t.Fatalf("hot keys not applied: %+v", cur)
	}
	if cur.HTTP.Listen != c.HTTP.Listen || cur.Database.ReadConnections != c.Database.ReadConnections || cur.Queue.Workers != c.Queue.Workers {
		t.Fatal("restart-only keys applied to running snapshot")
	}
	if a.LogLevel.Level() != slog.LevelDebug || a.inflightMax.Load() != 1 || a.restartKeys.Load() != 3 {
		t.Fatalf("runtime not retuned: level=%v max=%d pending=%d", a.LogLevel.Level(), a.inflightMax.Load(), a.restartKeys.Load())
	}
	if len(got) != 1 || got[0].Game.AuthRatePerMinute != 7 {
		t.Fatalf("subscriber not notified: %+v", got)
	}

	// Invalid configuration: rejected as a whole, snapshot untouched, no notification.
	bad := next
	bad.Log.Level = "loud"
	bad.HTTP.MaxConcurrent = 50
	if _, err := a.Reload(bad); err == nil {
		t.Fatal("invalid config accepted")
	}
	if a.Config().HTTP.MaxConcurrent != 1 || a.inflightMax.Load() != 1 || len(got) != 1 {
		t.Fatal("invalid reload changed state")
	}
	if a.Metrics.Counter("config_reloads_total", `result="rejected"`).Load() != 1 {
		t.Fatal("rejection not counted")
	}
}

func TestReloadRetunesConcurrencyGate(t *testing.T) {
	c := testConfig(t)
	c.HTTP.MaxConcurrent = 1
	a := newTestApp(t, c)
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	srv := httptest.NewServer(a.Handler(Hooks{Routes: func(mux *http.ServeMux, _ *App) {
		mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
			entered <- struct{}{}
			<-release
		})
	}}))
	defer srv.Close()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); res, _ := http.Get(srv.URL + "/slow"); res.Body.Close() }()
	<-entered
	res, _ := http.Get(srv.URL + "/healthz")
	res.Body.Close()
	if res.StatusCode != 503 {
		t.Fatalf("second request with max_concurrent=1: %d", res.StatusCode)
	}
	next := a.Config()
	next.HTTP.MaxConcurrent = 2
	if _, err := a.Reload(next); err != nil {
		t.Fatal(err)
	}
	res, _ = http.Get(srv.URL + "/healthz")
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("after raising max_concurrent: %d", res.StatusCode)
	}
	close(release)
	wg.Wait()
}

// Web auth is mounted when enabled and its limiters follow hot reloads.
func TestWebAuthWiredAndReloadable(t *testing.T) {
	c := testConfig(t)
	c.Auth.Web.Enabled = true
	c.Auth.Web.IPRatePerMinute = 1
	a := newTestApp(t, c)
	srv := httptest.NewServer(a.Handler(Hooks{}))
	defer srv.Close()
	login := func() int {
		res, err := http.Post(srv.URL+"/v1/auth/login", "application/json", strings.NewReader(`{"email":"a@example.com","password":"password1"}`))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if s := login(); s != 401 {
		t.Fatalf("first login %d", s)
	}
	if s := login(); s != 429 {
		t.Fatalf("second login with 1/min %d", s)
	}
	next := a.Config()
	next.Auth.Web.IPRatePerMinute = 100
	if _, err := a.Reload(next); err != nil {
		t.Fatal(err)
	}
	if s := login(); s != 401 {
		t.Fatalf("after reload %d", s)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func waitFor(t *testing.T, buf *syncBuf, substr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), substr) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in log:\n%s", substr, buf.String())
}

// TestSIGHUPReload drives the real serve path: SIGHUP re-reads the config file
// plus the original --set flags, applies hot keys and logs restart-only ones;
// an invalid file is rejected and the process keeps running.
func TestSIGHUPReload(t *testing.T) {
	if testing.Short() {
		t.Skip("sends signals to the test process")
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "gostack.yaml")
	write := func(s string) {
		if err := os.WriteFile(cfgPath, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("log:\n  level: info\n")
	stderr := &syncBuf{}
	done := make(chan int, 1)
	args := []string{"--config", cfgPath, "--set", "http.listen=127.0.0.1:0", "--set", "database.path=" + filepath.Join(dir, "app.db"),
		"--set", "database.analytics_path=" + filepath.Join(dir, "an.db"), "serve"}
	go func() {
		done <- Main(args, Hooks{}, Env{Stdout: io.Discard, Stderr: stderr, Getenv: func(string) (string, bool) { return "", false }})
	}()
	waitFor(t, stderr, `"msg":"serving"`)

	write("log:\n  level: debug\nhttp:\n  max_concurrent: 3\n  request_timeout: 3s\n")
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitFor(t, stderr, `"msg":"config reloaded"`)
	waitFor(t, stderr, `"key":"http.request_timeout"`)
	log := stderr.String()
	if !strings.Contains(log, `"applied":["http.max_concurrent","log.level"]`) {
		t.Fatalf("applied keys not logged:\n%s", log)
	}
	if strings.Contains(log, `"key":"http.listen"`) {
		t.Fatal("--set override lost on reload (http.listen reported as changed)")
	}

	write("log:\n  level: loud\n")
	syscall.Kill(os.Getpid(), syscall.SIGHUP)
	waitFor(t, stderr, "config reload rejected")

	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve exit %d\n%s", code, stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not stop")
	}
}
