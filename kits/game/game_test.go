package game_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pillar/gostack/adapters/pgtest"
	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/app"
	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/kits/game"
)

const docJSON = `{
  "client": {"welcome": "hi", "max_level": 30},
  "boards": {
    "main": {"order": "desc", "mode": "best", "period": "none", "min_score": 0, "max_score": 1000000, "quarantine_above": 500000},
    "speed": {"order": "asc", "mode": "best", "period": "daily", "min_score": 1, "max_score": 3600000},
    "total": {"order": "desc", "mode": "sum", "period": "weekly", "min_score": 0, "max_score": 1000}
  },
  "rewards": {
    "daily_login": {"grants": {"coin": 100}, "period": "daily", "per_period": 1},
    "starter": {"grants": {"coin": 50, "gem": 5}, "period": "once", "per_period": 1},
    "expired": {"grants": {"coin": 1}, "period": "once", "per_period": 1, "ends_at": 1}
  }
}`

type env struct {
	t      *testing.T
	a      *app.App
	srv    *httptest.Server
	cancel context.CancelFunc
	done   chan struct{}
}

func newEnv(t *testing.T, mutate ...func(*config.Config)) *env {
	t.Helper()
	cfg := config.Defaults()
	dir := t.TempDir()
	cfg.Database.Path = filepath.Join(dir, "app.db")
	cfg.Database.AnalyticsPath = filepath.Join(dir, "analytics.db")
	cfg.Kits = []string{"game"}
	cfg.Game.AuthRatePerMinute = 10000
	cfg.Game.ScoreRatePerMinute = 10000
	for _, m := range mutate {
		m(&cfg)
	}
	pgtest.UseScale(t, &cfg) // PostgreSQL when GOSTACK_TEST_POSTGRES_DSN is set
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	a, err := app.New(ctx, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []string{"alpha", "beta"} {
		if err := game.CreateGame(ctx, a.DB, "default", g, g, []string{"https://" + g + ".example"}); err != nil {
			t.Fatal(err)
		}
		if _, err := game.PublishConfig(ctx, a.DB, g, []byte(docJSON), "test", ""); err != nil {
			t.Fatal(err)
		}
	}
	e := &env{t: t, a: a, srv: httptest.NewServer(a.Handler(app.Hooks{})), done: make(chan struct{})}
	bg, cancel := context.WithCancel(ctx)
	e.cancel = cancel
	go func() { a.Game.Analytics.Run(bg, time.Second); close(e.done) }()
	t.Cleanup(func() {
		e.srv.Close()
		e.cancel()
		<-e.done
		a.Close()
	})
	return e
}

type resp struct {
	Status int
	Header http.Header
	Body   map[string]any
	Raw    []byte
}

func (e *env) do(method, path, token string, body any, hdr ...string) resp {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = bytes.NewBufferString(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	r := resp{Status: res.StatusCode, Header: res.Header, Raw: raw}
	_ = json.Unmarshal(raw, &r.Body)
	return r
}

func (r resp) code() string {
	if e, ok := r.Body["error"].(map[string]any); ok {
		return e["code"].(string)
	}
	return ""
}

func (e *env) expect(r resp, status int) resp {
	e.t.Helper()
	if r.Status != status {
		e.t.Fatalf("status %d want %d: %s", r.Status, status, r.Raw)
	}
	return r
}

type player struct{ id, guestID, secret, token string }

func (e *env) guest(gameID string) player {
	e.t.Helper()
	r := e.expect(e.do("POST", "/v1/games/"+gameID+"/auth/guest", "", "{}"), 201)
	return player{r.Body["player_id"].(string), r.Body["guest_id"].(string), r.Body["guest_secret"].(string), r.Body["access_token"].(string)}
}

func TestGuestLifecycle(t *testing.T) {
	e := newEnv(t)
	p := e.guest("alpha")
	// G02: logging in again with the stored credential returns the same player.
	r := e.expect(e.do("POST", "/v1/games/alpha/auth/guest", "", map[string]any{"guest_id": p.guestID, "guest_secret": p.secret}), 200)
	if r.Body["player_id"] != p.id || r.Body["guest_secret"] != nil {
		t.Fatalf("relogin: %s", r.Raw)
	}
	e.expect(e.do("POST", "/v1/games/alpha/auth/guest", "", map[string]any{"guest_id": p.guestID, "guest_secret": "wrong"}), 401)
	// Rotation invalidates the old secret.
	r = e.expect(e.do("POST", "/v1/games/alpha/auth/guest", "", map[string]any{"guest_id": p.guestID, "guest_secret": p.secret, "rotate": true}), 200)
	newSecret := r.Body["guest_secret"].(string)
	e.expect(e.do("POST", "/v1/games/alpha/auth/guest", "", map[string]any{"guest_id": p.guestID, "guest_secret": p.secret}), 401)
	e.expect(e.do("POST", "/v1/games/alpha/auth/guest", "", map[string]any{"guest_id": p.guestID, "guest_secret": newSecret}), 200)
	// Logout revokes immediately.
	me := e.expect(e.do("GET", "/v1/games/alpha/me", p.token, nil), 200)
	if me.Body["player_id"] != p.id {
		t.Fatal("me mismatch")
	}
	e.expect(e.do("POST", "/v1/games/alpha/auth/logout", p.token, nil), 204)
	e.expect(e.do("GET", "/v1/games/alpha/me", p.token, nil), 401)
	// Unknown and disabled games.
	e.expect(e.do("POST", "/v1/games/nope/auth/guest", "", "{}"), 404)
	if _, err := e.a.DB.Write.Exec(`UPDATE games SET status='disabled' WHERE game_id='beta'`); err != nil {
		t.Fatal(err)
	}
	e.expect(e.do("POST", "/v1/games/beta/auth/guest", "", "{}"), 403)
}

// G-A01: tokens are bound to one game and one player.
func TestCrossGameAndCrossPlayerIsolation(t *testing.T) {
	e := newEnv(t)
	a1, a2, b1 := e.guest("alpha"), e.guest("alpha"), e.guest("beta")
	e.expect(e.do("PUT", "/v1/games/alpha/saves/main", a1.token, map[string]any{"expected_revision": 0, "schema_version": 1, "payload": map[string]int{"gold": 1}}), 200)
	// alpha token on beta → forbidden.
	if r := e.do("GET", "/v1/games/beta/saves/main", a1.token, nil); r.Status != 403 {
		t.Fatalf("cross-game read status %d", r.Status)
	}
	if r := e.do("POST", "/v1/games/beta/rewards/starter/claim", a1.token, "{}"); r.Status != 403 {
		t.Fatalf("cross-game claim status %d", r.Status)
	}
	// Another alpha player cannot see a1's save (scoped by session, not by URL/body).
	e.expect(e.do("GET", "/v1/games/alpha/saves/main", a2.token, nil), 404)
	e.expect(e.do("GET", "/v1/games/beta/saves/main", b1.token, nil), 404)
	// Unknown body fields (e.g. an attempted player_id override) are rejected.
	e.expect(e.do("PUT", "/v1/games/alpha/saves/main", a2.token, map[string]any{"expected_revision": 0, "payload": 1, "player_id": a1.id}), 400)
}

// G-A03: two devices saving from the same revision — exactly one wins.
func TestSaveConflictAndIdempotency(t *testing.T) {
	e := newEnv(t)
	p := e.guest("alpha")
	put := func(rev int, payload any, key string) resp {
		h := []string{}
		if key != "" {
			h = []string{"Idempotency-Key", key}
		}
		return e.do("PUT", "/v1/games/alpha/saves/main", p.token, map[string]any{"expected_revision": rev, "schema_version": 1, "payload": payload}, h...)
	}
	e.expect(put(0, map[string]int{"lvl": 1}, ""), 200)
	var wg sync.WaitGroup
	results := make([]resp, 2)
	for i := range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); results[i] = put(1, map[string]int{"lvl": 10 + i}, "") }()
	}
	wg.Wait()
	ok, conflict := 0, 0
	for _, r := range results {
		switch {
		case r.Status == 200:
			ok++
		case r.Status == 409 && r.code() == "revision_conflict":
			conflict++
			if r.Body["error"].(map[string]any)["details"].(map[string]any)["current_revision"].(float64) != 2 {
				t.Fatalf("conflict details %s", r.Raw)
			}
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("ok=%d conflict=%d", ok, conflict)
	}
	// Idempotent retry replays the original response even though the revision moved on.
	first := e.expect(put(2, map[string]int{"lvl": 20}, "k-1"), 200)
	again := e.expect(put(2, map[string]int{"lvl": 20}, "k-1"), 200)
	if again.Header.Get("Idempotent-Replayed") != "true" || again.Body["revision"] != first.Body["revision"] {
		t.Fatalf("replay mismatch: %s vs %s", first.Raw, again.Raw)
	}
	if r := put(2, map[string]int{"lvl": 99}, "k-1"); r.Status != 409 || r.code() != "idempotency_key_reused" {
		t.Fatalf("key reuse: %d %s", r.Status, r.Raw)
	}
	got := e.expect(e.do("GET", "/v1/games/alpha/saves/main", p.token, nil), 200)
	if got.Body["revision"].(float64) != 3 || got.Body["payload"].(map[string]any)["lvl"].(float64) != 20 {
		t.Fatalf("final save %s", got.Raw)
	}
	// Size and slot limits.
	big := map[string]string{"x": string(bytes.Repeat([]byte("a"), 70<<10))}
	if r := e.do("PUT", "/v1/games/alpha/saves/big", p.token, map[string]any{"expected_revision": 0, "payload": big}); r.Status != 413 {
		t.Fatalf("oversize status %d", r.Status)
	}
	for i := 0; i < 4; i++ {
		e.expect(e.do("PUT", fmt.Sprintf("/v1/games/alpha/saves/s%d", i), p.token, map[string]any{"expected_revision": 0, "payload": 1}), 200)
	}
	if r := e.do("PUT", "/v1/games/alpha/saves/s9", p.token, map[string]any{"expected_revision": 0, "payload": 1}); r.Status != 409 || r.code() != "slot_limit" {
		t.Fatalf("slot limit: %d %s", r.Status, r.Raw)
	}
	var hist int
	e.a.DB.Read.QueryRow(`SELECT count(*) FROM save_revisions WHERE game_id='alpha' AND player_id=? AND slot='main'`, p.id).Scan(&hist)
	if hist != 3 {
		t.Fatalf("history rows=%d", hist)
	}
}

