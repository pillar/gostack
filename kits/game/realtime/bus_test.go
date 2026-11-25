package realtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/pillar/gostack/adapters/pgtest"
	"github.com/pillar/gostack/app"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/kits/game/gametest"
	"github.com/pillar/gostack/kits/game/realtime"
)

// node is one application instance with its realtime module on the bus.
type node struct {
	*env
	proxy *proxy // LISTEN session goes through it (nil: direct)
}

func (n *node) hub() *realtime.Hub { return n.m.Hub() }

// busPair starts two instances on one PostgreSQL database, both with the
// PostgreSQL bus. With proxied, each LISTEN session runs through a TCP proxy
// the test can cut.
func busPair(t *testing.T, o realtime.Options, proxied bool) (a, b *node) {
	t.Helper()
	pgtest.RequirePostgres(t)
	var mu sync.Mutex
	var nodes []*node
	routes := func(mux *http.ServeMux, ap *app.App) {
		dsn, err := config.ResolveSecret(ap.Cfg.Database.DSNRef)
		if err != nil {
			t.Fatal(err)
		}
		n := &node{env: &env{}}
		if proxied {
			n.proxy = newProxy(t, dsn)
			dsn = n.proxy.dsn
		}
		n.m = &realtime.Module{Kit: ap.Game, DB: ap.DB, Metrics: ap.Metrics, Options: o,
			Bus: &realtime.PostgresBus{DSN: dsn, ReconnectMin: 20 * time.Millisecond, ReconnectMax: 200 * time.Millisecond}}
		n.m.Routes(mux)
		mu.Lock()
		nodes = append(nodes, n)
		mu.Unlock()
	}
	first := gametest.New(t, gametest.Options{Routes: routes})
	second := first.Instance(t)
	nodes[0].Env, nodes[1].Env = first, second
	for _, n := range nodes {
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = n.m.Close(ctx)
		})
		eventually(t, 5*time.Second, func() bool { return n.hub().Stats().BusUp })
	}
	return nodes[0], nodes[1]
}

// sock reads a connection in the background, so waiting for "no message"
// does not cancel a Read (which would close the connection).
type sock struct {
	*websocket.Conn
	in   chan map[string]any
	done chan struct{}
	err  error
}

func open(e *env, game, token string) (*sock, map[string]any) {
	e.T.Helper()
	c, w := e.connect(game, token)
	s := &sock{Conn: c, in: make(chan map[string]any, 1024), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for {
			_, b, err := c.Read(context.Background())
			if err != nil {
				s.err = err
				return
			}
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			s.in <- m
		}
	}()
	return s, w
}

func (s *sock) next(t *testing.T) map[string]any {
	t.Helper()
	select {
	case m := <-s.in:
		return m
	case <-time.After(5 * time.Second):
		select {
		case <-s.done:
			t.Fatalf("no message in time; closed: %v", s.err)
		default:
		}
		t.Fatal("no message in time")
	}
	return nil
}

// status waits for the connection to end and returns its close code.
func (s *sock) status(t *testing.T) websocket.StatusCode {
	t.Helper()
	select {
	case <-s.done:
		return websocket.CloseStatus(s.err)
	case <-time.After(15 * time.Second):
		t.Fatal("connection not closed in time")
	}
	return 0
}

// expect reads business messages and checks their seqs and payload "i".
func expect(t *testing.T, c *sock, typ string, fromSeq int64, is ...int) {
	t.Helper()
	for k, i := range is {
		m := c.next(t)
		d, _ := m["data"].(map[string]any)
		if m["type"] != typ || num(m["seq"]) != fromSeq+int64(k) || num(d["i"]) != int64(i) {
			t.Fatalf("message %d: want %s seq %d i %d, got %v", k, typ, fromSeq+int64(k), i, m)
		}
	}
}

func quiet(t *testing.T, c *sock, d time.Duration) {
	t.Helper()
	select {
	case m := <-c.in:
		t.Fatalf("unexpected message %v", m)
	case <-time.After(d):
	}
}

func send(t *testing.T, h *realtime.Hub, game, player, typ string, i int) {
	t.Helper()
	if err := h.Send(game, player, typ, map[string]int{"i": i}); err != nil {
		t.Fatal(err)
	}
}

