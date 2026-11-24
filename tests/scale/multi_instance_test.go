// Package scale_test runs two application instances against one PostgreSQL
// database (profile scale) and checks that concurrent requests spread across
// both instances still have exactly one effect. It needs
// GOSTACK_TEST_POSTGRES_DSN (see adapters/pgtest); without it the tests skip.
package scale_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/pillar/gostack/adapters/pgtest"
	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/kits/game/gametest"
	"github.com/pillar/gostack/modules/jobs"
)

const doc = `{
  "items": {"coin": {"kind": "currency"}, "gem": {"kind": "currency"}},
  "rewards": {"starter": {"grants": {"coin": 50}, "period": "once", "per_period": 1}},
  "offers": {"pack": {"cost": {"coin": 10}, "grants": {"gem": 1}, "period": "once", "per_period": 1}},
  "boards": {"day": {"order": "desc", "mode": "best", "period": "daily", "min_score": 0, "max_score": 1000,
    "settlement": {"rewards": [{"from_rank": 1, "to_rank": 3, "grants": {"coin": 7}}]}}}
}`

func twoInstances(t *testing.T) (a, b *gametest.Env) {
	pgtest.RequirePostgres(t)
	a = gametest.New(t, gametest.Options{Config: doc, Mutate: func(c *config.Config) {
		c.Game.Modules = []string{"match"}
	}})
	if a.App.DB.Dialect != store.Postgres {
		t.Fatal("expected profile scale on PostgreSQL")
	}
	return a, a.Instance(t)
}

// fanOut runs n requests alternating between the instances and returns the statuses.
func fanOut(n int, envs []*gametest.Env, do func(e *gametest.Env, i int) int) []int {
	out := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() { defer wg.Done(); out[i] = do(envs[i%len(envs)], i) }()
	}
	wg.Wait()
	return out
}

func count(statuses []int, s int) int {
	n := 0
	for _, x := range statuses {
		if x == s {
			n++
		}
	}
	return n
}