// G-A02: concurrent claims of the same reward post exactly one ledger entry per asset.
func TestRewardClaimConcurrencyAndLedger(t *testing.T) {
	e := newEnv(t)
	p := e.guest("alpha")
	const n = 20
	var wg sync.WaitGroup
	statuses := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Distinct idempotency keys: these are genuinely different requests.
			statuses[i] = e.do("POST", "/v1/games/alpha/rewards/daily_login/claim", p.token, "{}", "Idempotency-Key", fmt.Sprintf("c-%d", i)).Status
		}()
	}
	wg.Wait()
	ok := 0
	for _, s := range statuses {
		if s == 200 {
			ok++
		} else if s != 409 {
			t.Fatalf("unexpected status %d", s)
		}
	}
	if ok != 1 {
		t.Fatalf("%d successful claims", ok)
	}
	var ledger int
	e.a.DB.Read.QueryRow(`SELECT count(*) FROM game_ledger WHERE game_id='alpha' AND player_id=?`, p.id).Scan(&ledger)
	if ledger != 1 {
		t.Fatalf("ledger rows=%d", ledger)
	}
	r := e.expect(e.do("POST", "/v1/games/alpha/rewards/starter/claim", p.token, "{}", "Idempotency-Key", "s1"), 200)
	if b := r.Body["balances"].(map[string]any); b["coin"].(float64) != 150 || b["gem"].(float64) != 5 {
		t.Fatalf("balances %s", r.Raw)
	}
	replay := e.expect(e.do("POST", "/v1/games/alpha/rewards/starter/claim", p.token, "{}", "Idempotency-Key", "s1"), 200)
	if replay.Body["claim_id"] != r.Body["claim_id"] {
		t.Fatal("replay returned a different claim")
	}
	if r := e.do("POST", "/v1/games/alpha/rewards/starter/claim", p.token, "{}"); r.code() != "already_claimed" {
		t.Fatalf("second claim %s", r.Raw)
	}
	e.expect(e.do("POST", "/v1/games/alpha/rewards/expired/claim", p.token, "{}"), 403)
	e.expect(e.do("POST", "/v1/games/alpha/rewards/missing/claim", p.token, "{}"), 404)
	w := e.expect(e.do("GET", "/v1/games/alpha/wallet", p.token, nil), 200)
	if w.Body["balances"].(map[string]any)["coin"].(float64) != 150 {
		t.Fatalf("wallet %s", w.Raw)
	}
	ms, err := game.Reconcile(context.Background(), e.a.DB.Read, "")
	if err != nil || len(ms) != 0 {
		t.Fatalf("reconcile %v %v", ms, err)
	}
}

