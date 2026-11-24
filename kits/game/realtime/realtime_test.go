package realtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/pillar/gostack/app"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game/gametest"
	"github.com/pillar/gostack/kits/game/realtime"
)

type env struct {
	*gametest.Env
	m *realtime.Module
}

func setup(t *testing.T, o realtime.Options, h realtime.Handler) *env {
	t.Helper()
	e := &env{}
	e.Env = gametest.New(t, gametest.Options{Routes: func(mux *http.ServeMux, a *app.App) {
		e.m = &realtime.Module{Kit: a.Game, DB: a.DB, Metrics: a.Metrics, Options: o, OnMessage: h}
		e.m.Routes(mux)
	}})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = e.m.Hub().Close(ctx)
	})
	return e
}

func (e *env) wsURL(gameID string) string {
	return "ws" + strings.TrimPrefix(e.URL, "http") + "/v1/games/" + gameID + "/realtime"
}

func (e *env) dial(gameID, token string, hdr ...string) (*websocket.Conn, *http.Response, error) {
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		h.Set(hdr[i], hdr[i+1])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, res, err := websocket.Dial(ctx, e.wsURL(gameID), &websocket.DialOptions{HTTPHeader: h})
	if c != nil {
		c.SetReadLimit(1 << 20)
		e.T.Cleanup(func() { _ = c.CloseNow() })
	}
	return c, res, err
}

// connect dials with header auth and returns the connection and its welcome.
func (e *env) connect(gameID, token string) (*websocket.Conn, map[string]any) {
	e.T.Helper()
	c, _, err := e.dial(gameID, token)
	if err != nil {
		e.T.Fatal(err)
	}
	w := read(e.T, c)
	if w["type"] != "welcome" {
		e.T.Fatalf("want welcome, got %v", w)
	}
	return c, w
}

