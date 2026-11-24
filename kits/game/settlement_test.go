package game_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/kits/game/gametest"
	"github.com/pillar/gostack/modules/jobs"
)

const settleDoc = `{
  "items": {"coin": {"kind": "currency"}, "trophy": {"kind": "item", "max_stack": 1}},
  "boards": {
    "day": {"order": "desc", "mode": "best", "period": "daily", "min_score": 0, "max_score": 1000, "quarantine_above": 900,
      "settlement": {"rewards": [
        {"from_rank": 1, "to_rank": 1, "grants": {"coin": 100, "trophy": 1}},
        {"from_rank": 2, "to_rank": 3, "grants": {"coin": 50}}]}},
    "forever": {"order": "desc", "mode": "best", "period": "none", "min_score": 0, "max_score": 10}
  }
}`

func insertScore(t *testing.T, e *gametest.Env, gameID, board, season, player string, score, at int64) {
	t.Helper()
	if _, err := e.App.DB.Write.Exec(`INSERT INTO scores(game_id,board_id,season_id,player_id,score,achieved_at,status,updated_at) VALUES(?,?,?,?,?,?,'accepted',?)`,
		gameID, board, season, player, score, at, at); err != nil {
		t.Fatal(err)
	}
}

func balance(t *testing.T, e *gametest.Env, p gametest.Player, asset string) int64 {
	t.Helper()
	r := e.Expect(e.Do("GET", "/v1/games/alpha/wallet", p.Token, nil), 200)
	v, _ := r.Body["balances"].(map[string]any)[asset].(float64)
	return int64(v)
}

func ledgerRows(t *testing.T, e *gametest.Env, kind string) int {
	t.Helper()
	var n int
	if err := e.App.DB.Read.QueryRow(`SELECT count(*) FROM game_ledger WHERE kind=?`, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSeasonSettlement(t *testing.T) {
	e := gametest.New(t, gametest.Options{Config: settleDoc})
	const season = "2026-01-01"
	p1, p2, p3, p4 := e.Guest("alpha"), e.Guest("alpha"), e.Guest("alpha"), e.Guest("alpha")
	b1 := e.Guest("beta")
	insertScore(t, e, "alpha", "day", season, p1.ID, 500, 1)
	insertScore(t, e, "alpha", "day", season, p2.ID, 700, 2)
	insertScore(t, e, "alpha", "day", season, p3.ID, 700, 3) // tie: later achiever ranks after p2
	insertScore(t, e, "alpha", "day", season, p4.ID, 100, 4)
	insertScore(t, e, "beta", "day", season, b1.ID, 999, 1)
	// A quarantined submission of p4 is never rewarded.
	if _, err := e.App.DB.Write.Exec(`INSERT INTO score_quarantine(game_id,board_id,season_id,player_id,score,reason,created_at) VALUES('alpha','day',?,?,950,'test',5)`, season, p4.ID); err != nil {
		t.Fatal(err)
	}
	// p2 already holds a trophy (max_stack 1): the settlement clamps instead of failing.
	e.Expect(e.Do("POST", "/v1/admin/games/alpha/players/"+p2.ID+"/adjust", e.AdminToken,
		map[string]any{"deltas": map[string]int{"trophy": 1}, "reason": "event prize", "source_key": "pre-1"}), 200)

	base := "/v1/admin/games/alpha/boards/day/seasons/"
	// Errors.
	e.Expect(e.Do("POST", base+game.SeasonID("daily", time.Now())+"/settle", e.AdminToken, nil), 409)
	if r := e.Do("POST", base+"2026-13-01/settle", e.AdminToken, nil); r.Status != 400 {
		t.Fatalf("invalid season %d", r.Status)
	}
	if r := e.Do("POST", "/v1/admin/games/alpha/boards/forever/seasons/all/settle", e.AdminToken, nil); r.Code() != "settlement_not_configured" {
		t.Fatalf("none board %s", r.Raw)
	}
	e.Expect(e.Do("POST", "/v1/admin/games/alpha/boards/nope/seasons/"+season+"/settle", e.AdminToken, nil), 404)
	e.Expect(e.Do("POST", base+season+"/settle", p1.Token, nil), 401)
	e.Expect(e.Do("GET", base+season+"/settlement", e.AdminToken, nil), 404)

	// Concurrent settles pay each player exactly once.
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := e.Do("POST", base+season+"/settle", e.AdminToken, nil); r.Status != 200 || r.Body["status"] != "completed" {
				t.Errorf("settle %d %s", r.Status, r.Raw)
			}
		}()
	}
	wg.Wait()
	r := e.Expect(e.Do("POST", base+season+"/settle", e.AdminToken, nil), 200)
	if r.Body["paid_players"].(float64) != 3 || r.Body["status"] != "completed" {
		t.Fatalf("result %s", r.Raw)
	}
	if balance(t, e, p2, "coin") != 100 || balance(t, e, p2, "trophy") != 1 || balance(t, e, p3, "coin") != 50 ||
		balance(t, e, p1, "coin") != 50 || balance(t, e, p4, "coin") != 0 {
		t.Fatal("unexpected balances after settlement")
	}
	if n := ledgerRows(t, e, "season"); n != 3 { // p2 coin (trophy clamped), p3 coin, p1 coin
		t.Fatalf("season ledger rows %d", n)
	}
	g := e.Expect(e.Do("GET", base+season+"/settlement", e.AdminToken, nil), 200)
	ps := g.Body["payouts"].([]any)
	if len(ps) != 3 || ps[0].(map[string]any)["player_id"] != p2.ID || ps[1].(map[string]any)["player_id"] != p3.ID {
		t.Fatalf("payouts %s", g.Raw)
	}
	// Approving a quarantined score after settlement is refused.
	var qid int64
	e.App.DB.Read.QueryRow(`SELECT id FROM score_quarantine WHERE player_id=?`, p4.ID).Scan(&qid)
	if r := e.Do("POST", "/v1/admin/games/alpha/quarantine/"+itoa(qid)+"/approve", e.AdminToken, nil); r.Code() != "season_settled" {
		t.Fatalf("approve after settle %s", r.Raw)
	}
	// Other game untouched.
	var n int
	e.App.DB.Read.QueryRow(`SELECT count(*) FROM game_ledger WHERE game_id='beta'`).Scan(&n)
	if n != 0 {
		t.Fatal("beta paid by alpha settlement")
	}
	if ms, _ := game.Reconcile(context.Background(), e.App.DB.Read, ""); len(ms) != 0 {
		t.Fatalf("reconcile %v", ms)
	}
}