func TestLedgerRejectsOverdraftAndDuplicateSource(t *testing.T) {
	e := newEnv(t)
	p := e.guest("alpha")
	ctx := context.Background()
	apply := func(src string, d map[string]int64) error {
		tx, err := e.a.DB.Write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := game.ApplyLedger(ctx, tx, "alpha", p.id, "test", src, d, time.Now().UnixMilli()); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err := apply("grant", map[string]int64{"coin": 10}); err != nil {
		t.Fatal(err)
	}
	if err := apply("spend-1", map[string]int64{"coin": -11}); err == nil {
		t.Fatal("overdraft allowed")
	}
	if err := apply("grant", map[string]int64{"coin": 10}); err == nil {
		t.Fatal("duplicate source key posted twice")
	}
	if err := apply("spend-2", map[string]int64{"coin": -10}); err != nil {
		t.Fatal(err)
	}
	if ms, _ := game.Reconcile(ctx, e.a.DB.Read, "alpha"); len(ms) != 0 {
		t.Fatalf("mismatch %v", ms)
	}
	// A tampered balance is detected by reconciliation.
	e.a.DB.Write.Exec(`UPDATE wallet_balances SET balance=999 WHERE player_id=?`, p.id)
	if ms, _ := game.Reconcile(ctx, e.a.DB.Read, "alpha"); len(ms) != 1 {
		t.Fatalf("tamper not detected: %v", ms)
	}
}

func TestLeaderboard(t *testing.T) {
	e := newEnv(t)
	var ps []player
	scores := []int{500, 900, 900, 100, 700}
	for _, s := range scores {
		p := e.guest("alpha")
		ps = append(ps, p)
		e.expect(e.do("POST", "/v1/games/alpha/boards/main/scores", p.token, map[string]int{"score": s}), 200)
		time.Sleep(2 * time.Millisecond) // distinct achieved_at for deterministic tie-break
	}
	// Lower score does not replace best.
	r := e.expect(e.do("POST", "/v1/games/alpha/boards/main/scores", ps[0].token, map[string]int{"score": 10}), 200)
	if r.Body["best"].(float64) != 500 {
		t.Fatalf("best %s", r.Raw)
	}
	// Out of range and quarantine.
	e.expect(e.do("POST", "/v1/games/alpha/boards/main/scores", ps[0].token, map[string]int{"score": 2000000}), 400)
	q := e.expect(e.do("POST", "/v1/games/alpha/boards/main/scores", ps[3].token, map[string]int{"score": 600000}), 200)
	if q.Body["status"] != "quarantined" {
		t.Fatalf("not quarantined %s", q.Raw)
	}
	page1 := e.expect(e.do("GET", "/v1/games/alpha/boards/main?limit=2", ps[3].token, nil), 200)
	ents := page1.Body["entries"].([]any)
	first, second := ents[0].(map[string]any), ents[1].(map[string]any)
	// Tie at 900: earlier achiever ranks first.
	if first["player_id"] != ps[1].id || second["player_id"] != ps[2].id || second["rank"].(float64) != 2 {
		t.Fatalf("page1 %s", page1.Raw)
	}
	if me := page1.Body["me"].(map[string]any); me["rank"].(float64) != 5 || me["score"].(float64) != 100 {
		t.Fatalf("me %s", page1.Raw)
	}
	cur := page1.Body["next_cursor"].(string)
	page2 := e.expect(e.do("GET", "/v1/games/alpha/boards/main?limit=2&cursor="+cur, "", nil), 200)
	ents = page2.Body["entries"].([]any)
	if ents[0].(map[string]any)["player_id"] != ps[4].id || ents[0].(map[string]any)["rank"].(float64) != 3 || ents[1].(map[string]any)["player_id"] != ps[0].id {
		t.Fatalf("page2 %s", page2.Raw)
	}
	page3 := e.expect(e.do("GET", "/v1/games/alpha/boards/main?limit=2&cursor="+page2.Body["next_cursor"].(string), "", nil), 200)
	if len(page3.Body["entries"].([]any)) != 1 || page3.Body["next_cursor"] != nil {
		t.Fatalf("page3 %s", page3.Raw)
	}
	// Ascending board: lower is better.
	e.expect(e.do("POST", "/v1/games/alpha/boards/speed/scores", ps[0].token, map[string]int{"score": 3000}), 200)
	e.expect(e.do("POST", "/v1/games/alpha/boards/speed/scores", ps[1].token, map[string]int{"score": 2000}), 200)
	sp := e.expect(e.do("GET", "/v1/games/alpha/boards/speed", ps[0].token, nil), 200)
	if sp.Body["entries"].([]any)[0].(map[string]any)["player_id"] != ps[1].id || sp.Body["me"].(map[string]any)["rank"].(float64) != 2 {
		t.Fatalf("speed %s", sp.Raw)
	}
	// Sum board accumulates; idempotent retry does not double count.
	for i := 0; i < 2; i++ {
		e.expect(e.do("POST", "/v1/games/alpha/boards/total/scores", ps[0].token, map[string]int{"score": 5}, "Idempotency-Key", "t1"), 200)
	}
	tr := e.expect(e.do("POST", "/v1/games/alpha/boards/total/scores", ps[0].token, map[string]int{"score": 7}), 200)
	if tr.Body["best"].(float64) != 12 {
		t.Fatalf("sum %s", tr.Raw)
	}
	e.expect(e.do("GET", "/v1/games/alpha/boards/nope", "", nil), 404)
	e.expect(e.do("GET", "/v1/games/alpha/boards/main?cursor=@@", "", nil), 400)
}

func TestConfigETagAndAdmin(t *testing.T) {
	e := newEnv(t)
	r := e.expect(e.do("GET", "/v1/games/alpha/config", "", nil), 200)
	if r.Body["revision"].(float64) != 1 || r.Body["data"].(map[string]any)["welcome"] != "hi" || r.Body["data"].(map[string]any)["boards"] != nil {
		t.Fatalf("config %s", r.Raw)
	}
	e.expect(e.do("GET", "/v1/games/alpha/config", "", nil, "If-None-Match", r.Header.Get("ETag")), 304)

	ctx := context.Background()
	keys := auth.APIKeys{DB: e.a.DB}
	_, admin, _ := keys.Create(ctx, "default", "ops", []string{"admin"}, 0)
	_, other, _ := keys.Create(ctx, "tenant2", "other", []string{"admin"}, 0)
	revoked, revokedSecret, _ := keys.Create(ctx, "default", "old", []string{"admin"}, 0)
	keys.Revoke(ctx, revoked.ID)

	newDoc := `{"client":{"welcome":"v2"}}`
	e.expect(e.do("PUT", "/v1/admin/games/alpha/config", "", newDoc), 401)
	e.expect(e.do("PUT", "/v1/admin/games/alpha/config", revokedSecret, newDoc), 401)
	e.expect(e.do("PUT", "/v1/admin/games/alpha/config", other, newDoc), 404) // other tenant
	e.expect(e.do("PUT", "/v1/admin/games/alpha/config", admin, `{"boards":{"x":{"order":"sideways"}}}`), 400)
	pub := e.expect(e.do("PUT", "/v1/admin/games/alpha/config", admin, newDoc), 200)
	if pub.Body["revision"].(float64) != 2 {
		t.Fatalf("publish %s", pub.Raw)
	}
	r2 := e.expect(e.do("GET", "/v1/games/alpha/config", "", nil, "If-None-Match", r.Header.Get("ETag")), 200)
	if r2.Body["data"].(map[string]any)["welcome"] != "v2" {
		t.Fatalf("after publish %s", r2.Raw)
	}
	e.expect(e.do("POST", "/v1/admin/games/alpha/config/rollback", admin, map[string]int{"revision": 1}), 200)
	r3 := e.expect(e.do("GET", "/v1/games/alpha/config", "", nil), 200)
	if r3.Body["revision"].(float64) != 3 || r3.Body["data"].(map[string]any)["welcome"] != "hi" {
		t.Fatalf("after rollback %s", r3.Raw)
	}
	e.expect(e.do("POST", "/v1/admin/games", admin, map[string]any{"game_id": "gamma"}), 201)
	e.expect(e.do("POST", "/v1/admin/games", admin, map[string]any{"game_id": "gamma"}), 409)
	// Unconfigured game returns an empty config.
	r4 := e.expect(e.do("GET", "/v1/games/gamma/config", "", nil), 200)
	if r4.Body["revision"].(float64) != 0 {
		t.Fatalf("empty config %s", r4.Raw)
	}
}

func TestEventsBatch(t *testing.T) {
	e := newEnv(t)
	p := e.guest("alpha")
	ev := func(id string) map[string]any {
		return map[string]any{"event_id": id, "event_name": "level_end", "session_id": "s1", "client_time": 1, "client_version": "1.0.0", "platform": "web", "schema_version": 1, "properties": map[string]int{"level": 3}}
	}
	r := e.expect(e.do("POST", "/v1/games/alpha/events/batch", p.token, map[string]any{"events": []any{ev("e1"), ev("e2"), ev("e1")}}), 202)
	if r.Body["accepted"].(float64) != 3 {
		t.Fatalf("batch %s", r.Raw)
	}
	e.expect(e.do("POST", "/v1/games/alpha/events/batch", "", map[string]any{"events": []any{}}), 400)
	bad := ev("e3")
	bad["properties"] = []int{1}
	e.expect(e.do("POST", "/v1/games/alpha/events/batch", "", map[string]any{"events": []any{bad}}), 400)
	deadline := time.Now().Add(5 * time.Second)
	var n int
	for time.Now().Before(deadline) {
		e.a.Analytics.Read.QueryRow(`SELECT count(*) FROM analytics_events WHERE game_id='alpha'`).Scan(&n)
		if n == 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n != 2 {
		t.Fatalf("stored events=%d want 2 (dedupe by event_id)", n)
	}
	if inMain, err := store.TableExists(context.Background(), e.a.DB.Read, e.a.DB.Dialect, "analytics_events"); err != nil || inMain {
		t.Fatal("analytics table must not live in the main database")
	}
}

// G-A06: when the analytics buffer is saturated, events are dropped and counted
// while economy transactions keep succeeding.
func TestAnalyticsOverloadDoesNotBlockEconomy(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Game.AnalyticsBuffer = 5 })
	e.cancel() // stop the flusher so the buffer stays full
	<-e.done
	e.done = make(chan struct{})
	close(e.done)
	p := e.guest("alpha")
	events := []any{}
	for i := 0; i < 20; i++ {
		events = append(events, map[string]any{"event_id": fmt.Sprintf("x%d", i), "event_name": "tick", "session_id": "s", "client_time": 1, "client_version": "1", "platform": "web", "schema_version": 1})
	}
	r := e.expect(e.do("POST", "/v1/games/alpha/events/batch", "", map[string]any{"events": events}), 202)
	if r.Body["accepted"].(float64) != 5 || r.Body["dropped"].(float64) != 15 {
		t.Fatalf("overload %s", r.Raw)
	}
	e.expect(e.do("POST", "/v1/games/alpha/rewards/starter/claim", p.token, "{}"), 200)
}

