package game_test

import (
	"sync"
	"testing"
	"time"

	"github.com/pillar/gostack/kits/game/gametest"
)

const antiCheatDoc = `{
  "boards": {
    "ranked": {"order": "desc", "mode": "best", "period": "none", "min_score": 0, "max_score": 1000,
      "rule_version": 3, "require_run": true, "min_run_ms": 200, "run_ttl_ms": 1500},
    "sumb": {"order": "desc", "mode": "sum", "period": "none", "min_score": 0, "max_score": 1000, "quarantine_above": 100}
  }
}`

func TestRunTokensAndRuleVersion(t *testing.T) {
	e := gametest.New(t, gametest.Options{Config: antiCheatDoc})
	p, other := e.Guest("alpha"), e.Guest("alpha")
	run := func(pl gametest.Player, board string) string {
		r := e.Expect(e.Do("POST", "/v1/games/alpha/boards/"+board+"/runs", pl.Token, nil), 201)
		return r.Body["run_token"].(string)
	}
	submit := func(pl gametest.Player, body map[string]any, hdr ...string) gametest.Resp {
		return e.Do("POST", "/v1/games/alpha/boards/ranked/scores", pl.Token, body, hdr...)
	}
	if r := submit(p, map[string]any{"score": 10}); r.Code() != "rule_version_mismatch" {
		t.Fatalf("no rule version %s", r.Raw)
	}
	if r := submit(p, map[string]any{"score": 10, "rule_version": 2, "run_token": "x"}); r.Code() != "rule_version_mismatch" {
		t.Fatalf("old rule version %s", r.Raw)
	}
	e.Expect(submit(p, map[string]any{"score": 10, "rule_version": 3}), 400) // run_token required
	e.Expect(e.Do("POST", "/v1/games/alpha/boards/nope/runs", p.Token, nil), 404)
	e.Expect(e.Do("POST", "/v1/games/alpha/boards/ranked/runs", "", nil), 401)

	// Too fast → quarantined; the token is then spent.
	tok := run(p, "ranked")
	if r := e.Expect(submit(p, map[string]any{"score": 10, "rule_version": 3, "run_token": tok}), 200); r.Body["status"] != "quarantined" {
		t.Fatalf("fast run %s", r.Raw)
	}
	if r := submit(p, map[string]any{"score": 10, "rule_version": 3, "run_token": tok}); r.Code() != "run_token_used" {
		t.Fatalf("replayed token %s", r.Raw)
	}
	// Tokens are bound to player and board.
	if r := submit(other, map[string]any{"score": 10, "rule_version": 3, "run_token": run(p, "ranked")}); r.Code() != "run_token_invalid" {
		t.Fatalf("other player %s", r.Raw)
	}
	if r := submit(p, map[string]any{"score": 10, "rule_version": 3, "run_token": run(p, "sumb")}); r.Code() != "run_token_invalid" {
		t.Fatalf("other board %s", r.Raw)
	}
	if r := submit(p, map[string]any{"score": 10, "rule_version": 3, "run_token": "run_forged"}); r.Code() != "run_token_invalid" {
		t.Fatalf("forged %s", r.Raw)
	}
	// Normal run; an idempotent retry replays instead of reporting the token as used.
	tok = run(p, "ranked")
	expired := run(p, "ranked")
	time.Sleep(250 * time.Millisecond)
	body := map[string]any{"score": 42, "rule_version": 3, "run_token": tok}
	r := e.Expect(submit(p, body, "Idempotency-Key", "s1"), 200)
	if r.Body["status"] != "accepted" || r.Body["best"].(float64) != 42 {
		t.Fatalf("run %s", r.Raw)
	}
	if r := submit(p, body, "Idempotency-Key", "s1"); r.Status != 200 || r.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("idempotent retry %s", r.Raw)
	}
	time.Sleep(1400 * time.Millisecond)
	if r := submit(p, map[string]any{"score": 50, "rule_version": 3, "run_token": expired}); r.Code() != "run_token_expired" {
		t.Fatalf("expired %s", r.Raw)
	}
}