func TestSeasonSettlementResumesAfterPartialRun(t *testing.T) {
	e := gametest.New(t, gametest.Options{Config: settleDoc})
	const season = "2026-01-02"
	p1, p2 := e.Guest("alpha"), e.Guest("alpha")
	insertScore(t, e, "alpha", "day", season, p1.ID, 900, 1)
	insertScore(t, e, "alpha", "day", season, p2.ID, 800, 2)
	// Simulate a crash after p1 was paid: running settlement row + payout + ledger.
	ctx := context.Background()
	err := e.App.DB.WriteTx(ctx, func(tx *sqlTx) error {
		if _, err := tx.Exec(`INSERT INTO season_settlements(game_id,board_id,season_id,status,config_revision,created_at,updated_at) VALUES('alpha','day',?,'running',1,1,1)`, season); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO season_payouts(game_id,board_id,season_id,player_id,rank,score,grants,created_at) VALUES('alpha','day',?,?,1,900,'{"coin":100,"trophy":1}',1)`, season, p1.ID); err != nil {
			return err
		}
		return game.ApplyLedger(ctx, tx, "alpha", p1.ID, "season", "season:day:"+season+":"+p1.ID, map[string]int64{"coin": 100, "trophy": 1}, 1)
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.App.Game.SettleSeason(ctx, "alpha", "day", season)
	if err != nil || res.Status != "completed" || res.PaidPlayers != 2 {
		t.Fatalf("resume %+v %v", res, err)
	}
	if balance(t, e, p1, "coin") != 100 || balance(t, e, p1, "trophy") != 1 || balance(t, e, p2, "coin") != 50 {
		t.Fatal("resume paid twice or skipped")
	}
}

func TestScheduleSettlementsJob(t *testing.T) {
	now := time.Now().UTC()
	if now.Sub(now.Truncate(24*time.Hour)) < 3*time.Minute {
		t.Skip("too close to the UTC day boundary")
	}
	e := gametest.New(t, gametest.Options{Config: settleDoc})
	season := game.SeasonID("daily", now.Add(-24*time.Hour))
	p := e.Guest("alpha")
	insertScore(t, e, "alpha", "day", season, p.ID, 10, 1)
	ctx := context.Background()
	q := &jobs.Queue{DB: e.App.DB}
	n, err := e.App.Game.ScheduleSettlements(ctx, q)
	if err != nil || n != 2 { // alpha and beta both have the board
		t.Fatalf("scheduled %d %v", n, err)
	}
	if n, err = e.App.Game.ScheduleSettlements(ctx, q); err != nil || n != 0 {
		t.Fatalf("rescheduled %d %v", n, err)
	}
	handlers := e.App.Game.JobHandlers()
	for i := 0; i < 2; i++ {
		c, err := q.Claim(ctx, []string{game.JobSettleSeason}, time.Minute)
		if err != nil || c == nil {
			t.Fatalf("claim %v %v", c, err)
		}
		if err := handlers[c.Type](ctx, c); err != nil {
			t.Fatal(err)
		}
		if err := q.Ack(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if balance(t, e, p, "coin") != 100 {
		t.Fatal("job did not pay")
	}
	// A completed season is not scheduled again even after the job rows are purged.
	e.App.DB.Write.Exec(`DELETE FROM jobs`)
	if n, err = e.App.Game.ScheduleSettlements(ctx, q); err != nil || n != 0 {
		t.Fatalf("after completion %d %v", n, err)
	}
}
