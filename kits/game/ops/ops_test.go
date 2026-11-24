package ops_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pillar/gostack/app"
	"github.com/pillar/gostack/kits/game/gametest"
	"github.com/pillar/gostack/kits/game/ops"
)

// clock is real time plus an adjustable offset, so it stays consistent with the
// server's own timestamps (player registration) while tests can jump forward.
type clock struct{ off atomic.Int64 }

func (c *clock) Now() time.Time      { return time.Now().Add(time.Duration(c.off.Load())) }
func (c *clock) Set(t time.Time)     { c.off.Store(int64(time.Until(t))) }
func (c *clock) Add(d time.Duration) { c.off.Add(int64(d)) }
func utc(s string) time.Time         { t, _ := time.Parse(time.RFC3339, s); return t }
func admin(g, p string) string       { return "/v1/admin/games/" + g + p }
func pub(g, p string) string         { return "/v1/games/" + g + p }
func obj(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

type fixture struct {
	*gametest.Env
	M   *ops.Module
	Clk *clock
}

func setup(t *testing.T, mod func(*ops.Module)) *fixture {
	t.Helper()
	f := &fixture{Clk: &clock{}}
	f.Env = gametest.New(t, gametest.Options{Routes: func(mux *http.ServeMux, a *app.App) {
		f.M = &ops.Module{Kit: a.Game, DB: a.DB, Now: f.Clk.Now}
		if mod != nil {
			mod(f.M)
		}
		f.M.Routes(mux)
	}})
	return f
}

func (f *fixture) balance(g string, p gametest.Player, asset string) int64 {
	f.T.Helper()
	r := f.Expect(f.Do("GET", pub(g, "/wallet"), p.Token, nil), 200)
	b, _ := r.Body["balances"].(map[string]any)
	v, _ := b[asset].(float64)
	return int64(v)
}

func (f *fixture) count(q string, args ...any) int {
	f.T.Helper()
	var n int
	if err := f.App.DB.Read.QueryRow(q, args...).Scan(&n); err != nil {
		f.T.Fatal(err)
	}
	return n
}

// burst runs fn n times concurrently and returns the status codes.
func burst(n int, fn func(i int) int) map[int]int {
	var mu sync.Mutex
	out := map[int]int{}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := fn(i)
			mu.Lock()
			out[s]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

func TestAnnouncements(t *testing.T) {
	f := setup(t, nil)
	p := f.Guest("alpha")
	now := f.Clk.Now()
	put := func(id string, body map[string]any) {
		f.Expect(f.Do("PUT", admin("alpha", "/announcements/"+id), f.AdminToken, body), 200)
	}
	put("live", obj("title", "Live", "body", "b", "priority", 1))
	put("high", obj("title", "High", "body", "b", "priority", 9, "starts_at", now.Add(-time.Hour).UnixMilli(), "ends_at", now.Add(time.Hour).Format(time.RFC3339)))
	put("future", obj("title", "Future", "body", "b", "starts_at", now.Add(time.Hour).UnixMilli()))
	put("past", obj("title", "Past", "body", "b", "starts_at", now.Add(-2*time.Hour).UnixMilli(), "ends_at", now.Add(-time.Hour).UnixMilli()))
	put("wx", obj("title", "WeChat only", "body", "b", "platforms", []string{"wechat"}))

	ids := func(r gametest.Resp) string {
		var s []string
		for _, a := range r.Body["announcements"].([]any) {
			s = append(s, a.(map[string]any)["id"].(string))
		}
		return strings.Join(s, ",")
	}
	// Public (no token) and authenticated both work; priority ordering; platform filter.
	if got := ids(f.Expect(f.Do("GET", pub("alpha", "/announcements"), "", nil), 200)); got != "high,live" {
		t.Fatalf("got %s", got)
	}
	if got := ids(f.Expect(f.Do("GET", pub("alpha", "/announcements?platform=wechat"), p.Token, nil), 200)); got != "high,live,wx" {
		t.Fatalf("got %s", got)
	}
	f.Clk.Add(90 * time.Minute) // high ended, future started
	if got := ids(f.Expect(f.Do("GET", pub("alpha", "/announcements"), "", nil), 200)); got != "live,future" {
		t.Fatalf("got %s", got)
	}
	// Other game sees nothing; admin list shows everything; non-admin cannot write.
	if got := ids(f.Expect(f.Do("GET", pub("beta", "/announcements"), "", nil), 200)); got != "" {
		t.Fatalf("beta saw %s", got)
	}
	if got := ids(f.Expect(f.Do("GET", admin("alpha", "/announcements"), f.AdminToken, nil), 200)); len(strings.Split(got, ",")) != 5 {
		t.Fatalf("admin list %s", got)
	}
	f.Expect(f.Do("PUT", admin("alpha", "/announcements/x"), p.Token, obj("title", "t")), 401)
	f.Expect(f.Do("PUT", admin("alpha", "/announcements/x"), f.AdminToken, obj("body", "no title")), 400)
	f.Expect(f.Do("PUT", admin("alpha", "/announcements/x"), f.AdminToken, obj("title", "t", "starts_at", 10, "ends_at", 5)), 400)
	f.Expect(f.Do("PUT", admin("alpha", "/announcements/BAD ID"), f.AdminToken, obj("title", "t")), 404)
	f.Expect(f.Do("DELETE", admin("alpha", "/announcements/live"), f.AdminToken, nil), 204)
	f.Expect(f.Do("DELETE", admin("alpha", "/announcements/live"), f.AdminToken, nil), 404)
}

func TestMailDirectBroadcastAndClaim(t *testing.T) {
	f := setup(t, nil)
	a, b := f.Guest("alpha"), f.Guest("alpha")
	other := f.Guest("beta")
	time.Sleep(5 * time.Millisecond)

	f.Expect(f.Do("POST", admin("alpha", "/mail"), f.AdminToken, obj("player_id", a.ID, "title", "Hi A", "body", "x", "grants", obj("coin", 50))), 201)
	r := f.Expect(f.Do("POST", admin("alpha", "/mail"), f.AdminToken, obj("broadcast", true, "title", "All", "body", "x", "grants", obj("gem", 7))), 201)
	bcast := r.Body["mail_id"].(string)
	time.Sleep(5 * time.Millisecond)
	late := f.Guest("alpha") // registered after the broadcast: does not receive it
	// Broadcast is stored once, no per-player rows.
	if n := f.count(`SELECT count(*) FROM ops_mail`); n != 2 {
		t.Fatalf("mail rows %d", n)
	}
	if n := f.count(`SELECT count(*) FROM ops_mail_state`); n != 0 {
		t.Fatalf("state rows %d", n)
	}
	list := func(p gametest.Player) []any {
		return f.Expect(f.Do("GET", pub("alpha", "/mail"), p.Token, nil), 200).Body["mail"].([]any)
	}
	if len(list(a)) != 2 || len(list(b)) != 1 || len(list(late)) != 0 {
		t.Fatal("unexpected mailbox sizes")
	}
	// Invalid recipient / both / neither.
	f.Expect(f.Do("POST", admin("alpha", "/mail"), f.AdminToken, obj("player_id", "nobody", "title", "t")), 404)
	f.Expect(f.Do("POST", admin("alpha", "/mail"), f.AdminToken, obj("player_id", a.ID, "broadcast", true, "title", "t")), 400)
	f.Expect(f.Do("POST", admin("alpha", "/mail"), f.AdminToken, obj("title", "t")), 400)
	f.Expect(f.Do("POST", admin("alpha", "/mail"), f.AdminToken, obj("broadcast", true, "title", "t", "grants", obj("coin", 0))), 400)
	// Admin idempotency: same key does not send twice.
	body := obj("broadcast", true, "title", "Once", "body", "x")
	f.Expect(f.Do("POST", admin("alpha", "/mail"), f.AdminToken, body, "Idempotency-Key", "m1"), 201)
	rr := f.Expect(f.Do("POST", admin("alpha", "/mail"), f.AdminToken, body, "Idempotency-Key", "m1"), 201)
	if rr.Header.Get("Idempotent-Replayed") != "true" || f.count(`SELECT count(*) FROM ops_mail WHERE title='Once'`) != 1 {
		t.Fatal("admin send not idempotent")
	}

	// Read, claim, claim again, cross-player and cross-game isolation.
	f.Expect(f.Do("POST", pub("alpha", "/mail/"+bcast+"/read"), b.Token, nil), 204)
	f.Expect(f.Do("POST", pub("alpha", "/mail/"+bcast+"/claim"), late.Token, nil), 404)
	f.Expect(f.Do("POST", pub("beta", "/mail/"+bcast+"/claim"), other.Token, nil), 404)
	f.Expect(f.Do("POST", pub("alpha", "/mail/"+bcast+"/claim"), b.Token, nil), 200)
	if c := f.Expect(f.Do("POST", pub("alpha", "/mail/"+bcast+"/claim"), b.Token, nil), 409).Code(); c != "already_claimed" {
		t.Fatal(c)
	}
	// The broadcast is still claimable by another player independently.
	f.Expect(f.Do("POST", pub("alpha", "/mail/"+bcast+"/claim"), a.Token, nil), 200)
	if f.balance("alpha", a, "gem") != 7 || f.balance("alpha", b, "gem") != 7 {
		t.Fatal("balances")
	}
	// Idempotent replay of a claim returns the stored response without error.
	var direct string
	for _, x := range list(a) {
		if m := x.(map[string]any); m["title"] == "Hi A" {
			direct = m["id"].(string)
		}
	}
	c1 := f.Expect(f.Do("POST", pub("alpha", "/mail/"+direct+"/claim"), a.Token, nil, "Idempotency-Key", "k"), 200)
	c2 := f.Expect(f.Do("POST", pub("alpha", "/mail/"+direct+"/claim"), a.Token, nil, "Idempotency-Key", "k"), 200)
	if c2.Header.Get("Idempotent-Replayed") != "true" || string(c1.Raw) != string(c2.Raw) {
		t.Fatal("claim replay")
	}
	if got := f.balance("alpha", a, "coin"); got != 50 {
		t.Fatalf("coin %d", got)
	}
	// Mail without attachment cannot be claimed.
	r = f.Expect(f.Do("POST", admin("alpha", "/mail"), f.AdminToken, obj("player_id", b.ID, "title", "Plain")), 201)
	if c := f.Expect(f.Do("POST", pub("alpha", "/mail/"+r.Body["mail_id"].(string)+"/claim"), b.Token, nil), 409).Code(); c != "no_attachment" {
		t.Fatal(c)
	}
}

func TestMailConcurrentClaimPaysOnce(t *testing.T) {
	f := setup(t, nil)
	p := f.Guest("alpha")
	r := f.Expect(f.Do("POST", admin("alpha", "/mail"), f.AdminToken, obj("player_id", p.ID, "title", "Gift", "grants", obj("coin", 100))), 201)
	id := r.Body["mail_id"].(string)
	got := burst(12, func(int) int { return f.Do("POST", pub("alpha", "/mail/"+id+"/claim"), p.Token, nil).Status })
	if got[200] != 1 || got[409] != 11 {
		t.Fatalf("statuses %v", got)
	}
	if f.balance("alpha", p, "coin") != 100 {
		t.Fatal("paid more than once")
	}
	if n := f.count(`SELECT count(*) FROM game_ledger WHERE source_key=?`, "mail:"+id+":"+p.ID); n != 1 {
		t.Fatalf("ledger rows %d", n)
	}
}

func TestMailDeleteExpiryAndMaintain(t *testing.T) {
	f := setup(t, nil)
	p := f.Guest("alpha")
	time.Sleep(5 * time.Millisecond)
	send := func(body map[string]any) string {
		return f.Expect(f.Do("POST", admin("alpha", "/mail"), f.AdminToken, body), 201).Body["mail_id"].(string)
	}
	gift := send(obj("player_id", p.ID, "title", "Gift", "grants", obj("coin", 5)))
	plain := send(obj("player_id", p.ID, "title", "Plain"))
	exp := send(obj("player_id", p.ID, "title", "Expiring", "grants", obj("coin", 9), "expires_at", f.Clk.Now().Add(time.Hour).UnixMilli()))

	// Unclaimed attachment: delete refused unless discard=true; plain mail deletes freely.
	if c := f.Expect(f.Do("DELETE", pub("alpha", "/mail/"+gift), p.Token, nil), 409).Code(); c != "unclaimed_attachments" {
		t.Fatal(c)
	}
	f.Expect(f.Do("DELETE", pub("alpha", "/mail/"+plain), p.Token, nil), 204)
	f.Expect(f.Do("DELETE", pub("alpha", "/mail/"+plain), p.Token, nil), 404)
	f.Expect(f.Do("DELETE", pub("alpha", "/mail/"+gift+"?discard=true"), p.Token, nil), 204)
	f.Expect(f.Do("POST", pub("alpha", "/mail/"+gift+"/claim"), p.Token, nil), 404) // hidden mail cannot be claimed
	if f.balance("alpha", p, "coin") != 0 {
		t.Fatal("forfeited mail paid")
	}
	// Expired mail disappears and cannot be claimed.
	f.Clk.Add(2 * time.Hour)
	f.Expect(f.Do("POST", pub("alpha", "/mail/"+exp+"/claim"), p.Token, nil), 404)
	if l := f.Expect(f.Do("GET", pub("alpha", "/mail"), p.Token, nil), 200).Body["mail"].([]any); len(l) != 0 {
		t.Fatalf("mailbox %v", l)
	}
	// Maintain removes mail expired more than 7 days ago, with state.
	if err := f.M.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.count(`SELECT count(*) FROM ops_mail WHERE mail_id=?`, exp) != 1 {
		t.Fatal("removed too early")
	}
	f.Clk.Add(8 * 24 * time.Hour)
	if err := f.M.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.count(`SELECT count(*) FROM ops_mail WHERE mail_id=?`, exp) != 0 || f.count(`SELECT count(*) FROM ops_mail_state WHERE mail_id=?`, exp) != 0 {
		t.Fatal("expired mail not cleaned")
	}
	// Admin recall.
	id := send(obj("broadcast", true, "title", "Recall", "grants", obj("coin", 1)))
	f.Expect(f.Do("DELETE", admin("alpha", "/mail/"+id), f.AdminToken, nil), 204)
	f.Expect(f.Do("POST", pub("alpha", "/mail/"+id+"/claim"), p.Token, nil), 404)
	f.Expect(f.Do("GET", admin("alpha", "/mail"), f.AdminToken, nil), 200)
}

func TestCheckin(t *testing.T) {
	f := setup(t, nil)
	p := f.Guest("alpha")
	f.Expect(f.Do("GET", pub("alpha", "/checkin"), p.Token, nil), 404) // not configured
	cfg := obj("cycle", []any{obj("coin", 10), obj("coin", 20), obj("coin", 30, "gem", 1)}, "timezone", "Asia/Shanghai", "streak_reset", true)
	f.Expect(f.Do("PUT", admin("alpha", "/checkin"), f.AdminToken, obj("cycle", []any{}, "timezone", "UTC")), 400)
	f.Expect(f.Do("PUT", admin("alpha", "/checkin"), f.AdminToken, obj("cycle", []any{obj("coin", 1)}, "timezone", "Mars/Base")), 400)
	f.Expect(f.Do("PUT", admin("alpha", "/checkin"), p.Token, cfg), 401)
	f.Expect(f.Do("PUT", admin("alpha", "/checkin"), f.AdminToken, cfg), 200)

	// 15:30 UTC is 23:30 in Shanghai.
	f.Clk.Set(utc("2026-03-01T15:30:00Z"))
	st := f.Expect(f.Do("GET", pub("alpha", "/checkin"), p.Token, nil), 200).Body
	if st["today"] != "2026-03-01" || st["checked_in_today"] != false || st["streak"] != 0.0 {
		t.Fatalf("%v", st)
	}
	r := f.Expect(f.Do("POST", pub("alpha", "/checkin"), p.Token, nil), 200).Body
	if r["day"] != "2026-03-01" || r["streak"] != 1.0 {
		t.Fatalf("%v", r)
	}
	if c := f.Expect(f.Do("POST", pub("alpha", "/checkin"), p.Token, nil), 409).Code(); c != "already_checked_in" {
		t.Fatal(c)
	}
	if f.balance("alpha", p, "coin") != 10 {
		t.Fatal("day 1 reward")
	}
	// One hour later it is already 2026-03-02 in Shanghai (still 03-01 in UTC): a new claim is allowed.
	f.Clk.Set(utc("2026-03-01T16:30:00Z"))
	st = f.Expect(f.Do("GET", pub("alpha", "/checkin"), p.Token, nil), 200).Body
	if st["today"] != "2026-03-02" || st["checked_in_today"] != false || st["next_index"] != 1.0 {
		t.Fatalf("%v", st)
	}
	f.Expect(f.Do("POST", pub("alpha", "/checkin"), p.Token, nil), 200)
	f.Clk.Add(24 * time.Hour)
	r = f.Expect(f.Do("POST", pub("alpha", "/checkin"), p.Token, nil), 200).Body
	if r["streak"] != 3.0 || r["index"] != 2.0 {
		t.Fatalf("%v", r)
	}
	if f.balance("alpha", p, "coin") != 60 || f.balance("alpha", p, "gem") != 1 {
		t.Fatal("balances after 3 days")
	}
	// Missing a day resets the streak when streak_reset is on.
	f.Clk.Add(48 * time.Hour)
	if st = f.Expect(f.Do("GET", pub("alpha", "/checkin"), p.Token, nil), 200).Body; st["streak"] != 0.0 {
		t.Fatalf("%v", st)
	}
	r = f.Expect(f.Do("POST", pub("alpha", "/checkin"), p.Token, nil), 200).Body
	if r["streak"] != 1.0 || r["index"] != 0.0 {
		t.Fatalf("%v", r)
	}
	// Without reset the streak continues after a gap and the cycle wraps.
	cfg["streak_reset"] = false
	f.Expect(f.Do("PUT", admin("alpha", "/checkin"), f.AdminToken, cfg), 200)
	f.Clk.Add(72 * time.Hour)
	r = f.Expect(f.Do("POST", pub("alpha", "/checkin"), p.Token, nil), 200).Body
	if r["streak"] != 2.0 {
		t.Fatalf("%v", r)
	}
	// Concurrent same-day claims pay once.
	q := f.Guest("alpha")
	got := burst(8, func(int) int { return f.Do("POST", pub("alpha", "/checkin"), q.Token, nil).Status })
	if got[200] != 1 || got[409] != 7 {
		t.Fatalf("%v", got)
	}
	// Other game is not configured.
	o := f.Guest("beta")
	f.Expect(f.Do("POST", pub("beta", "/checkin"), o.Token, nil), 404)
}

func TestTasks(t *testing.T) {
	f := setup(t, nil)
	p, q := f.Guest("alpha"), f.Guest("alpha")
	put := func(id string, body map[string]any) {
		f.Expect(f.Do("PUT", admin("alpha", "/tasks/"+id), f.AdminToken, body), 200)
	}
	put("win3", obj("metric", "wins", "target", 3, "period", "daily", "grants", obj("coin", 30), "timezone", "UTC"))
	put("first", obj("metric", "wins", "target", 1, "period", "once", "grants", obj("gem", 1)))
	put("week", obj("metric", "wins", "target", 5, "period", "weekly", "grants", obj("coin", 100)))
	f.Expect(f.Do("PUT", admin("alpha", "/tasks/bad"), f.AdminToken, obj("metric", "wins", "target", 0, "period", "daily", "grants", obj("coin", 1))), 400)
	f.Expect(f.Do("PUT", admin("alpha", "/tasks/bad"), f.AdminToken, obj("metric", "wins", "target", 1, "period", "hourly", "grants", obj("coin", 1))), 400)
	f.Expect(f.Do("PUT", admin("alpha", "/tasks/bad"), f.AdminToken, obj("metric", "wins", "target", 1, "period", "daily", "grants", obj("coin", 1), "timezone", "Nope/Zone")), 400)

	task := func(p gametest.Player, id string) map[string]any {
		for _, x := range f.Expect(f.Do("GET", pub("alpha", "/tasks"), p.Token, nil), 200).Body["tasks"].([]any) {
			if m := x.(map[string]any); m["id"] == id {
				return m
			}
		}
		t.Fatalf("task %s missing", id)
		return nil
	}
	f.Expect(f.Do("POST", pub("alpha", "/tasks/win3/claim"), p.Token, nil, "Idempotency-Key", "a"), 409) // not completed
	// Game code advances progress inside its own transaction.
	for i := 0; i < 2; i++ {
		err := f.App.DB.WriteTx(context.Background(), func(tx *sql.Tx) error { return f.M.Progress(context.Background(), tx, "alpha", p.ID, "wins", 1) })
		if err != nil {
			t.Fatal(err)
		}
	}
	if m := task(p, "win3"); m["progress"] != 2.0 || m["completed"] != false {
		t.Fatalf("%v", m)
	}
	if m := task(q, "win3"); m["progress"] != 0.0 {
		t.Fatalf("player isolation: %v", m)
	}
	// Progress through the admin endpoint; clients cannot post progress (no such route).
	f.Expect(f.Do("POST", admin("alpha", "/progress"), f.AdminToken, obj("player_id", p.ID, "metric", "wins", "delta", 1)), 200)
	f.Expect(f.Do("POST", admin("alpha", "/progress"), f.AdminToken, obj("player_id", "nobody", "metric", "wins", "delta", 1)), 404)
	f.Expect(f.Do("POST", admin("alpha", "/progress"), f.AdminToken, obj("player_id", p.ID, "metric", "wins", "delta", -1)), 400)
	f.Expect(f.Do("POST", admin("alpha", "/progress"), p.Token, obj("player_id", p.ID, "metric", "wins", "delta", 1)), 401)
	if s := f.Do("POST", pub("alpha", "/progress"), p.Token, obj("metric", "wins", "delta", 5)).Status; s != 404 && s != 405 {
		t.Fatalf("client progress endpoint exists: %d", s)
	}
	if err := f.App.DB.WriteTx(context.Background(), func(tx *sql.Tx) error { return f.M.Progress(context.Background(), tx, "alpha", p.ID, "wins", 0) }); err == nil {
		t.Fatal("zero delta accepted")
	}

	got := burst(8, func(int) int { return f.Do("POST", pub("alpha", "/tasks/win3/claim"), p.Token, nil).Status })
	if got[200] != 1 || got[409] != 7 {
		t.Fatalf("%v", got)
	}
	if f.balance("alpha", p, "coin") != 30 {
		t.Fatal("task paid more than once")
	}
	f.Expect(f.Do("POST", pub("alpha", "/tasks/first/claim"), p.Token, nil), 200)
	f.Expect(f.Do("POST", pub("alpha", "/tasks/first/claim"), p.Token, nil), 409)
	f.Expect(f.Do("POST", pub("alpha", "/tasks/week/claim"), p.Token, nil), 409) // 3/5
	f.Expect(f.Do("POST", pub("alpha", "/tasks/nope/claim"), p.Token, nil), 404)
	f.Expect(f.Do("POST", pub("beta", "/tasks/win3/claim"), p.Token, nil), 403)

	// Next UTC day: daily task resets and pays again; "once" stays claimed; weekly keeps accumulating.
	f.Clk.Add(24 * time.Hour)
	if m := task(p, "win3"); m["progress"] != 0.0 || m["claimed"] != false {
		t.Fatalf("daily not reset: %v", m)
	}
	if m := task(p, "first"); m["claimed"] != true {
		t.Fatalf("%v", m)
	}
	f.Expect(f.Do("POST", admin("alpha", "/progress"), f.AdminToken, obj("player_id", p.ID, "metric", "wins", "delta", 3)), 200)
	f.Expect(f.Do("POST", pub("alpha", "/tasks/win3/claim"), p.Token, nil), 200)
	if f.balance("alpha", p, "coin") != 60 {
		t.Fatal("second day payout")
	}
	f.Expect(f.Do("DELETE", admin("alpha", "/tasks/win3"), f.AdminToken, nil), 204)
	f.Expect(f.Do("POST", pub("alpha", "/tasks/win3/claim"), p.Token, nil), 404)
}

func TestTaskEventGate(t *testing.T) {
	f := setup(t, nil)
	p := f.Guest("alpha")
	now := f.Clk.Now()
	f.Expect(f.Do("PUT", admin("alpha", "/events/spring"), f.AdminToken, obj("title", "Spring", "timezone", "Asia/Shanghai", "metadata", obj("theme", "flowers"),
		"starts_at", now.Add(time.Hour).Format(time.RFC3339), "ends_at", now.Add(3*time.Hour).Format(time.RFC3339))), 200)
	f.Expect(f.Do("PUT", admin("alpha", "/events/bad"), f.AdminToken, obj("title", "x", "starts_at", 5, "ends_at", 4)), 400)
	f.Expect(f.Do("PUT", admin("alpha", "/events/bad"), f.AdminToken, obj("title", "x", "starts_at", 5, "ends_at", 9, "metadata", []int{1})), 400)
	f.Expect(f.Do("PUT", admin("alpha", "/tasks/ev"), f.AdminToken, obj("metric", "m", "target", 1, "period", "once", "grants", obj("coin", 5), "event_id", "spring")), 200)

	evs := func() int {
		return len(f.Expect(f.Do("GET", pub("alpha", "/events"), "", nil), 200).Body["events"].([]any))
	}
	tasks := func() int {
		return len(f.Expect(f.Do("GET", pub("alpha", "/tasks"), p.Token, nil), 200).Body["tasks"].([]any))
	}
	// Before the event: no active event, task hidden, progress ignored, claim refused.
	if evs() != 0 || tasks() != 0 {
		t.Fatal("event or task visible too early")
	}
	f.Expect(f.Do("POST", admin("alpha", "/progress"), f.AdminToken, obj("player_id", p.ID, "metric", "m", "delta", 1)), 200)
	f.Expect(f.Do("POST", pub("alpha", "/tasks/ev/claim"), p.Token, nil), 403)
	f.Clk.Add(2 * time.Hour)
	if evs() != 1 || tasks() != 1 {
		t.Fatal("event or task not visible while active")
	}
	if ev := f.Expect(f.Do("GET", pub("alpha", "/events"), "", nil), 200).Body["events"].([]any)[0].(map[string]any); ev["timezone"] != "Asia/Shanghai" || ev["metadata"].(map[string]any)["theme"] != "flowers" {
		t.Fatalf("%v", ev)
	}
	f.Expect(f.Do("POST", pub("alpha", "/tasks/ev/claim"), p.Token, nil), 409) // earlier progress was ignored
	f.Expect(f.Do("POST", admin("alpha", "/progress"), f.AdminToken, obj("player_id", p.ID, "metric", "m", "delta", 1)), 200)
	f.Expect(f.Do("POST", pub("alpha", "/tasks/ev/claim"), p.Token, nil), 200)
	f.Clk.Add(2 * time.Hour)
	if evs() != 0 {
		t.Fatal("event still active after end")
	}
	if f.Expect(f.Do("GET", pub("beta", "/events"), "", nil), 200); len(f.Expect(f.Do("GET", admin("alpha", "/events"), f.AdminToken, nil), 200).Body["events"].([]any)) != 1 {
		t.Fatal("admin list")
	}
	f.Expect(f.Do("DELETE", admin("alpha", "/events/spring"), f.AdminToken, nil), 204)
}

func (f *fixture) batch(g string, body map[string]any) gametest.Resp {
	return f.Expect(f.Do("POST", admin(g, "/redeem-batches"), f.AdminToken, body), 201)
}

func codesOf(r gametest.Resp) []string {
	var out []string
	for _, c := range r.Body["codes"].([]any) {
		out = append(out, c.(string))
	}
	return out
}

func TestRedeemSingleUseConcurrentAndHashed(t *testing.T) {
	f := setup(t, nil)
	a, b := f.Guest("alpha"), f.Guest("alpha")
	r := f.batch("alpha", obj("count", 3, "grants", obj("coin", 25), "label", "launch"))
	codes := codesOf(r)
	if len(codes) != 3 || len(codes[0]) != 14 {
		t.Fatalf("%v", codes)
	}
	// Only hashes are stored.
	var dump string
	rows, _ := f.App.DB.Read.Query(`SELECT code_hash FROM ops_redeem_codes`)
	for rows.Next() {
		var h string
		rows.Scan(&h)
		dump += h
	}
	rows.Close()
	for _, c := range codes {
		if strings.Contains(dump, strings.ReplaceAll(c, "-", "")) || strings.Contains(dump, c) || len(dump) != 3*64 {
			t.Fatal("plaintext code stored")
		}
	}
	// Two players race for one code: exactly one wins.
	players := []gametest.Player{a, b}
	got := burst(2, func(i int) int {
		return f.Do("POST", pub("alpha", "/redeem"), players[i].Token, obj("code", codes[0])).Status
	})
	if got[200] != 1 || got[400] != 1 {
		t.Fatalf("%v", got)
	}
	if f.balance("alpha", a, "coin")+f.balance("alpha", b, "coin") != 25 {
		t.Fatal("paid twice")
	}
	// The same player redeeming again gets the distinct error; normalisation ignores case and dashes.
	winner, loser := a, b
	if f.balance("alpha", a, "coin") == 0 {
		winner, loser = b, a
	}
	if c := f.Expect(f.Do("POST", pub("alpha", "/redeem"), winner.Token, obj("code", strings.ToLower(strings.ReplaceAll(codes[0], "-", "")))), 409).Code(); c != "already_redeemed" {
		t.Fatal(c)
	}
	// Exhausted, unknown and malformed codes are indistinguishable.
	var bodies []string
	for _, c := range []string{codes[0], "ZZZZ-ZZZZ-ZZZZ", "!!", ""} {
		x := f.Expect(f.Do("POST", pub("alpha", "/redeem"), loser.Token, obj("code", c)), 400)
		bodies = append(bodies, x.Code()+fmt.Sprint(x.Body["error"].(map[string]any)["message"]))
	}
	for _, s := range bodies {
		if s != bodies[0] || !strings.HasPrefix(s, "invalid_code") {
			t.Fatalf("non-uniform errors: %v", bodies)
		}
	}
	// A code redeemed in alpha is invalid in beta even with the same text.
	o := f.Guest("beta")
	f.Expect(f.Do("POST", pub("beta", "/redeem"), o.Token, obj("code", codes[1])), 400)
	f.Expect(f.Do("POST", pub("alpha", "/redeem"), loser.Token, obj("code", codes[1])), 200)
	// Idempotent replay returns the same success.
	r1 := f.Expect(f.Do("POST", pub("alpha", "/redeem"), winner.Token, obj("code", codes[2]), "Idempotency-Key", "r1"), 200)
	r2 := f.Expect(f.Do("POST", pub("alpha", "/redeem"), winner.Token, obj("code", codes[2]), "Idempotency-Key", "r1"), 200)
	if r2.Header.Get("Idempotent-Replayed") != "true" || string(r1.Raw) != string(r2.Raw) {
		t.Fatal("replay")
	}
	// Admin listing shows counts but never codes.
	l := f.Expect(f.Do("GET", admin("alpha", "/redeem-batches"), f.AdminToken, nil), 200)
	bt := l.Body["batches"].([]any)[0].(map[string]any)
	if bt["codes"] != 3.0 || bt["redeemed"] != 3.0 || strings.Contains(string(l.Raw), codes[0]) {
		t.Fatalf("%s", l.Raw)
	}
	f.Expect(f.Do("POST", pub("alpha", "/redeem"), "", obj("code", codes[0])), 401)
}

func TestRedeemSharedExpiryDisableAndValidation(t *testing.T) {
	f := setup(t, nil)
	ps := []gametest.Player{f.Guest("alpha"), f.Guest("alpha"), f.Guest("alpha")}
	f.batch("alpha", obj("shared", true, "code", "SPRING-2026", "max_redemptions", 2, "grants", obj("gem", 3)))
	// Duplicate custom code is refused; invalid shapes are 400.
	f.Expect(f.Do("POST", admin("alpha", "/redeem-batches"), f.AdminToken, obj("shared", true, "code", "spring2026", "max_redemptions", 2, "grants", obj("gem", 3))), 409)
	for _, bad := range []map[string]any{
		obj("grants", obj("gem", 1)),                 // no count
		obj("count", 0, "grants", obj("gem", 1)),     // count 0
		obj("count", 20000, "grants", obj("gem", 1)), // too many
		obj("count", 1),                              // no grants
		obj("shared", true, "grants", obj("gem", 1)), // no max_redemptions
		obj("shared", true, "count", 2, "max_redemptions", 1, "grants", obj("gem", 1)),
		obj("count", 1, "grants", obj("gem", 1), "expires_at", 1), // already expired
	} {
		f.Expect(f.Do("POST", admin("alpha", "/redeem-batches"), f.AdminToken, bad), 400)
	}
	f.Expect(f.Do("POST", pub("alpha", "/redeem"), ps[0].Token, obj("code", "spring-2026")), 200)
	f.Expect(f.Do("POST", pub("alpha", "/redeem"), ps[0].Token, obj("code", "SPRING2026")), 409)
	f.Expect(f.Do("POST", pub("alpha", "/redeem"), ps[1].Token, obj("code", "SPRING2026")), 200)
	f.Expect(f.Do("POST", pub("alpha", "/redeem"), ps[2].Token, obj("code", "SPRING2026")), 400) // exhausted
	if f.balance("alpha", ps[0], "gem") != 3 || f.balance("alpha", ps[2], "gem") != 0 {
		t.Fatal("shared balances")
	}
	// Concurrent shared code with capacity 3 and 8 players pays exactly 3 times.
	f.batch("alpha", obj("shared", true, "code", "RACE", "max_redemptions", 3, "grants", obj("coin", 1)))
	var many []gametest.Player
	for i := 0; i < 8; i++ {
		many = append(many, f.Guest("alpha"))
	}
	got := burst(8, func(i int) int {
		return f.Do("POST", pub("alpha", "/redeem"), many[i].Token, obj("code", "RACE")).Status
	})
	if got[200] != 3 || got[400] != 5 {
		t.Fatalf("%v", got)
	}
	// Expiry and disabling.
	exp := codesOf(f.batch("alpha", obj("count", 1, "grants", obj("coin", 9), "expires_at", f.Clk.Now().Add(time.Hour).UnixMilli())))[0]
	f.Clk.Add(2 * time.Hour)
	f.Expect(f.Do("POST", pub("alpha", "/redeem"), ps[2].Token, obj("code", exp)), 400)
	r := f.batch("alpha", obj("count", 1, "grants", obj("coin", 9)))
	f.Expect(f.Do("DELETE", admin("alpha", "/redeem-batches/"+r.Body["batch_id"].(string)), f.AdminToken, nil), 204)
	f.Expect(f.Do("DELETE", admin("alpha", "/redeem-batches/"+r.Body["batch_id"].(string)), f.AdminToken, nil), 204) // idempotent
	f.Expect(f.Do("DELETE", admin("alpha", "/redeem-batches/nope"), f.AdminToken, nil), 404)
	f.Expect(f.Do("POST", pub("alpha", "/redeem"), ps[2].Token, obj("code", codesOf(r)[0])), 400)
	f.Expect(f.Do("POST", pub("alpha", "/redeem"), ps[2].Token, `{"code":"X","extra":1}`), 400)
}

func TestRedeemRateLimits(t *testing.T) {
	f := setup(t, func(m *ops.Module) { m.RedeemPerPlayerPerMinute = 3; m.RedeemPerIPPerMinute = 5 })
	a := f.Guest("alpha")
	for i := 0; i < 3; i++ {
		f.Expect(f.Do("POST", pub("alpha", "/redeem"), a.Token, obj("code", "AAAA-BBBB")), 400)
	}
	r := f.Expect(f.Do("POST", pub("alpha", "/redeem"), a.Token, obj("code", "AAAA-BBBB")), 429)
	if r.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
	// Per-IP limit: other players share this test client's IP (5 total per minute; 3 used).
	b, c := f.Guest("alpha"), f.Guest("alpha")
	f.Expect(f.Do("POST", pub("alpha", "/redeem"), b.Token, obj("code", "AAAA-BBBB")), 400)
	f.Expect(f.Do("POST", pub("alpha", "/redeem"), b.Token, obj("code", "AAAA-BBBB")), 400)
	f.Expect(f.Do("POST", pub("alpha", "/redeem"), c.Token, obj("code", "AAAA-BBBB")), 429)
}