func read(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	m, err := tryRead(c, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func tryRead(c *websocket.Conn, d time.Duration) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(b, &m)
}

func write(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

// closeStatus reads until the connection ends and returns the close code.
func closeStatus(t *testing.T, c *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

func eventually(t *testing.T, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *env) counter(name, reason string) int64 {
	return e.App.Metrics.Counter(name, `reason="`+reason+`"`).Load()
}

func num(v any) int64 { f, _ := v.(float64); return int64(f) }

func TestAuthHeaderAndFirstMessage(t *testing.T) {
	e := setup(t, realtime.Options{}, nil)
	p := e.Guest("alpha")
	_, w := e.connect("alpha", p.Token)
	if w["player_id"] != p.ID || num(w["seq"]) != 0 || w["epoch"] == "" {
		t.Fatalf("welcome %v", w)
	}

	c, _, err := e.dial("alpha", "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, c, map[string]any{"type": "auth", "token": p.Token})
	if w := read(t, c); w["type"] != "welcome" || w["player_id"] != p.ID {
		t.Fatalf("welcome %v", w)
	}
	write(t, c, map[string]any{"type": "ping"})
	if m := read(t, c); m["type"] != "pong" {
		t.Fatalf("%v", m)
	}
	write(t, c, map[string]any{"type": "auth", "token": p.Token})
	if m := read(t, c); m["code"] != "already_authenticated" {
		t.Fatalf("%v", m)
	}
}

func TestAuthFailures(t *testing.T) {
	e := setup(t, realtime.Options{AuthTimeout: 200 * time.Millisecond}, nil)
	p := e.Guest("alpha")

	// Tokens in the URL are refused even when otherwise valid.
	for _, q := range []string{"?access_token=", "?token="} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, res, err := websocket.Dial(ctx, e.wsURL("alpha")+q+p.Token, nil)
		cancel()
		if err == nil || res == nil || res.StatusCode != 400 {
			t.Fatalf("url token accepted: %v %v", err, res)
		}
	}
	// Header auth: wrong game → 403, bad token → 401, before the upgrade.
	if _, res, err := e.dial("beta", p.Token); err == nil || res.StatusCode != 403 {
		t.Fatalf("cross-game token: %v", err)
	}
	if _, res, err := e.dial("alpha", "nope"); err == nil || res.StatusCode != 401 {
		t.Fatalf("bad token: %v", err)
	}
	if _, res, err := e.dial("nogame", p.Token); err == nil || res.StatusCode != 404 {
		t.Fatalf("unknown game: %v", err)
	}
	// First-message auth failures close with 4003 / 4001.
	c, _, err := e.dial("beta", "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, c, map[string]any{"type": "auth", "token": p.Token})
	if s := closeStatus(t, c); s != realtime.CodeForbidden {
		t.Fatalf("status %v", s)
	}
	c, _, _ = e.dial("alpha", "")
	write(t, c, map[string]any{"type": "auth", "token": "bogus"})
	if s := closeStatus(t, c); s != realtime.CodeUnauthorized {
		t.Fatalf("status %v", s)
	}
	c, _, _ = e.dial("alpha", "")
	write(t, c, map[string]any{"type": "ping"})
	if s := closeStatus(t, c); s != realtime.CodeUnauthorized {
		t.Fatalf("status %v", s)
	}
	// No auth message within AuthTimeout.
	c, _, _ = e.dial("alpha", "")
	if s := closeStatus(t, c); s != realtime.CodeUnauthorized {
		t.Fatalf("status %v", s)
	}
	eventually(t, 5*time.Second, func() bool { return e.m.Hub().Stats().Connections == 0 })
	if e.counter("realtime_closed_total", "auth_timeout") != 1 {
		t.Fatal("auth timeout not counted")
	}
}

func TestOriginCheck(t *testing.T) {
	e := setup(t, realtime.Options{}, nil)
	p := e.Guest("alpha")
	if _, res, err := e.dial("alpha", p.Token, "Origin", "https://evil.example"); err == nil || res.StatusCode != 403 {
		t.Fatalf("foreign origin: %v", err)
	}
	if _, res, err := e.dial("alpha", p.Token, "Origin", "https://beta.example"); err == nil || res.StatusCode != 403 {
		t.Fatalf("other game's origin: %v", err)
	}
	if _, _, err := e.dial("alpha", p.Token, "Origin", "https://alpha.example"); err != nil {
		t.Fatal(err)
	}
}

func TestSeqOrderingAndIsolation(t *testing.T) {
	e := setup(t, realtime.Options{}, nil)
	hub := e.m.Hub()
	a, b := e.Guest("alpha"), e.Guest("beta")
	ca, _ := e.connect("alpha", a.Token)
	cb, _ := e.connect("beta", b.Token)

	for i := 1; i <= 20; i++ {
		if err := hub.Send("alpha", a.ID, "tick", map[string]int{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 20; i++ {
		m := read(t, ca)
		if m["type"] != "tick" || num(m["seq"]) != int64(i) || num(m["data"].(map[string]any)["n"]) != int64(i) {
			t.Fatalf("message %d: %v", i, m)
		}
	}
	// Same player id under another game, and broadcasts, stay within the game.
	if err := hub.Send("beta", a.ID, "leak", nil); err != nil {
		t.Fatal(err)
	}
	if n, err := hub.Broadcast("beta", "news", "b-only"); err != nil || n != 1 {
		t.Fatalf("broadcast n=%d err=%v", n, err)
	}
	if n, _ := hub.Broadcast("alpha", "news", "a-only"); n != 1 {
		t.Fatalf("broadcast n=%d", n)
	}
	if m := read(t, ca); m["data"] != "a-only" || num(m["seq"]) != 21 {
		t.Fatalf("%v", m)
	}
	if m := read(t, cb); m["data"] != "b-only" || num(m["seq"]) != 1 {
		t.Fatalf("%v", m)
	}
	if err := hub.Send("alpha", a.ID, "welcome", nil); !errors.Is(err, realtime.ErrInvalidType) {
		t.Fatalf("reserved type: %v", err)
	}
	if err := hub.Send("alpha", a.ID, "big", strings.Repeat("x", 5000)); !errors.Is(err, realtime.ErrTooLarge) {
		t.Fatalf("payload limit: %v", err)
	}
}

func TestAckAndResume(t *testing.T) {
	e := setup(t, realtime.Options{}, nil)
	hub := e.m.Hub()
	p := e.Guest("alpha")
	c, w := e.connect("alpha", p.Token)
	epoch := w["epoch"]
	for i := 1; i <= 5; i++ {
		_ = hub.Send("alpha", p.ID, "tick", i)
	}
	read(t, c)
	read(t, c)
	write(t, c, map[string]any{"type": "ack", "seq": 2})
	write(t, c, map[string]any{"type": "ack", "seq": 99})
	if m := read(t, c); m["type"] == "tick" {
		// remaining ticks may arrive before the ack error
		for m["type"] == "tick" {
			m = read(t, c)
		}
		if m["code"] != "invalid_ack" {
			t.Fatalf("%v", m)
		}
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	eventually(t, 5*time.Second, func() bool { return hub.Stats().Connections == 0 })
	_ = hub.Send("alpha", p.ID, "tick", 6) // offline: buffered for resume

	c, w = e.connect("alpha", p.Token)
	if w["epoch"] != epoch || num(w["seq"]) != 6 {
		t.Fatalf("welcome %v", w)
	}
	write(t, c, map[string]any{"type": "resume", "epoch": epoch, "last_seq": 2})
	m := read(t, c)
	if m["type"] != "resumed" || num(m["last_seq"]) != 2 || num(m["seq"]) != 6 {
		t.Fatalf("%v", m)
	}
	for i := 3; i <= 6; i++ {
		if m := read(t, c); num(m["seq"]) != int64(i) || num(m["data"]) != int64(i) {
			t.Fatalf("replay %d: %v", i, m)
		}
	}
	// Acked entries are gone; an unknown epoch cannot resume either.
	write(t, c, map[string]any{"type": "resume", "epoch": epoch, "last_seq": 1})
	if m := read(t, c); m["type"] != "resume_failed" || num(m["seq"]) != 6 {
		t.Fatalf("%v", m)
	}
	write(t, c, map[string]any{"type": "resume", "epoch": "other", "last_seq": 6})
	if m := read(t, c); m["type"] != "resume_failed" {
		t.Fatalf("%v", m)
	}
	// Live delivery continues after a failed resume.
	_ = hub.Send("alpha", p.ID, "tick", 7)
	if m := read(t, c); num(m["seq"]) != 7 {
		t.Fatalf("%v", m)
	}
	if e.App.Metrics.Counter("realtime_resume_total", `reason="ok"`).Load() != 1 {
		t.Fatal("resume metric")
	}
}

func TestResumeFailedAfterBufferOverflow(t *testing.T) {
	e := setup(t, realtime.Options{ReplayBuffer: 4}, nil)
	hub := e.m.Hub()
	p := e.Guest("alpha")
	c, w := e.connect("alpha", p.Token)
	_ = c.Close(websocket.StatusNormalClosure, "")
	eventually(t, 5*time.Second, func() bool { return hub.Stats().Connections == 0 })
	for i := 1; i <= 10; i++ {
		_ = hub.Send("alpha", p.ID, "tick", i)
	}
	c, _ = e.connect("alpha", p.Token)
	write(t, c, map[string]any{"type": "resume", "epoch": w["epoch"], "last_seq": 0})
	if m := read(t, c); m["type"] != "resume_failed" || num(m["seq"]) != 10 {
		t.Fatalf("%v", m)
	}
	// The last 4 are still replayable.
	write(t, c, map[string]any{"type": "resume", "epoch": w["epoch"], "last_seq": 6})
	if m := read(t, c); m["type"] != "resumed" {
		t.Fatalf("%v", m)
	}
	for i := 7; i <= 10; i++ {
		if m := read(t, c); num(m["seq"]) != int64(i) {
			t.Fatalf("%v", m)
		}
	}
}

func TestSlowConsumerDisconnected(t *testing.T) {
	e := setup(t, realtime.Options{SendQueue: 8, ReplayBuffer: 16, MaxPayloadBytes: 64 << 10, ReplayMaxBytes: 2 << 20}, nil)
	hub := e.m.Hub()
	p := e.Guest("alpha")
	c, _ := e.connect("alpha", p.Token) // then never reads
	payload := strings.Repeat("x", 60<<10)
	deadline := time.Now().Add(20 * time.Second)
	for e.counter("realtime_closed_total", "slow_consumer") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("slow consumer not detected")
		}
		_ = hub.Send("alpha", p.ID, "blob", payload)
	}
	eventually(t, 15*time.Second, func() bool { return hub.Stats().Connections == 0 })
	// The client drains what was buffered, then the connection ends. The 4008
	// close frame arrives only if it fit into the socket before the close timeout.
	s := closeStatus(t, c)
	t.Logf("client saw close status %v", s)
	if s != realtime.CodeSlowConsumer && s != -1 {
		t.Fatalf("unexpected status %v", s)
	}
}

func TestConnectionLimits(t *testing.T) {
	e := setup(t, realtime.Options{MaxConnections: 2, MaxPerPlayer: 1}, nil)
	p, q := e.Guest("alpha"), e.Guest("alpha")
	c1, _ := e.connect("alpha", p.Token)
	// Same player again: the older connection is replaced (4009).
	c2, _ := e.connect("alpha", p.Token)
	if s := closeStatus(t, c1); s != realtime.CodeReplaced {
		t.Fatalf("status %v", s)
	}
	eventually(t, 5*time.Second, func() bool { return e.m.Hub().Stats().Connections == 1 })
	e.connect("alpha", q.Token)
	if _, res, err := e.dial("alpha", q.Token); err == nil || res.StatusCode != 503 {
		t.Fatalf("over limit: %v", err)
	}
	if _, res, err := e.dial("alpha", ""); err == nil || res.StatusCode != 503 {
		t.Fatalf("unauthenticated over limit: %v", err)
	}
	if e.counter("realtime_rejected_total", "max_connections") != 2 {
		t.Fatal("rejections not counted")
	}
	_ = c2
}

func TestMessageSizeAndRateLimits(t *testing.T) {
	e := setup(t, realtime.Options{MaxMessageBytes: 64, InboundRate: 1, InboundBurst: 3}, nil)
	p := e.Guest("alpha")
	c, _ := e.connect("alpha", p.Token)
	write(t, c, map[string]any{"type": "chat", "data": strings.Repeat("x", 200)})
	if s := closeStatus(t, c); s != websocket.StatusMessageTooBig {
		t.Fatalf("status %v", s)
	}
	c, _ = e.connect("alpha", p.Token)
	for i := 0; i < 10; i++ {
		b, _ := json.Marshal(map[string]any{"type": "ping"})
		if c.Write(context.Background(), websocket.MessageText, b) != nil {
			break
		}
	}
	if s := closeStatus(t, c); s != realtime.CodeRateLimited {
		t.Fatalf("status %v", s)
	}
}

func TestPingTimeoutClosesDeadConnection(t *testing.T) {
	e := setup(t, realtime.Options{PingInterval: 100 * time.Millisecond, PongTimeout: 150 * time.Millisecond}, nil)
	hub := e.m.Hub()
	p := e.Guest("alpha")
	// A reading client answers pings and stays connected.
	live, _ := e.connect("alpha", p.Token)
	go func() {
		for {
			if _, _, err := live.Read(context.Background()); err != nil {
				return
			}
		}
	}()
	// A client that never reads never answers pings (dead peer).
	q := e.Guest("alpha")
	e.connect("alpha", q.Token)
	eventually(t, 5*time.Second, func() bool { return e.counter("realtime_closed_total", "ping_timeout") == 1 })
	eventually(t, 5*time.Second, func() bool { return hub.Stats().Connections == 1 })
	time.Sleep(500 * time.Millisecond)
	if hub.Stats().Connections != 1 {
		t.Fatal("live connection dropped")
	}
}

func TestSessionRevokedWhileConnected(t *testing.T) {
	e := setup(t, realtime.Options{PingInterval: 50 * time.Millisecond, SessionRecheck: 50 * time.Millisecond}, nil)
	p := e.Guest("alpha")
	c, _ := e.connect("alpha", p.Token)
	e.Expect(e.Do("POST", "/v1/games/alpha/auth/logout", p.Token, nil), 204)
	if s := closeStatus(t, c); s != realtime.CodeUnauthorized {
		t.Fatalf("status %v", s)
	}
}

func TestApplicationMessages(t *testing.T) {
	var hub *realtime.Hub
	h := func(ctx context.Context, gameID, playerID string, m realtime.Message) error {
		switch m.Type {
		case "echo":
			var v any
			_ = json.Unmarshal(m.Data, &v)
			return hub.Send(gameID, playerID, "echoed", v)
		case "fail":
			return httpx.Conflict("not_your_turn", "wait")
		}
		return errors.New("boom")
	}
	e := setup(t, realtime.Options{}, h)
	hub = e.m.Hub()
	p := e.Guest("alpha")
	c, _ := e.connect("alpha", p.Token)
	write(t, c, map[string]any{"type": "echo", "id": "r1", "data": map[string]int{"x": 1}})
	got := map[string]map[string]any{}
	for range 2 {
		m := read(t, c)
		got[m["type"].(string)] = m
	}
	if got["ok"]["ref"] != "r1" || num(got["echoed"]["seq"]) != 1 {
		t.Fatalf("%v", got)
	}
	write(t, c, map[string]any{"type": "fail", "id": "r2"})
	if m := read(t, c); m["type"] != "error" || m["code"] != "not_your_turn" || m["ref"] != "r2" {
		t.Fatalf("%v", m)
	}
	write(t, c, map[string]any{"type": "other"})
	if m := read(t, c); m["code"] != "internal" {
		t.Fatalf("%v", m)
	}
	write(t, c, map[string]any{"type": "BAD TYPE"})
	if m := read(t, c); m["code"] != "unknown_type" {
		t.Fatalf("%v", m)
	}
	_ = c.Write(context.Background(), websocket.MessageText, []byte("not json"))
	if m := read(t, c); m["code"] != "invalid_message" {
		t.Fatalf("%v", m)
	}
}

func TestDefaultRejectsApplicationMessages(t *testing.T) {
	e := setup(t, realtime.Options{}, nil)
	p := e.Guest("alpha")
	c, _ := e.connect("alpha", p.Token)
	write(t, c, map[string]any{"type": "chat", "data": "hi"})
	if m := read(t, c); m["type"] != "error" || m["code"] != "unknown_type" {
		t.Fatalf("%v", m)
	}
}

func TestGracefulShutdown(t *testing.T) {
	e := setup(t, realtime.Options{}, nil)
	hub := e.m.Hub()
	p, q := e.Guest("alpha"), e.Guest("beta")
	c1, _ := e.connect("alpha", p.Token)
	c2, _ := e.connect("beta", q.Token)
	done := make(chan websocket.StatusCode, 2)
	for _, c := range []*websocket.Conn{c1, c2} {
		go func() { done <- closeStatus(t, c) }()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hub.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if s := <-done; s != websocket.StatusGoingAway {
			t.Fatalf("status %v", s)
		}
	}
	if hub.Stats().Connections != 0 {
		t.Fatal("connections left")
	}
	if _, res, err := e.dial("alpha", p.Token); err == nil || res.StatusCode != 503 {
		t.Fatalf("dial after close: %v", err)
	}
	if err := hub.Send("alpha", p.ID, "tick", 1); !errors.Is(err, realtime.ErrClosed) {
		t.Fatalf("send after close: %v", err)
	}
}

// TestIdleConnectionsFootprint opens many idle connections and reports the
// goroutine and heap deltas. Client and server run in the same process, so the
// numbers include both sides; they are local measurements, not the PRD 1C1G
// acceptance test (3000 connections for 30 minutes).
func TestIdleConnectionsFootprint(t *testing.T) {
	if testing.Short() {
		t.Skip("load test")
	}
	const n = 1000
	e := setup(t, realtime.Options{MaxConnections: n + 10}, nil)
	p := e.Guest("alpha")
	tokens := make([]string, 0, n/2)
	for range n / 2 {
		tokens = append(tokens, e.Guest("alpha").Token)
	}
	_ = p
	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	g0, h0 := runtime.NumGoroutine(), ms.HeapInuse
	start := time.Now()
	conns := make([]*websocket.Conn, 0, n)
	for i := range n {
		c, _ := e.connect("alpha", tokens[i/2]) // two connections per player
		conns = append(conns, c)
	}
	elapsed := time.Since(start)
	eventually(t, 10*time.Second, func() bool { return e.m.Hub().Stats().Connections == n })
	runtime.GC()
	runtime.ReadMemStats(&ms)
	g1, h1 := runtime.NumGoroutine(), ms.HeapInuse
	t.Logf("%d idle connections (client+server in one process, %s/%s): open %v, goroutines +%d (%.2f/conn), heap in use +%.1f MiB (%.1f KiB/conn)",
		n, runtime.GOOS, runtime.GOARCH, elapsed.Round(time.Millisecond), g1-g0, float64(g1-g0)/n,
		float64(h1-h0)/(1<<20), float64(h1-h0)/1024/n)
	n2, err := e.m.Hub().Broadcast("alpha", "news", "hello")
	if err != nil || n2 != n/2 {
		t.Fatalf("broadcast reached %d players: %v", n2, err)
	}
	for _, c := range conns[:10] {
		if m := read(t, c); m["data"] != "hello" {
			t.Fatalf("%v", m)
		}
	}
}
