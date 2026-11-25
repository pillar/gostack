package game_test

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/pillar/gostack/kits/game"
)

// Reconcile finds every kind of balance/ledger mismatch and stays linear in
// players (the former FULL OUTER JOIN took ~130 s for 20k players on SQLite).
func TestReconcileMismatchKindsAndScale(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	const n = 20000
	err := e.a.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		for i := 0; i < n; i++ {
			p := fmt.Sprintf("p%06d", i)
			if _, err := tx.ExecContext(ctx, `INSERT INTO players(game_id,player_id,created_at) VALUES('alpha',?,0)`, p); err != nil {
				return err
			}
			if err := game.ApplyLedger(ctx, tx, "alpha", p, "reward", "s:"+p, map[string]int64{"coin": 5, "gem": 1}, 0); err != nil {
				return err
			}
		}
		// Tamper: balance differs from ledger; balance without any ledger row;
		// ledger row whose balance row is gone (wallet row deleted).
		for _, q := range []string{
			`UPDATE wallet_balances SET balance=7 WHERE game_id='alpha' AND player_id='p000001' AND asset='coin'`,
			`INSERT INTO wallet_balances(game_id,player_id,asset,balance,version,updated_at) VALUES('alpha','p000002','ruby',3,1,0)`,
			`DELETE FROM wallet_balances WHERE game_id='alpha' AND player_id='p000003' AND asset='gem'`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	mm, err := game.Reconcile(ctx, e.a.DB.Read, "")
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("reconcile of %d players took %v", n, d)
	}
	var got []string
	for _, m := range mm {
		got = append(got, fmt.Sprintf("%s/%s/%s %d!=%d", m.GameID, m.PlayerID, m.Asset, m.Balance, m.Ledger))
	}
	sort.Strings(got)
	want := []string{"alpha/p000001/coin 7!=5", "alpha/p000002/ruby 3!=0", "alpha/p000003/gem 0!=1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("mismatches %v, want %v", got, want)
	}
	if mm, _ := game.Reconcile(ctx, e.a.DB.Read, "beta"); len(mm) != 0 {
		t.Fatalf("game filter leaked: %v", mm)
	}
}