// A Send on instance B reaches the player's connection on instance A in order
// with contiguous seqs; concurrent senders on both instances never produce a
// duplicate or a gap.
func TestBusSendAcrossInstances(t *testing.T) {
	// The 80-message burst below exceeds the default SendQueue (32); backpressure
	// is covered by TestBusBackpressure.
	a, b := busPair(t, realtime.Options{SendQueue: 256, ReplayBuffer: 256}, false)
	p := a.Guest("alpha")
	c, w := open(a.env, "alpha", p.Token)
	if num(w["seq"]) != 0 || w["epoch"] == "" {
		t.Fatalf("welcome %v", w)
	}
	for i := 1; i <= 30; i++ {
		send(t, b.hub(), "alpha", p.ID, "tick", i)
	}
	expect(t, c, "tick", 1, seq(1, 30)...)

	// Interleaved senders on both instances: one total order, per-sender FIFO.
	var wg sync.WaitGroup
	for _, n := range []*node{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 1; i <= 40; i++ {
				_ = n.hub().Send("alpha", p.ID, "from_"+n.URL[len(n.URL)-5:], map[string]int{"i": i})
			}
		}()
	}
	wg.Wait()
	last := map[string]int64{}
	for s := int64(31); s <= 110; s++ {
		m := c.next(t)
		if num(m["seq"]) != s {
			t.Fatalf("want seq %d, got %v", s, m)
		}
		typ := m["type"].(string)
		i := num(m["data"].(map[string]any)["i"])
		if i != last[typ]+1 {
			t.Fatalf("sender %s out of order: %d after %d", typ, i, last[typ])
		}
		last[typ] = i
	}
	quiet(t, c, 200*time.Millisecond)

	// A second connection of the player on instance B shares the stream.
	c2, w2 := open(b.env, "alpha", p.Token)
	if w2["epoch"] != w["epoch"] || num(w2["seq"]) != 110 {
		t.Fatalf("welcome on B %v (A %v)", w2, w)
	}
	send(t, a.hub(), "alpha", p.ID, "tick", 111)
	expect(t, c, "tick", 111, 111)
	expect(t, c2, "tick", 111, 111)
}

func seq(from, to int) []int {
	var out []int
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

// A broadcast on one instance reaches connected players on both instances,
// once per player even when the player is connected to both.
func TestBusBroadcastBothInstances(t *testing.T) {
	a, b := busPair(t, realtime.Options{}, false)
	p1, p2, p3, pb := a.Guest("alpha"), a.Guest("alpha"), a.Guest("alpha"), a.Guest("beta")
	c1, _ := open(a.env, "alpha", p1.Token)
	c2, _ := open(b.env, "alpha", p2.Token)
	c3a, _ := open(a.env, "alpha", p3.Token)
	c3b, _ := open(b.env, "alpha", p3.Token)
	cb, _ := open(b.env, "beta", pb.Token)

	n, err := a.hub().Broadcast("alpha", "news", map[string]int{"i": 1})
	if err != nil || n != 2 { // p1 and p3 are connected to A
		t.Fatalf("broadcast n=%d err=%v", n, err)
	}
	if _, err := b.hub().Broadcast("alpha", "news", map[string]int{"i": 2}); err != nil {
		t.Fatal(err)
	}
	// Broadcasts issued on different instances have no mutual order: each
	// player gets both, with seqs 1 and 2, in some order.
	got := func(c *sock) []int64 {
		var is []int64
		for s := int64(1); s <= 2; s++ {
			m := c.next(t)
			if m["type"] != "news" || num(m["seq"]) != s {
				t.Fatalf("want news seq %d, got %v", s, m)
			}
			is = append(is, num(m["data"].(map[string]any)["i"]))
		}
		if is[0]+is[1] != 3 || is[0] == is[1] {
			t.Fatalf("broadcasts %v, want 1 and 2", is)
		}
		return is
	}
	got(c1)
	got(c2)
	// p3's two connections (one per instance) see the same stream: two
	// messages, same order, not four.
	if x, y := got(c3a), got(c3b); !slices.Equal(x, y) {
		t.Fatalf("p3 connections diverge: %v vs %v", x, y)
	}
	for _, c := range []*sock{c1, c2, c3a, c3b, cb} {
		quiet(t, c, 200*time.Millisecond)
	}
	var rows int64
	if err := a.App.DB.Read.QueryRow(`SELECT COUNT(*) FROM realtime_messages WHERE player_id=?`, p3.ID).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("p3 stored messages %d (%v), want 2", rows, err)
	}
	if _, err := a.hub().Broadcast("alpha", "news", strings.Repeat("x", 4000)); err != nil {
		t.Fatalf("broadcast within the NOTIFY budget: %v", err)
	}
}

