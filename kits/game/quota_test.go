package game_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pillar/gostack/adapters/pgtest"
	"github.com/pillar/gostack/adapters/valkeytest"
	"github.com/pillar/gostack/app"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/kits/game/gametest"
)

// G-A09: per-game resource quotas. In every test game "alpha" hits a quota
// while game "beta" on the same deployment keeps working.

func setQuotas(e *gametest.Env, gameID string, body string) gametest.Resp {
	e.T.Helper()
	return e.Expect(e.Do("PUT", "/v1/admin/games/"+gameID+"/quotas", e.AdminToken, body), 200)
}

func quotaDetails(t *testing.T, r gametest.Resp, code string) map[string]any {
	t.Helper()
	if r.Code() != code {
		t.Fatalf("want %s: %d %s", code, r.Status, r.Raw)
	}
	return r.Body["error"].(map[string]any)["details"].(map[string]any)
}

// saveBody builds a save PUT whose payload is a JSON string of exactly n bytes.
func saveBody(expected int64, n int) map[string]any {
	return map[string]any{"expected_revision": expected, "schema_version": 1, "payload": strings.Repeat("x", n-2)}
}

func saveIn(e *gametest.Env, gameID, slot string, p gametest.Player, expected int64, n int) gametest.Resp {
	return e.Do("PUT", "/v1/games/"+gameID+"/saves/"+slot, p.Token, saveBody(expected, n))
}

// usage returns game_usage.save_bytes and the actual sum of current payloads.
func saveUsage(t *testing.T, e *gametest.Env, gameID string) (counter, actual int64) {
	t.Helper()
	ctx := context.Background()
	if err := e.App.DB.Read.QueryRowContext(ctx, `SELECT save_bytes FROM game_usage WHERE game_id=?`, gameID).Scan(&counter); err != nil {
		t.Fatal(err)
	}
	if err := e.App.DB.Read.QueryRowContext(ctx, `SELECT coalesce(sum(length(payload)),0) FROM save_slots WHERE game_id=?`, gameID).Scan(&actual); err != nil {
		t.Fatal(err)
	}
	return counter, actual
}

func TestQuotaAdminAPI(t *testing.T) {
	e := gametest.New(t, gametest.Options{Mutate: func(c *config.Config) { c.Game.QuotaDefaults.MaxPlayers = 1000 }})
	r := e.Expect(e.Do("GET", "/v1/admin/games/alpha/quotas", e.AdminToken, nil), 200)
	if l := r.Body["limits"].(map[string]any); l["max_players"].(float64) != 1000 || l["save_bytes_total"].(float64) != 0 ||
		r.Body["overrides"].(map[string]any)["max_players"] != nil {
		t.Fatalf("defaults %s", r.Raw)
	}
	r = setQuotas(e, "alpha", `{"max_players": 5, "save_bytes_total": 0}`)
	if l := r.Body["limits"].(map[string]any); l["max_players"].(float64) != 5 || l["save_bytes_total"].(float64) != 0 {
		t.Fatalf("override %s", r.Raw)
	}
	// Overrides are replaced as a whole: omitted fields inherit the default again.
	r = setQuotas(e, "alpha", `{"requests_per_minute": 100}`)
	if l := r.Body["limits"].(map[string]any); l["max_players"].(float64) != 1000 || l["requests_per_minute"].(float64) != 100 {
		t.Fatalf("replace %s", r.Raw)
	}
	for _, bad := range []string{`{"max_players": -1}`, `{"nope": 1}`, `{"requests_per_minute": 2000000000}`, `[]`} {
		e.Expect(e.Do("PUT", "/v1/admin/games/alpha/quotas", e.AdminToken, bad), 400)
	}
	p := e.Guest("alpha")
	e.Expect(e.Do("GET", "/v1/admin/games/alpha/quotas", p.Token, nil), 401)
	e.Expect(e.Do("GET", "/v1/admin/games/nope/quotas", e.AdminToken, nil), 404)
	// beta is untouched.
	if r := e.Expect(e.Do("GET", "/v1/admin/games/beta/quotas", e.AdminToken, nil), 200); r.Body["limits"].(map[string]any)["requests_per_minute"].(float64) != 0 {
		t.Fatalf("beta %s", r.Raw)
	}
}

func TestQuotaConfigValidation(t *testing.T) {
	c := config.Defaults()
	c.Kits = []string{"game"}
	c.Game.QuotaDefaults.SaveBytesTotal = -1
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "quota_defaults") {
		t.Fatalf("negative default accepted: %v", err)
	}
}

