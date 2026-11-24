package game

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sort"

	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/idempotency"
)

// ErrInsufficientFunds is returned when a debit would make a balance negative.
var ErrInsufficientFunds = httpx.Conflict("insufficient_funds", "insufficient balance")

// ApplyLedger posts deltas for one source event inside tx: balances and the
// immutable ledger change together, and (game, sourceKey, asset) is unique so the
// same source can never post twice. Callers pass the business transaction.
func ApplyLedger(ctx context.Context, tx *sql.Tx, gameID, playerID, kind, sourceKey string, deltas map[string]int64, now int64) error {
	assets := make([]string, 0, len(deltas))
	for a := range deltas {
		assets = append(assets, a)
	}
	sort.Strings(assets) // deterministic order
	for _, asset := range assets {
		amount := deltas[asset]
		if amount == 0 {
			continue
		}
		var bal int64
		err := tx.QueryRowContext(ctx, `INSERT INTO wallet_balances(game_id,player_id,asset,balance,version,updated_at) VALUES(?,?,?,0,0,?)
			ON CONFLICT(game_id,player_id,asset) DO UPDATE SET updated_at=excluded.updated_at
			RETURNING balance`, gameID, playerID, asset, now).Scan(&bal)
		if err != nil {
			return err
		}
		next := bal + amount
		if next < 0 {
			return ErrInsufficientFunds
		}
		if _, err := tx.ExecContext(ctx, `UPDATE wallet_balances SET balance=?, version=version+1, updated_at=? WHERE game_id=? AND player_id=? AND asset=? AND balance=?`,
			next, now, gameID, playerID, asset, bal); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO game_ledger(id,game_id,player_id,asset,amount,balance_after,kind,source_key,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			httpx.RandomID("led_"), gameID, playerID, asset, amount, next, kind, sourceKey, now); err != nil {
			return fmt.Errorf("ledger %s/%s: %w", sourceKey, asset, err)
		}
	}
	return nil
}

func balances(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, gameID, playerID string) (map[string]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT asset, balance FROM wallet_balances WHERE game_id=? AND player_id=?`, gameID, playerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var a string
		var b int64
		if err := rows.Scan(&a, &b); err != nil {
			return nil, err
		}
		out[a] = b
	}
	return out, rows.Err()
}

func (k *Kit) getWallet(w http.ResponseWriter, r *http.Request) error {
	s := sessionFrom(r.Context())
	b, err := balances(r.Context(), k.DB.Read, s.GameID, s.PlayerID)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"balances": b})
	return nil
}

// claimReward grants a configured reward once per period limit (G05). Eligibility,
// the claim record, balances and ledger rows commit in one transaction.
func (k *Kit) claimReward(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := sessionFrom(ctx)
	reward := r.PathValue("reward")
	if !idPattern.MatchString(reward) {
		return httpx.NotFound("reward not found")
	}
	body, err := readBody(r, 1024)
	if err != nil {
		return err
	}
	status, resp, replayed, err := idempotency.Do(ctx, k.DB, "game:"+s.GameID+":"+s.PlayerID, r.Header.Get("Idempotency-Key"), "reward.claim:"+reward, body,
		func(tx *sql.Tx) (idempotency.Result, error) {
			// Read config inside the write transaction so the revision recorded is the one applied.
			c, err := k.currentConfig(ctx, tx, s.GameID)
			if err != nil {
				return idempotency.Result{}, err
			}
			def, ok := c.Doc.Rewards[reward]
			if !ok {
				return idempotency.Result{}, httpx.NotFound("reward not found")
			}
			now := k.Now()
			ms := now.UnixMilli()
			if (def.StartsAt != 0 && ms < def.StartsAt) || (def.EndsAt != 0 && ms >= def.EndsAt) {
				return idempotency.Result{}, httpx.Forbidden("reward is not available now")
			}
			period := "once"
			if def.Period == "daily" {
				period = SeasonID("daily", now)
			}
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM reward_claims WHERE game_id=? AND player_id=? AND reward_id=? AND period_key=?`,
				s.GameID, s.PlayerID, reward, period).Scan(&n); err != nil {
				return idempotency.Result{}, err
			}
			if n >= def.PerPeriod {
				return idempotency.Result{}, httpx.Conflict("already_claimed", "reward claim limit reached")
			}
			claimID := httpx.RandomID("rcl_")
			// The UNIQUE(game,player,reward,period,seq) constraint is the final guard against double grants.
			if _, err := tx.ExecContext(ctx, `INSERT INTO reward_claims(claim_id,game_id,player_id,reward_id,period_key,seq,config_revision,created_at) VALUES(?,?,?,?,?,?,?,?)`,
				claimID, s.GameID, s.PlayerID, reward, period, n+1, c.Revision, ms); err != nil {
				return idempotency.Result{}, err
			}
			if err := ApplyLedger(ctx, tx, s.GameID, s.PlayerID, "reward", "reward:"+claimID, def.Grants, ms); err != nil {
				return idempotency.Result{}, err
			}
			if err := checkStacks(ctx, tx, c.Doc, s.GameID, s.PlayerID, def.Grants); err != nil {
				return idempotency.Result{}, err
			}
			b, err := balances(ctx, tx, s.GameID, s.PlayerID)
			if err != nil {
				return idempotency.Result{}, err
			}
			return idempotency.Result{Body: map[string]any{"claim_id": claimID, "grants": def.Grants, "balances": b, "config_revision": c.Revision}}, nil
		})
	if err != nil {
		return mapWriteErr(err)
	}
	if !replayed && k.Metrics != nil {
		k.Metrics.Counter("game_reward_claims_total", "").Add(1)
	}
	idempotency.Write(w, status, resp, replayed)
	return nil
}

// Mismatch is a wallet balance that disagrees with the ledger sum.
type Mismatch struct {
	GameID   string `json:"game_id"`
	PlayerID string `json:"player_id"`
	Asset    string `json:"asset"`
	Balance  int64  `json:"balance"`
	Ledger   int64  `json:"ledger_sum"`
}

// Reconcile compares every balance with the sum of its ledger entries (G05 对账, G-A10).
func Reconcile(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, gameID string) ([]Mismatch, error) {
	rows, err := q.QueryContext(ctx, `
		WITH l AS (SELECT game_id, player_id, asset, sum(amount) AS s FROM game_ledger WHERE (?='' OR game_id=?) GROUP BY 1,2,3)
		SELECT coalesce(w.game_id,l.game_id), coalesce(w.player_id,l.player_id), coalesce(w.asset,l.asset), coalesce(w.balance,0), coalesce(l.s,0)
		FROM wallet_balances w FULL OUTER JOIN l ON w.game_id=l.game_id AND w.player_id=l.player_id AND w.asset=l.asset
		WHERE (?='' OR coalesce(w.game_id,l.game_id)=?) AND coalesce(w.balance,0) <> coalesce(l.s,0)`, gameID, gameID, gameID, gameID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Mismatch{}
	for rows.Next() {
		var m Mismatch
		if err := rows.Scan(&m.GameID, &m.PlayerID, &m.Asset, &m.Balance, &m.Ledger); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