func TestQuarantineReview(t *testing.T) {
	e := gametest.New(t, gametest.Options{Config: antiCheatDoc})
	p, other := e.Guest("alpha"), e.Guest("alpha")
	submit := func(pl gametest.Player, score int) gametest.Resp {
		return e.Expect(e.Do("POST", "/v1/games/alpha/boards/sumb/scores", pl.Token, map[string]int{"score": score}), 200)
	}
	submit(p, 50)
	if r := submit(p, 150); r.Body["status"] != "quarantined" || r.Body["best"].(float64) != 50 {
		t.Fatalf("quarantine %s", r.Raw)
	}
	submit(other, 500)
	// Beta has its own queue.
	b := e.Guest("beta")
	e.Expect(e.Do("POST", "/v1/games/beta/boards/sumb/scores", b.Token, map[string]int{"score": 700}), 200)

	admin := "/v1/admin/games/alpha/quarantine"
	l := e.Expect(e.Do("GET", admin+"?limit=1", e.AdminToken, nil), 200)
	items := l.Body["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["player_id"] != p.ID || l.Body["next_after"] == nil {
		t.Fatalf("list %s", l.Raw)
	}
	id := itoa(int64(items[0].(map[string]any)["id"].(float64)))
	l2 := e.Expect(e.Do("GET", admin+"?limit=1&after="+itoa(int64(l.Body["next_after"].(float64))), e.AdminToken, nil), 200)
	if it := l2.Body["items"].([]any); len(it) != 1 || it[0].(map[string]any)["player_id"] != other.ID || l2.Body["next_after"] != nil {
		t.Fatalf("page2 %s", l2.Raw)
	}
	e.Expect(e.Do("GET", admin+"?status=bogus", e.AdminToken, nil), 400)
	e.Expect(e.Do("GET", admin, p.Token, nil), 401)
	// Beta's entry is invisible from alpha.
	var betaID int64
	e.App.DB.Read.QueryRow(`SELECT id FROM score_quarantine WHERE game_id='beta'`).Scan(&betaID)
	e.Expect(e.Do("POST", admin+"/"+itoa(betaID)+"/approve", e.AdminToken, nil), 404)

	// Concurrent approvals apply the sum exactly once.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := e.Do("POST", admin+"/"+id+"/approve", e.AdminToken, nil); r.Status != 200 {
				t.Errorf("approve %s", r.Raw)
			}
		}()
	}
	wg.Wait()
	board := e.Expect(e.Do("GET", "/v1/games/alpha/boards/sumb", p.Token, nil), 200)
	if board.Body["me"].(map[string]any)["score"].(float64) != 200 {
		t.Fatalf("approved sum %s", board.Raw)
	}
	r := e.Expect(e.Do("POST", admin+"/"+id+"/approve", e.AdminToken, nil), 200)
	entry := r.Body["entry"].(map[string]any)
	if r.Body["replayed"] != true || entry["review"] != "approved" || entry["reviewed_by"] == nil || entry["reviewed_by"] == "" {
		t.Fatalf("approve replay %s", r.Raw)
	}
	if r := e.Do("POST", admin+"/"+id+"/reject", e.AdminToken, nil); r.Code() != "already_reviewed" {
		t.Fatalf("reject approved %s", r.Raw)
	}
	oid := itoa(int64(l2.Body["items"].([]any)[0].(map[string]any)["id"].(float64)))
	e.Expect(e.Do("POST", admin+"/"+oid+"/reject", e.AdminToken, nil), 200)
	if b := e.Expect(e.Do("GET", "/v1/games/alpha/boards/sumb", other.Token, nil), 200); b.Body["me"] != nil {
		t.Fatalf("rejected score ranked %s", b.Raw)
	}
	rej := e.Expect(e.Do("GET", admin+"?status=rejected", e.AdminToken, nil), 200)
	if len(rej.Body["items"].([]any)) != 1 {
		t.Fatalf("rejected list %s", rej.Raw)
	}
	e.Expect(e.Do("POST", admin+"/999999/approve", e.AdminToken, nil), 404)
	e.Expect(e.Do("POST", admin+"/"+oid+"/approve", p.Token, nil), 401)
}