func TestQuotaMaxPlayers(t *testing.T) {
	e := gametest.New(t, gametest.Options{})
	setQuotas(e, "alpha", `{"max_players": 2}`)
	e.Guest("alpha")
	e.Guest("alpha")
	r := e.Do("POST", "/v1/games/alpha/auth/guest", "", "{}")
	if r.Status != 403 {
		t.Fatalf("third player: %d %s", r.Status, r.Raw)
	}
	if d := quotaDetails(t, r, "quota_exceeded"); d["quota"] != "max_players" || d["limit"].(float64) != 2 || d["used"].(float64) != 2 {
		t.Fatalf("details %v", d)
	}
	for range 3 {
		e.Guest("beta") // other game unaffected
	}
	// Raising the quota applies immediately on this instance.
	setQuotas(e, "alpha", `{"max_players": 3}`)
	e.Guest("alpha")
	e.Expect(e.Do("POST", "/v1/games/alpha/auth/guest", "", "{}"), 403)
	r = e.Expect(e.Do("GET", "/v1/admin/games/alpha/quotas", e.AdminToken, nil), 200)
	if r.Body["usage"].(map[string]any)["players"].(float64) != 3 {
		t.Fatalf("usage %s", r.Raw)
	}
	var m strings.Builder
	e.App.Metrics.Render(&m)
	for _, want := range []string{`game_quota_limit{game="alpha",quota="max_players"} 3`, `game_quota_limit{game="beta",quota="max_players"} 0`,
		`game_quota_usage{game="alpha",quota="write_concurrency"} 0`} {
		if !strings.Contains(m.String(), want) {
			t.Fatalf("metrics lack %q:\n%s", want, m.String())
		}
	}
	// Existing players still log in.
	setQuotas(e, "alpha", `{"max_players": 0}`)
	e.Guest("alpha")
}

func TestQuotaRequestsPerMinute(t *testing.T) {
	e := gametest.New(t, gametest.Options{})
	setQuotas(e, "alpha", `{"requests_per_minute": 5}`)
	for range 5 {
		e.Expect(e.Do("GET", "/v1/games/alpha/config", "", nil), 200)
	}
	r := e.Do("GET", "/v1/games/alpha/config", "", nil)
	if d := quotaDetails(t, r, "rate_limited"); r.Status != 429 || d["quota"] != "requests_per_minute" || r.Header.Get("Retry-After") == "" {
		t.Fatalf("429 %d %s %v", r.Status, r.Raw, r.Header)
	}
	e.Expect(e.Do("POST", "/v1/games/alpha/auth/guest", "", "{}"), 429) // every route of the game
	for range 20 {
		e.Expect(e.Do("GET", "/v1/games/beta/config", "", nil), 200)
	}
	// Admin routes are not counted; lifting the quota applies at once.
	setQuotas(e, "alpha", `{"requests_per_minute": 0}`)
	e.Expect(e.Do("GET", "/v1/games/alpha/config", "", nil), 200)
	// Raising credits the increase immediately.
	setQuotas(e, "alpha", `{"requests_per_minute": 50}`)
	e.Expect(e.Do("GET", "/v1/games/alpha/config", "", nil), 200)
}

// With rate_limit.driver valkey the per-game request quota is shared by all
// instances; save and player quotas are enforced by the shared database.
func TestQuotaAcrossInstances(t *testing.T) {
	if !pgtest.Enabled() || !valkeytest.Enabled() {
		t.Skip("needs GOSTACK_TEST_POSTGRES_DSN and GOSTACK_TEST_VALKEY_ADDR")
	}
	e := gametest.New(t, gametest.Options{})
	e2 := e.Instance(t)
	setQuotas(e, "alpha", `{"requests_per_minute": 10, "max_players": 3}`)
	ok := 0
	for i := range 20 {
		env := e
		if i%2 == 1 {
			env = e2
		}
		if r := env.Do("GET", "/v1/games/alpha/config", "", nil); r.Status == 200 {
			ok++
		}
	}
	if ok != 10 {
		t.Fatalf("allowed %d of 20 across two instances, want 10", ok)
	}
	setQuotas(e, "alpha", `{"max_players": 3}`)
	time.Sleep(5100 * time.Millisecond) // the second instance's cached limits expire (quotaTTL)
	e.Guest("alpha")
	e2.Guest("alpha")
	e.Guest("alpha")
	e2.Expect(e2.Do("POST", "/v1/games/alpha/auth/guest", "", "{}"), 403)
	e2.Guest("beta")
}