func scalar(t *testing.T, e *gametest.Env, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := e.App.DB.Read.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTwoInstancesRewardAndPurchaseOnce(t *testing.T) {
	a, b := twoInstances(t)
	envs := []*gametest.Env{a, b}
	p := a.Guest("alpha")
	st := fanOut(16, envs, func(e *gametest.Env, i int) int {
		return e.Do("POST", "/v1/games/alpha/rewards/starter/claim", p.Token, "{}", "Idempotency-Key", fmt.Sprintf("r%d", i)).Status
	})
	if count(st, 200) != 1 || count(st, 409) != 15 {
		t.Fatalf("reward claim statuses %v: want one 200 and 409 for the rest", st)
	}
	st = fanOut(16, envs, func(e *gametest.Env, i int) int {
		return e.Do("POST", "/v1/games/alpha/offers/pack/purchase", p.Token, "{}", "Idempotency-Key", fmt.Sprintf("o%d", i)).Status
	})
	if count(st, 200) != 1 {
		t.Fatalf("purchase statuses %v: want exactly one 200", st)
	}
	w := b.Expect(b.Do("GET", "/v1/games/alpha/wallet", p.Token, nil), 200).Body["balances"].(map[string]any)
	if w["coin"].(float64) != 40 || w["gem"].(float64) != 1 {
		t.Fatalf("wallet %v want coin 40 gem 1", w)
	}
	if n := scalar(t, a, `SELECT count(*) FROM game_ledger WHERE game_id='alpha' AND player_id=?`, p.ID); n != 3 {
		t.Fatalf("ledger rows %d want 3 (reward, cost, grant)", n)
	}
	// The same Idempotency-Key replays across instances.
	r1 := a.Expect(a.Do("POST", "/v1/games/alpha/boards/day/scores", p.Token, map[string]int{"score": 5}, "Idempotency-Key", "s1"), 200)
	r2 := b.Expect(b.Do("POST", "/v1/games/alpha/boards/day/scores", p.Token, map[string]int{"score": 5}, "Idempotency-Key", "s1"), 200)
	if r2.Header.Get("Idempotent-Replayed") != "true" || string(r1.Raw) != string(r2.Raw) {
		t.Fatalf("cross-instance replay: %q / %s vs %s", r2.Header.Get("Idempotent-Replayed"), r1.Raw, r2.Raw)
	}
}

func TestTwoInstancesSettleSeasonOnce(t *testing.T) {
	a, b := twoInstances(t)
	const season = "2026-01-01"
	for i := range 5 {
		p := a.Guest("alpha")
		if _, err := a.App.DB.Write.Exec(`INSERT INTO scores(game_id,board_id,season_id,player_id,score,achieved_at,status,updated_at) VALUES('alpha','day',?,?,?,?,'accepted',?)`,
			season, p.ID, 100+i, i, i); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := []*gametest.Env{a, b}[i%2]
			_, errs[i] = e.App.Game.SettleSeason(context.Background(), "alpha", "day", season)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil && !errors.Is(err, store.ErrOverloaded) {
			t.Fatal(err)
		}
	}
	// A pass that gave up under contention is resumable; finish from either instance.
	res, err := b.App.Game.SettleSeason(context.Background(), "alpha", "day", season)
	if err != nil || res.Status != "completed" || res.PaidPlayers != 3 {
		t.Fatalf("settlement %+v %v", res, err)
	}
	if n := scalar(t, a, `SELECT count(*) FROM season_payouts WHERE game_id='alpha'`); n != 3 {
		t.Fatalf("payouts %d want 3", n)
	}
	if n := scalar(t, a, `SELECT count(*) FROM game_ledger WHERE game_id='alpha' AND source_key LIKE 'season:%'`); n != 3 {
		t.Fatalf("settlement ledger rows %d want 3", n)
	}
}

func TestTwoInstancesMatchTicketsOnce(t *testing.T) {
	a, b := twoInstances(t)
	a.Expect(a.Do("PUT", "/v1/admin/games/alpha/match/queues/duel", a.AdminToken,
		`{"size":2,"rating_window":1000,"widen_per_sec":10,"ttl":600}`), 200)
	const players = 20
	for i := range players {
		p := a.Guest("alpha")
		e := []*gametest.Env{a, b}[i%2]
		e.Expect(e.Do("POST", "/v1/games/alpha/match/tickets", p.Token, map[string]any{"queue": "duel", "attributes": map[string]int{"rating": 1000 + i}}), 201)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := []*gametest.Env{a, b}[i%2]
			for range 5 {
				if _, err := e.App.Mod.Match.MatchOnce(context.Background()); err != nil && !errors.Is(err, store.ErrOverloaded) {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if n := scalar(t, a, `SELECT count(*) FROM match_tickets WHERE status='matched'`); n != players {
		t.Fatalf("matched tickets %d want %d", n, players)
	}
	if n := scalar(t, a, `SELECT count(*) FROM match_rooms`); n != players/2 {
		t.Fatalf("rooms %d want %d", n, players/2)
	}
	if n := scalar(t, a, `SELECT count(DISTINCT ticket_id) FROM match_room_members`); n != players {
		t.Fatalf("room members %d want %d (each ticket in exactly one room)", n, players)
	}
	if n := scalar(t, a, `SELECT count(*) FROM (SELECT room_id FROM match_room_members GROUP BY room_id HAVING count(*) <> 2) x`); n != 0 {
		t.Fatalf("%d rooms without exactly 2 members", n)
	}
}

func TestTwoInstancesClaimJobsOnce(t *testing.T) {
	a, b := twoInstances(t)
	ctx := context.Background()
	const total = 120
	for i := range total {
		if _, err := a.App.Jobs.EnqueueNow(ctx, jobs.Job{Type: "t", Payload: []byte(fmt.Sprint(i))}); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var contention int
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := []*gametest.Env{a, b}[i%2].App.Jobs
			for {
				c, err := q.Claim(ctx, nil, 60e9)
				if errors.Is(err, store.ErrOverloaded) {
					mu.Lock()
					contention++
					mu.Unlock()
					continue
				}
				if err != nil {
					t.Error(err)
					return
				}
				if c == nil {
					return
				}
				mu.Lock()
				seen[c.ID]++
				mu.Unlock()
				if err := q.Ack(ctx, c); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if len(seen) != total {
		t.Fatalf("claimed %d distinct jobs, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("job %s claimed %d times", id, n)
		}
	}
	if n := scalar(t, a, `SELECT count(*) FROM jobs WHERE status='succeeded'`); n != total {
		t.Fatalf("succeeded %d want %d", n, total)
	}
	t.Logf("claims that exhausted serialization retries (retried by the caller): %d", contention)
}