// Resume on another instance replays from the shared window or fails; it never
// skips or duplicates a seq.
func TestBusResumeAcrossInstances(t *testing.T) {
	a, b := busPair(t, realtime.Options{ReplayBuffer: 8}, false)
	p := a.Guest("alpha")
	c, w := open(a.env, "alpha", p.Token)
	epoch := w["epoch"]
	for i := 1; i <= 3; i++ {
		send(t, b.hub(), "alpha", p.ID, "tick", i)
	}
	expect(t, c, "tick", 1, 1, 2, 3)
	_ = c.Close(websocket.StatusNormalClosure, "")
	send(t, b.hub(), "alpha", p.ID, "tick", 4) // offline
	send(t, b.hub(), "alpha", p.ID, "tick", 5)
	waitLast(t, a.Env, p.ID, 5)

	// Resume on B, which never had a connection of this player.
	cb, wb := open(b.env, "alpha", p.Token)
	if wb["epoch"] != epoch {
		t.Fatalf("epoch differs across instances: %v vs %v", wb, w)
	}
	write(t, cb.Conn, map[string]any{"type": "resume", "epoch": epoch, "last_seq": 3})
	if m := cb.next(t); m["type"] != "resumed" || num(m["seq"]) != 5 {
		t.Fatalf("%v", m)
	}
	expect(t, cb, "tick", 4, 4, 5)

	// Back on A (its buffer only had 1..3): full replay from the table.
	ca, _ := open(a.env, "alpha", p.Token)
	write(t, ca.Conn, map[string]any{"type": "resume", "epoch": epoch, "last_seq": 0})
	if m := ca.next(t); m["type"] != "resumed" || num(m["seq"]) != 5 {
		t.Fatalf("%v", m)
	}
	expect(t, ca, "tick", 1, 1, 2, 3, 4, 5)
	for _, bad := range []map[string]any{
		{"type": "resume", "epoch": "other", "last_seq": 5},
		{"type": "resume", "epoch": epoch, "last_seq": 99},
	} {
		write(t, ca.Conn, bad)
		if m := ca.next(t); m["type"] != "resume_failed" || num(m["seq"]) != 5 || m["epoch"] != epoch {
			t.Fatalf("%v: %v", bad, m)
		}
	}
	// Overflow the shared window (8): seqs 6..15, rows 8..15 remain.
	_ = ca.Close(websocket.StatusNormalClosure, "")
	_ = cb.Close(websocket.StatusNormalClosure, "")
	for i := 6; i <= 15; i++ {
		send(t, b.hub(), "alpha", p.ID, "tick", i)
	}
	waitLast(t, a.Env, p.ID, 15)
	ca, _ = open(a.env, "alpha", p.Token)
	write(t, ca.Conn, map[string]any{"type": "resume", "epoch": epoch, "last_seq": 5})
	if m := ca.next(t); m["type"] != "resume_failed" || num(m["seq"]) != 15 {
		t.Fatalf("resume past the window: %v", m)
	}
	write(t, ca.Conn, map[string]any{"type": "resume", "epoch": epoch, "last_seq": 7})
	if m := ca.next(t); m["type"] != "resumed" {
		t.Fatalf("%v", m)
	}
	expect(t, ca, "tick", 8, seq(8, 15)...)
	send(t, b.hub(), "alpha", p.ID, "tick", 16)
	expect(t, ca, "tick", 16, 16)
}