func TestQuotaSaveBytes(t *testing.T) {
	e := gametest.New(t, gametest.Options{})
	p, q := e.Guest("alpha"), e.Guest("alpha")
	b := e.Guest("beta")
	setQuotas(e, "alpha", `{"save_bytes_total": 1000}`)
	e.Expect(saveIn(e, "alpha", "main", p, 0, 600), 200)
	r := saveIn(e, "alpha", "main", q, 0, 500)
	if r.Status != 403 {
		t.Fatalf("over quota: %d %s", r.Status, r.Raw)
	}
	if d := quotaDetails(t, r, "quota_exceeded"); d["quota"] != "save_bytes_total" || d["used"].(float64) != 600 || d["requested"].(float64) != 500 {
		t.Fatalf("details %v", d)
	}
	e.Expect(saveIn(e, "alpha", "main", q, 0, 400), 200) // exactly at the limit
	e.Expect(saveIn(e, "alpha", "main", p, 1, 601), 403) // growing by 1 byte
	e.Expect(saveIn(e, "alpha", "main", p, 1, 100), 200) // shrinking always works
	if c, a := saveUsage(t, e, "alpha"); c != 500 || a != 500 {
		t.Fatalf("after shrink counter=%d actual=%d", c, a)
	}
	// beta has no quota and its own counter.
	e.Expect(saveIn(e, "beta", "main", b, 0, 5000), 200)
	if c, a := saveUsage(t, e, "beta"); c != 5000 || a != 5000 {
		t.Fatalf("beta counter=%d actual=%d", c, a)
	}
	// Restoring revision 1 (600 bytes) lands exactly on the limit: 400 + 600.
	r = e.Do("POST", "/v1/games/alpha/saves/main/restore", p.Token, map[string]any{"revision": 1, "expected_revision": 2})
	e.Expect(r, 200)
	if c, a := saveUsage(t, e, "alpha"); c != 1000 || a != 1000 {
		t.Fatalf("after restore counter=%d actual=%d", c, a)
	}
	e.Expect(e.Do("POST", "/v1/games/alpha/saves/main/restore", p.Token, map[string]any{"revision": 2, "expected_revision": 3}), 200) // back to 100
	setQuotas(e, "alpha", `{"save_bytes_total": 600}`)
	r = e.Do("POST", "/v1/games/alpha/saves/main/restore", p.Token, map[string]any{"revision": 1, "expected_revision": 4})
	if quotaDetails(t, r, "quota_exceeded")["quota"] != "save_bytes_total" {
		t.Fatalf("restore over quota %s", r.Raw)
	}
	// Delete frees the bytes.
	e.Expect(e.Do("DELETE", "/v1/games/alpha/saves/main?expected_revision=1", q.Token, nil), 200)
	if c, a := saveUsage(t, e, "alpha"); c != 100 || a != 100 {
		t.Fatalf("after delete counter=%d actual=%d", c, a)
	}
	e.Expect(saveIn(e, "alpha", "main", q, 0, 500), 200)
	r = e.Expect(e.Do("GET", "/v1/admin/games/alpha/quotas", e.AdminToken, nil), 200)
	if r.Body["usage"].(map[string]any)["save_bytes"].(float64) != 600 {
		t.Fatalf("usage %s", r.Raw)
	}
	// A missing counter row (game created outside CreateGame) is rebuilt by recount.
	if _, err := e.App.DB.Write.Exec(`DELETE FROM game_usage WHERE game_id='alpha'`); err != nil {
		t.Fatal(err)
	}
	e.Expect(saveIn(e, "alpha", "slot2", q, 0, 10), 403)
	setQuotas(e, "alpha", `{"save_bytes_total": 0}`)
	e.Expect(saveIn(e, "alpha", "slot2", q, 0, 10), 200)
	if c, a := saveUsage(t, e, "alpha"); c != 610 || a != 610 {
		t.Fatalf("rebuilt counter=%d actual=%d", c, a)
	}
	u, err := game.RecountUsage(context.Background(), e.App.DB, "alpha")
	if err != nil || u.SaveBytes != 610 || u.Players != 2 {
		t.Fatalf("recount %+v %v", u, err)
	}
}

