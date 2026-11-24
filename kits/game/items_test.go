package game_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/kits/game/gametest"
)

const itemsDoc = `{
  "items": {
    "coin": {"kind": "currency", "name": "Coin"},
    "gem": {"kind": "currency"},
    "potion": {"kind": "item", "max_stack": 10},
    "sword": {"kind": "item", "max_stack": 1}
  },
  "rewards": {
    "starter": {"grants": {"coin": 100}, "period": "once", "per_period": 1},
    "sword_gift": {"grants": {"sword": 1}, "period": "daily", "per_period": 5}
  },
  "offers": {
    "potion_pack": {"cost": {"coin": 30}, "grants": {"potion": 3}, "period": "daily", "per_period": 2},
    "sword": {"cost": {"coin": 10}, "grants": {"sword": 1}, "period": "unlimited"}
  }
}`

func adjust(e *gametest.Env, token, gameID, player string, deltas map[string]int64, reason, key string) gametest.Resp {
	return e.Do("POST", "/v1/admin/games/"+gameID+"/players/"+player+"/adjust", token,
		map[string]any{"deltas": deltas, "reason": reason, "source_key": key})
}

func TestOffersInventoryAndStacks(t *testing.T) {
	e := gametest.New(t, gametest.Options{Config: itemsDoc})
	p := e.Guest("alpha")
	buy := func(pl gametest.Player, offer, key string) gametest.Resp {
		return e.Do("POST", "/v1/games/alpha/offers/"+offer+"/purchase", pl.Token, "{}", "Idempotency-Key", key)
	}
	if r := buy(p, "potion_pack", "k0"); r.Code() != "insufficient_funds" {
		t.Fatalf("no funds %s", r.Raw)
	}
	e.Expect(e.Do("POST", "/v1/games/alpha/rewards/starter/claim", p.Token, "{}"), 200)
	r := e.Expect(buy(p, "potion_pack", "k1"), 200)
	if b := r.Body["balances"].(map[string]any); b["coin"].(float64) != 70 || b["potion"].(float64) != 3 {
		t.Fatalf("purchase %s", r.Raw)
	}
	rr := e.Expect(buy(p, "potion_pack", "k1"), 200)
	if rr.Header.Get("Idempotent-Replayed") != "true" || rr.Body["purchase_id"] != r.Body["purchase_id"] {
		t.Fatalf("replay %s", rr.Raw)
	}
	e.Expect(buy(p, "potion_pack", "k2"), 200)
	if r := buy(p, "potion_pack", "k3"); r.Code() != "purchase_limit" {
		t.Fatalf("limit %s", r.Raw)
	}
	// Stack cap: second sword exceeds max_stack 1 and rolls back the payment.
	e.Expect(buy(p, "sword", "s1"), 200)
	if r := buy(p, "sword", "s2"); r.Code() != "stack_limit" {
		t.Fatalf("stack %s", r.Raw)
	}
	if r := e.Do("POST", "/v1/games/alpha/rewards/sword_gift/claim", p.Token, "{}"); r.Code() != "stack_limit" {
		t.Fatalf("reward stack %s", r.Raw)
	}
	if balance(t, e, p, "coin") != 30 || balance(t, e, p, "sword") != 1 {
		t.Fatal("stack_limit did not roll back")
	}
	e.Expect(buy(p, "nope", "n1"), 404)
	e.Expect(e.Do("POST", "/v1/games/beta/offers/sword/purchase", p.Token, "{}"), 403)

	inv := e.Expect(e.Do("GET", "/v1/games/alpha/inventory", p.Token, nil), 200)
	cur := inv.Body["currencies"].([]any)
	items := inv.Body["items"].([]any)
	if len(cur) != 2 || cur[0].(map[string]any)["id"] != "coin" || cur[0].(map[string]any)["name"] != "Coin" || cur[0].(map[string]any)["balance"].(float64) != 30 ||
		cur[1].(map[string]any)["id"] != "gem" || len(items) != 2 || items[1].(map[string]any)["id"] != "sword" || items[1].(map[string]any)["max_stack"].(float64) != 1 {
		t.Fatalf("inventory %s", inv.Raw)
	}
	e.Expect(e.Do("GET", "/v1/games/alpha/inventory", "", nil), 401)

	// Concurrent purchases with funds for exactly one pack: one succeeds.
	q := e.Guest("alpha")
	e.Expect(adjust(e, e.AdminToken, "alpha", q.ID, map[string]int64{"coin": 30}, "grant", "q-30"), 200)
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := buy(q, "potion_pack", fmt.Sprintf("c%d", i))
			switch {
			case r.Status == 200:
				ok.Add(1)
			case r.Code() != "insufficient_funds":
				t.Errorf("concurrent %s", r.Raw)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || balance(t, e, q, "potion") != 3 || balance(t, e, q, "coin") != 0 {
		t.Fatalf("concurrent purchases ok=%d", ok.Load())
	}
	if ms, _ := game.Reconcile(context.Background(), e.App.DB.Read, ""); len(ms) != 0 {
		t.Fatalf("reconcile %v", ms)
	}
}

func TestAdminAdjust(t *testing.T) {
	e := gametest.New(t, gametest.Options{Config: itemsDoc})
	p, b := e.Guest("alpha"), e.Guest("beta")
	r := e.Expect(adjust(e, e.AdminToken, "alpha", p.ID, map[string]int64{"coin": 500, "gem": 3}, "support ticket 42", "t-42"), 200)
	if r.Body["replayed"] != false || r.Body["balances"].(map[string]any)["coin"].(float64) != 500 {
		t.Fatalf("adjust %s", r.Raw)
	}
	r = e.Expect(adjust(e, e.AdminToken, "alpha", p.ID, map[string]int64{"gem": 3, "coin": 500}, "support ticket 42", "t-42"), 200)
	if r.Body["replayed"] != true || balance(t, e, p, "coin") != 500 {
		t.Fatalf("adjust replay %s", r.Raw)
	}
	if r := adjust(e, e.AdminToken, "alpha", p.ID, map[string]int64{"coin": 1}, "x", "t-42"); r.Code() != "source_key_reused" {
		t.Fatalf("reuse %s", r.Raw)
	}
	if r := adjust(e, e.AdminToken, "alpha", p.ID, map[string]int64{"coin": -501}, "clawback", "t-43"); r.Code() != "insufficient_funds" {
		t.Fatalf("overdraft %s", r.Raw)
	}
	e.Expect(adjust(e, e.AdminToken, "alpha", p.ID, map[string]int64{"coin": -100}, "clawback", "t-44"), 200)
	if r := adjust(e, e.AdminToken, "alpha", p.ID, map[string]int64{"sword": 2}, "too many", "t-45"); r.Code() != "stack_limit" {
		t.Fatalf("stack %s", r.Raw)
	}
	e.Expect(adjust(e, e.AdminToken, "alpha", p.ID, map[string]int64{"ruby": 1}, "x", "t-46"), 400)
	e.Expect(adjust(e, e.AdminToken, "alpha", p.ID, map[string]int64{"coin": 1}, "", "t-47"), 400)
	e.Expect(adjust(e, e.AdminToken, "alpha", p.ID, map[string]int64{"coin": 1}, "x", ""), 400)
	e.Expect(adjust(e, e.AdminToken, "alpha", b.ID, map[string]int64{"coin": 1}, "x", "t-48"), 404) // beta player via alpha
	// Players cannot call adjust.
	e.Expect(adjust(e, p.Token, "alpha", p.ID, map[string]int64{"coin": 1}, "x", "t-49"), 401)
	if balance(t, e, p, "coin") != 400 {
		t.Fatal("balance")
	}
	var reason, by string
	if err := e.App.DB.Read.QueryRow(`SELECT reason, admin_key_id FROM wallet_adjustments WHERE game_id='alpha' AND source_key='t-42'`).Scan(&reason, &by); err != nil || reason != "support ticket 42" || by == "" {
		t.Fatalf("audit %q %q %v", reason, by, err)
	}
	if n := ledgerRows(t, e, "adjust"); n != 3 {
		t.Fatalf("adjust ledger rows %d", n)
	}
}

func TestP0ConfigValidation(t *testing.T) {
	e := gametest.New(t, gametest.Options{})
	bad := []string{
		`{"items": {"coin": {"kind": "currency"}}, "rewards": {"r": {"grants": {"gem": 1}, "period": "once", "per_period": 1}}}`,
		`{"items": {"coin": {"kind": "currency", "max_stack": 5}}}`,
		`{"items": {"coin": {"kind": "thing"}}}`,
		`{"offers": {"o": {"cost": {"coin": 1}, "grants": {"gem": 1}, "period": "weekly", "per_period": 1}}}`,
		`{"offers": {"o": {"cost": {"coin": 1}, "grants": {"coin": 1}, "period": "unlimited"}}}`,
		`{"offers": {"o": {"cost": {}, "grants": {"gem": 1}, "period": "unlimited"}}}`,
		`{"boards": {"b": {"order": "desc", "mode": "best", "period": "none", "min_score": 0, "max_score": 1, "settlement": {"rewards": [{"from_rank": 1, "to_rank": 1, "grants": {"coin": 1}}]}}}}`,
		`{"boards": {"b": {"order": "desc", "mode": "best", "period": "daily", "min_score": 0, "max_score": 1, "settlement": {"rewards": [{"from_rank": 1, "to_rank": 3, "grants": {"coin": 1}}, {"from_rank": 3, "to_rank": 4, "grants": {"coin": 1}}]}}}}`,
		`{"boards": {"b": {"order": "desc", "mode": "best", "period": "daily", "min_score": 0, "max_score": 1, "settlement": {"rewards": [{"from_rank": 1, "to_rank": 1, "grants": {"coin": 0}}]}}}}`,
		`{"boards": {"b": {"order": "desc", "mode": "best", "period": "none", "min_score": 0, "max_score": 1, "min_run_ms": 10}}}`,
		`{"boards": {"b": {"order": "desc", "mode": "best", "period": "none", "min_score": 0, "max_score": 1, "require_run": true, "min_run_ms": 100, "run_ttl_ms": 100}}}`,
		`{"saves": {"min_schema_version": -1}}`,
		`{"rollouts": [{"id": "r", "enabled": true, "percent": 101, "client": {"a": 1}}]}`,
		`{"rollouts": [{"id": "r", "enabled": true, "percent": 10, "min_client_version": "1.x", "client": {"a": 1}}]}`,
		`{"rollouts": [{"id": "r", "enabled": true, "percent": 10, "client": {}}]}`,
		`{"rollouts": [{"id": "r", "enabled": true, "percent": 10, "client": {"a": 1}}, {"id": "r", "enabled": true, "percent": 10, "client": {"a": 1}}]}`,
	}
	for _, doc := range bad {
		if r := e.Do("PUT", "/v1/admin/games/alpha/config", e.AdminToken, doc); r.Status != 400 {
			t.Errorf("accepted %s: %d %s", doc, r.Status, r.Raw)
		}
	}
	e.Expect(e.Do("PUT", "/v1/admin/games/alpha/config", e.AdminToken, itemsDoc), 200)
	e.Expect(e.Do("PUT", "/v1/admin/games/alpha/config", e.AdminToken, settleDoc), 200)
}