// Killing an instance's LISTEN session: messages published meanwhile by the
// other instance are delivered after the reconnect (caught up from the table).
// While an instance's own session is down it delivers locally only, under a
// fresh epoch (4012 first); after the reconnect it returns to the shared stream.
func TestBusOutageAndRecovery(t *testing.T) {
	a, b := busPair(t, realtime.Options{}, true)
	p1, p2 := a.Guest("alpha"), a.Guest("alpha")
	c1, w1 := open(a.env, "alpha", p1.Token)
	c2, w2 := open(b.env, "alpha", p2.Token)
	send(t, b.hub(), "alpha", p1.ID, "tick", 1)
	expect(t, c1, "tick", 1, 1)

	// 1. A's session is cut; B keeps publishing to p1 (on A).
	a.proxy.cut()
	eventually(t, 5*time.Second, func() bool { return !a.hub().Stats().BusUp })
	for i := 2; i <= 4; i++ {
		send(t, b.hub(), "alpha", p1.ID, "tick", i)
	}
	quiet(t, c1, 300*time.Millisecond)
	a.proxy.restore()
	eventually(t, 5*time.Second, func() bool { return a.hub().Stats().BusUp })
	expect(t, c1, "tick", 2, 2, 3, 4) // caught up in order
	if a.App.Metrics.Counter("realtime_bus_reconnects_total", "").Load() < 1 {
		t.Fatal("reconnect not counted")
	}

	// 2. B's session is cut: B degrades to local delivery.
	b.proxy.cut()
	eventually(t, 5*time.Second, func() bool { return !b.hub().Stats().BusUp })
	send(t, b.hub(), "alpha", p2.ID, "tick", 1) // p2 on B: stream switches to a local epoch
	if s := c2.status(t); s != realtime.CodeStreamReset {
		t.Fatalf("p2 close %v, want 4012", s)
	}
	c2, lw := open(b.env, "alpha", p2.Token)
	if lw["epoch"] == w2["epoch"] {
		t.Fatalf("degraded stream kept the shared epoch: %v", lw)
	}
	send(t, b.hub(), "alpha", p2.ID, "tick", 2)
	expect(t, c2, "tick", num(lw["seq"])+1, 2) // delivered locally
	send(t, b.hub(), "alpha", p1.ID, "tick", 99)
	quiet(t, c1, 300*time.Millisecond) // p1 is on A: not reachable while B is degraded
	if got := b.App.Metrics.Counter("realtime_bus_degraded_total", `reason="bus_down"`).Load(); got < 3 {
		t.Fatalf("degraded counter %d", got)
	}

	// 3. B reconnects: its local stream ends (4012); p2 rejoins the shared
	// stream and cannot resume the local epoch.
	b.proxy.restore()
	eventually(t, 5*time.Second, func() bool { return b.hub().Stats().BusUp })
	if s := c2.status(t); s != realtime.CodeStreamReset {
		t.Fatalf("p2 close after recovery %v, want 4012", s)
	}
	c2, rw := open(b.env, "alpha", p2.Token)
	if rw["epoch"] != w2["epoch"] {
		t.Fatalf("recovered epoch %v, want %v", rw["epoch"], w2["epoch"])
	}
	write(t, c2.Conn, map[string]any{"type": "resume", "epoch": lw["epoch"], "last_seq": num(lw["seq"]) + 1})
	if m := c2.next(t); m["type"] != "resume_failed" {
		t.Fatalf("%v", m)
	}
	send(t, a.hub(), "alpha", p2.ID, "tick", 3)
	expect(t, c2, "tick", num(rw["seq"])+1, 3)
	send(t, b.hub(), "alpha", p1.ID, "tick", 5)
	expect(t, c1, "tick", 5, 5) // p1's shared stream continued at 5 (99 was local to B)
	if w1["epoch"] == "" {
		t.Fatal("no epoch")
	}
}

// Bounds still apply under the bus: a connection that does not read is closed
// as a slow consumer (4008), and a full publish queue rejects Send.
func TestBusBackpressure(t *testing.T) {
	a, b := busPair(t, realtime.Options{SendQueue: 8, ReplayBuffer: 16, MaxPayloadBytes: 64 << 10, ReplayMaxBytes: 2 << 20, BusQueue: 4}, false)
	p := a.Guest("alpha")
	c, _ := a.connect("alpha", p.Token)    // then never reads
	payload := strings.Repeat("x", 60<<10) // larger than a NOTIFY: read from the table
	overloaded := 0
	deadline := time.Now().Add(30 * time.Second)
	for a.counter("realtime_closed_total", "slow_consumer") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("slow consumer not detected")
		}
		if err := b.hub().Send("alpha", p.ID, "blob", payload); errors.Is(err, realtime.ErrOverloaded) {
			overloaded++
			time.Sleep(time.Millisecond)
		}
	}
	eventually(t, 15*time.Second, func() bool { return a.hub().Stats().Connections == 0 })
	_ = c
	for overloaded == 0 && time.Now().Before(deadline) {
		if err := b.hub().Send("alpha", p.ID, "tick", 1); errors.Is(err, realtime.ErrOverloaded) {
			overloaded++
		}
	}
	if overloaded == 0 || b.counter("realtime_send_dropped_total", "bus_queue") == 0 {
		t.Fatalf("bus queue bound not enforced (overloaded=%d)", overloaded)
	}
}