func TestQuotaAnalyticsEventsPerDay(t *testing.T) {
	e := gametest.New(t, gametest.Options{})
	setQuotas(e, "alpha", `{"analytics_events_per_day": 3}`)
	batch := func(gameID, prefix string, n int) gametest.Resp {
		evs := make([]map[string]any, n)
		for i := range evs {
			evs[i] = map[string]any{"event_id": fmt.Sprintf("%s-%d", prefix, i), "event_name": "e", "session_id": "s"}
		}
		return e.Expect(e.Do("POST", "/v1/games/"+gameID+"/events/batch", "", map[string]any{"events": evs}), 202)
	}
	if r := batch("alpha", "a", 5); r.Body["accepted"].(float64) != 3 || r.Body["dropped"].(float64) != 2 {
		t.Fatalf("alpha %s", r.Raw)
	}
	if r := batch("alpha", "b", 1); r.Body["accepted"].(float64) != 0 || r.Body["dropped"].(float64) != 1 {
		t.Fatalf("alpha again %s", r.Raw)
	}
	if r := batch("beta", "a", 5); r.Body["accepted"].(float64) != 5 {
		t.Fatalf("beta %s", r.Raw)
	}
	r := e.Expect(e.Do("GET", "/v1/admin/games/alpha/quotas", e.AdminToken, nil), 200)
	if r.Body["usage"].(map[string]any)["analytics_events_today"].(float64) != 3 {
		t.Fatalf("usage %s", r.Raw)
	}
	setQuotas(e, "alpha", `{"analytics_events_per_day": 4}`)
	if r := batch("alpha", "c", 2); r.Body["accepted"].(float64) != 1 {
		t.Fatalf("raised %s", r.Raw)
	}
}

// A restarted instance seeds today's count from the analytics store, so a
// restart does not grant a fresh daily allowance.
func TestQuotaAnalyticsSeededAfterRestart(t *testing.T) {
	e := gametest.New(t, gametest.Options{SQLite: true})
	setQuotas(e, "alpha", `{"analytics_events_per_day": 3}`)
	evs := []map[string]any{{"event_id": "x1", "event_name": "e", "session_id": "s"}, {"event_id": "x2", "event_name": "e", "session_id": "s"}}
	e.Expect(e.Do("POST", "/v1/games/alpha/events/batch", "", map[string]any{"events": evs}), 202)
	e.Stop() // flushes analytics
	e2 := gametest.Attach(t, e.Cfg, gametest.Options{}, e.AdminToken)
	evs = []map[string]any{{"event_id": "y1", "event_name": "e", "session_id": "s"}, {"event_id": "y2", "event_name": "e", "session_id": "s"}}
	r := e2.Expect(e2.Do("POST", "/v1/games/alpha/events/batch", "", map[string]any{"events": evs}), 202)
	if r.Body["accepted"].(float64) != 1 || r.Body["dropped"].(float64) != 1 {
		t.Fatalf("after restart %s", r.Raw)
	}
}

func TestQuotaWriteConcurrency(t *testing.T) {
	entered, release := make(chan struct{}, 4), make(chan struct{})
	e := gametest.New(t, gametest.Options{Routes: func(mux *http.ServeMux, a *app.App) {
		a.Game.Handle(mux, "POST /ext/slow", game.Public, func(w http.ResponseWriter, r *http.Request) error {
			entered <- struct{}{}
			<-release
			httpx.WriteJSON(w, 200, map[string]bool{"ok": true})
			return nil
		})
	}})
	setQuotas(e, "alpha", `{"write_concurrency": 1}`)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.Do("POST", "/v1/games/alpha/ext/slow", "", "{}")
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("slow request did not start")
	}
	r := e.Do("POST", "/v1/games/alpha/auth/guest", "", "{}")
	if r.Status != 503 || quotaDetails(t, r, "overloaded")["quota"] != "write_concurrency" {
		t.Fatalf("second write %d %s", r.Status, r.Raw)
	}
	e.Expect(e.Do("GET", "/v1/games/alpha/config", "", nil), 200) // reads are not capped
	e.Guest("beta")                                               // other game unaffected
	r = e.Expect(e.Do("GET", "/v1/admin/games/alpha/quotas", e.AdminToken, nil), 200)
	if r.Body["usage"].(map[string]any)["writes_in_flight"].(float64) != 1 {
		t.Fatalf("in flight %s", r.Raw)
	}
	close(release)
	wg.Wait()
	e.Guest("alpha") // slot released
}