func TestCORSAllowlist(t *testing.T) {
	e := newEnv(t)
	r := e.do("OPTIONS", "/v1/games/alpha/config", "", nil, "Origin", "https://alpha.example", "Access-Control-Request-Method", "GET")
	if r.Status != 204 || r.Header.Get("Access-Control-Allow-Origin") != "https://alpha.example" {
		t.Fatalf("allowed origin: %d %v", r.Status, r.Header)
	}
	r = e.do("GET", "/v1/games/alpha/config", "", nil, "Origin", "https://beta.example")
	if r.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("foreign origin allowed")
	}
}

func TestWeChatLoginAndBind(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("appid") != "wxapp" || q.Get("secret") != "s3cret" {
			w.Write([]byte(`{"errcode":40013,"errmsg":"invalid appid"}`))
			return
		}
		switch q.Get("js_code") {
		case "bad":
			w.Write([]byte(`{"errcode":40029,"errmsg":"invalid code"}`))
		default:
			fmt.Fprintf(w, `{"openid":"open-%s","session_key":"k"}`, q.Get("js_code"))
		}
	}))
	defer fake.Close()
	t.Setenv("WX_SECRET", "s3cret")
	e := newEnv(t, func(c *config.Config) {
		c.Game.Platforms = map[string]config.PlatformConfig{"wechat": {Endpoint: fake.URL, Apps: map[string]config.PlatformApp{"alpha": {AppID: "wxapp", AppSecretRef: "env:WX_SECRET"}}}}
	})
	r1 := e.expect(e.do("POST", "/v1/games/alpha/auth/platform", "", map[string]string{"platform": "wechat", "code": "u1"}), 201)
	r2 := e.expect(e.do("POST", "/v1/games/alpha/auth/platform", "", map[string]string{"platform": "wechat", "code": "u1"}), 200)
	if _, has := r1.Body["guest_secret"]; has {
		t.Fatalf("platform login exposes guest fields: %s", r1.Raw)
	}
	if r1.Body["player_id"] != r2.Body["player_id"] {
		t.Fatal("same openid produced two players")
	}
	e.expect(e.do("POST", "/v1/games/alpha/auth/platform", "", map[string]string{"platform": "wechat", "code": "bad"}), 401)
	e.expect(e.do("POST", "/v1/games/beta/auth/platform", "", map[string]string{"platform": "wechat", "code": "u1"}), 501)
	e.expect(e.do("POST", "/v1/games/alpha/auth/platform", "", map[string]string{"platform": "steam", "code": "u1"}), 501)

	g := e.guest("alpha")
	e.expect(e.do("POST", "/v1/games/alpha/auth/bind", g.token, map[string]string{"platform": "wechat", "code": "u2"}), 200)
	// u1 already belongs to another player: conflict, nothing overwritten.
	c := e.do("POST", "/v1/games/alpha/auth/bind", g.token, map[string]string{"platform": "wechat", "code": "u1"})
	if c.Status != 409 || c.code() != "identity_conflict" {
		t.Fatalf("bind conflict %s", c.Raw)
	}
	r3 := e.expect(e.do("POST", "/v1/games/alpha/auth/platform", "", map[string]string{"platform": "wechat", "code": "u2"}), 200)
	if r3.Body["player_id"] != g.id {
		t.Fatal("bound identity does not log into the guest player")
	}
}