// Wiring through configuration (game.realtime.bus: postgres) on two instances.
func TestBusWiredByConfig(t *testing.T) {
	pgtest.RequirePostgres(t)
	a := gametest.New(t, gametest.Options{Mutate: func(c *config.Config) {
		c.Game.Modules = []string{"realtime"}
		c.Game.Realtime.Bus = "postgres"
	}})
	b := a.Instance(t)
	for _, e := range []*gametest.Env{a, b} {
		eventually(t, 5*time.Second, func() bool { return e.App.Mod.Realtime.Hub().Stats().BusUp })
	}
	ea := &env{Env: a}
	p := a.Guest("alpha")
	c, _ := open(ea, "alpha", p.Token)
	send(t, b.App.Mod.Realtime.Hub(), "alpha", p.ID, "tick", 1)
	expect(t, c, "tick", 1, 1)
}

// Local cross-instance delivery latency (Send on B → frame read on A), logged
// for the record; it is not a performance gate.
func TestBusLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("latency sample")
	}
	a, b := busPair(t, realtime.Options{}, false)
	p := a.Guest("alpha")
	c, _ := open(a.env, "alpha", p.Token)
	measure := func(h *realtime.Hub) []time.Duration {
		var ds []time.Duration
		for i := 0; i < 300; i++ {
			t0 := time.Now()
			if err := h.Send("alpha", p.ID, "tick", i); err != nil {
				t.Fatal(err)
			}
			c.next(t)
			ds = append(ds, time.Since(t0))
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		return ds
	}
	pct := func(ds []time.Duration, q float64) time.Duration { return ds[min(len(ds)-1, int(q*float64(len(ds))))] }
	for _, r := range []struct {
		name string
		h    *realtime.Hub
	}{{"cross-instance (B→A)", b.hub()}, {"same instance via bus (A→A)", a.hub()}} {
		ds := measure(r.h)
		t.Logf("%s: n=%d p50=%v p95=%v p99=%v max=%v", r.name, len(ds), pct(ds, .5), pct(ds, .95), pct(ds, .99), slices.Max(ds))
	}
}

// proxy is a TCP proxy to PostgreSQL that the test can cut and restore.
type proxy struct {
	t      *testing.T
	ln     net.Listener
	target string
	dsn    string
	mu     sync.Mutex
	down   bool
	conns  []net.Conn
}

func newProxy(t *testing.T, dsn string) *proxy {
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{t: t, ln: ln, target: u.Host}
	u.Host = ln.Addr().String()
	p.dsn = u.String()
	t.Cleanup(func() { ln.Close(); p.cut() })
	go p.accept()
	return p
}

func (p *proxy) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		down := p.down
		p.mu.Unlock()
		if down {
			c.Close()
			continue
		}
		s, err := net.Dial("tcp", p.target)
		if err != nil {
			c.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, c, s)
		p.mu.Unlock()
		go func() { _, _ = io.Copy(s, c); s.Close() }()
		go func() { _, _ = io.Copy(c, s); c.Close() }()
	}
}

// cut closes the proxied connections and refuses new ones until restore.
func (p *proxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = true
	for _, c := range p.conns {
		c.Close()
	}
	p.conns = nil
}

func (p *proxy) restore() {
	p.mu.Lock()
	p.down = false
	p.mu.Unlock()
}

// waitLast waits until the player's shared stream reached seq n (Send is asynchronous under the bus).
func waitLast(t *testing.T, e *gametest.Env, playerID string, n int64) {
	t.Helper()
	eventually(t, 5*time.Second, func() bool {
		var last int64
		_ = e.App.DB.Read.QueryRow(`SELECT last_seq FROM realtime_streams WHERE player_id=?`, playerID).Scan(&last)
		return last == n
	})
}