// Concurrent save puts, restores and deletes keep game_usage.save_bytes equal
// to the sum of current payloads, and never let it exceed the quota (SQLite
// single writer; PostgreSQL SERIALIZABLE with retries).
func TestQuotaSaveCountersConcurrent(t *testing.T) {
	e := gametest.New(t, gametest.Options{})
	const players, rounds = 8, 6
	ps := make([]gametest.Player, players)
	for i := range ps {
		ps[i] = e.Guest("alpha")
	}
	b := e.Guest("beta")
	const limit = 8000
	setQuotas(e, "alpha", fmt.Sprintf(`{"save_bytes_total": %d}`, limit))
	var wg sync.WaitGroup
	var mu sync.Mutex
	statuses := map[int]int{}
	for i, p := range ps {
		for _, slot := range []string{"a", "b"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var rev int64
				for j := range rounds {
					n := 100 + (i*137+j*311)%900
					r := saveIn(e, "alpha", slot, p, rev, n)
					mu.Lock()
					statuses[r.Status]++
					mu.Unlock()
					if r.Status == 200 {
						rev = int64(r.Body["revision"].(float64))
					}
					if j == rounds/2 && rev > 1 {
						if rr := e.Do("POST", "/v1/games/alpha/saves/"+slot+"/restore", p.Token, map[string]any{"revision": rev - 1, "expected_revision": rev}); rr.Status == 200 {
							rev = int64(rr.Body["revision"].(float64))
						}
					}
					if j == rounds-2 && rev > 0 && slot == "b" {
						if rr := e.Do("DELETE", fmt.Sprintf("/v1/games/alpha/saves/b?expected_revision=%d", rev), p.Token, nil); rr.Status == 200 {
							rev = 0
						}
					}
				}
			}()
		}
	}
	wg.Add(1)
	go func() { // beta writes concurrently without a quota
		defer wg.Done()
		var rev int64
		for range rounds {
			r := saveIn(e, "beta", "main", b, rev, 20000)
			if r.Status != 200 {
				t.Errorf("beta save %d %s", r.Status, r.Raw)
				return
			}
			rev = int64(r.Body["revision"].(float64))
		}
	}()
	wg.Wait()
	if statuses[200] == 0 || statuses[403] == 0 {
		t.Fatalf("expected both accepted and quota-rejected saves: %v", statuses)
	}
	for s := range statuses {
		if s != 200 && s != 403 && s != 503 { // 503: write contention retries exhausted (PostgreSQL)
			t.Fatalf("unexpected status %d: %v", s, statuses)
		}
	}
	c, a := saveUsage(t, e, "alpha")
	if c != a || c > limit {
		t.Fatalf("alpha counter=%d actual=%d limit=%d statuses=%v", c, a, limit, statuses)
	}
	if c, a := saveUsage(t, e, "beta"); c != a || c != 20000 {
		t.Fatalf("beta counter=%d actual=%d", c, a)
	}
	t.Logf("statuses %v, alpha bytes %d", statuses, c)
}

func TestQuotaCLI(t *testing.T) {
	e := gametest.New(t, gametest.Options{SQLite: true})
	setQuotas(e, "alpha", `{"max_players": 7}`)
	p := e.Guest("alpha")
	e.Expect(saveIn(e, "alpha", "main", p, 0, 300), 200)
	e.Stop() // the CLI opens the database itself
	cli := func(args ...string) (int, string, string) {
		var out, errb strings.Builder
		env := app.Env{Stdout: &out, Stderr: &errb, Getenv: func(string) (string, bool) { return "", false }}
		base := []string{"--set", "database.path=" + e.Cfg.Database.Path, "--set", "database.analytics_path=" + e.Cfg.Database.AnalyticsPath}
		code := app.Main(append(base, args...), app.Hooks{}, env)
		return code, out.String(), errb.String()
	}
	code, out, errs := cli("game", "quotas", "alpha")
	if code != 0 || !strings.Contains(out, "max_players               7 (used 1)") || !strings.Contains(out, "save_bytes_total          0 (used 300)") {
		t.Fatalf("quotas: %d %q %q", code, out, errs)
	}
	code, out, errs = cli("--json", "game", "quotas", "alpha", "--recount")
	if code != 0 || !strings.Contains(out, `"save_bytes": 300`) {
		t.Fatalf("recount: %d %q %q", code, out, errs)
	}
	if code, _, _ := cli("game", "quotas"); code == 0 {
		t.Fatal("missing id accepted")
	}
	if code, _, _ := cli("game", "quotas", "nope"); code == 0 {
		t.Fatal("unknown game accepted")
	}
}