func TestRateLimitAndErrorsEnvelope(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.Game.AuthRatePerMinute = 2 })
	e.expect(e.do("POST", "/v1/games/alpha/auth/guest", "", "{}"), 201)
	e.expect(e.do("POST", "/v1/games/alpha/auth/guest", "", "{}"), 201)
	r := e.do("POST", "/v1/games/alpha/auth/guest", "", "{}")
	if r.Status != 429 || r.Header.Get("Retry-After") == "" || r.Body["request_id"] == "" || r.Header.Get("X-Request-ID") == "" {
		t.Fatalf("rate limit %d %v %s", r.Status, r.Header, r.Raw)
	}
	nf := e.do("GET", "/nope", "", nil)
	if nf.Status != 404 || nf.code() != "not_found" {
		t.Fatalf("404 envelope %s", nf.Raw)
	}
	e.expect(e.do("GET", "/healthz", "", nil), 200)
	// Not marked ready because Serve() was not used in this harness.
	e.expect(e.do("GET", "/readyz", "", nil), 503)
}

// Regressions from independent review.
func TestReviewRegressions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	doc := `{"boards":{"acc":{"order":"desc","mode":"sum","period":"none","min_score":0,"max_score":100,"quarantine_above":100}},
	         "rewards":{"r":{"grants":{"coin":1},"period":"once","per_period":1}}}`
	if _, err := game.PublishConfig(ctx, e.a.DB, "alpha", []byte(doc), "t", ""); err != nil {
		t.Fatal(err)
	}
	p := e.guest("alpha")
	// Sum boards quarantine per submission, not by accumulated total.
	e.expect(e.do("POST", "/v1/games/alpha/boards/acc/scores", p.token, map[string]int{"score": 60}), 200)
	r := e.expect(e.do("POST", "/v1/games/alpha/boards/acc/scores", p.token, map[string]int{"score": 60}), 200)
	if r.Body["status"] != "accepted" || r.Body["best"].(float64) != 120 {
		t.Fatalf("sum quarantine: %s", r.Raw)
	}
	// Same Idempotency-Key on different operations is not a conflict.
	e.expect(e.do("POST", "/v1/games/alpha/rewards/r/claim", p.token, "{}", "Idempotency-Key", "k1"), 200)
	e.expect(e.do("POST", "/v1/games/alpha/boards/acc/scores", p.token, map[string]int{"score": 1}, "Idempotency-Key", "k1"), 200)
	// Saved payload bytes are returned verbatim, so the checksum verifies.
	body := `{"expected_revision":0,"payload":{ "name": "a<b&c" }}`
	e.expect(e.do("PUT", "/v1/games/alpha/saves/main", p.token, body), 200)
	g := e.expect(e.do("GET", "/v1/games/alpha/saves/main", p.token, nil), 200)
	var raw struct {
		Checksum string          `json:"checksum"`
		Payload  json.RawMessage `json:"payload"`
	}
	json.Unmarshal(g.Raw, &raw)
	if string(raw.Payload) != `{ "name": "a<b&c" }` || raw.Checksum != auth.Hash(string(raw.Payload)) {
		t.Fatalf("payload/checksum mismatch: %s", g.Raw)
	}
}
