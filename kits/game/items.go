package game

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/idempotency"
)

// checkStacks returns 409 stack_limit when a positive delta left an item above
// its catalog max_stack. Call after ApplyLedger inside the same transaction.
func checkStacks(ctx context.Context, tx *sql.Tx, doc Document, gameID, playerID string, deltas map[string]int64) error {
	for asset, d := range deltas {
		it, ok := doc.Items[asset]
		if !ok || d <= 0 || it.MaxStack == 0 {
			continue
		}
		var bal int64
		if err := tx.QueryRowContext(ctx, `SELECT balance FROM wallet_balances WHERE game_id=? AND player_id=? AND asset=?`, gameID, playerID, asset).Scan(&bal); err != nil {
			return err
		}
		if bal > it.MaxStack {
			e := httpx.Conflict("stack_limit", "item stack limit exceeded")
			e.Details = map[string]any{"asset": asset, "max_stack": it.MaxStack}
			return e
		}
	}
	return nil
}

// clampStacks reduces grants so items do not exceed max_stack (used where a
// grant must not fail, e.g. season settlement).
func clampStacks(ctx context.Context, tx *sql.Tx, doc Document, gameID, playerID string, grants map[string]int64) (map[string]int64, error) {
	out := maps.Clone(grants)
	for asset, g := range grants {
		it, ok := doc.Items[asset]
		if !ok || it.MaxStack == 0 {
			continue
		}
		var bal int64
		err := tx.QueryRowContext(ctx, `SELECT balance FROM wallet_balances WHERE game_id=? AND player_id=? AND asset=?`, gameID, playerID, asset).Scan(&bal)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		out[asset] = max(0, min(g, it.MaxStack-bal))
		if out[asset] == 0 {
			delete(out, asset)
		}
	}
	return out, nil
}

// purchaseOffer exchanges an offer's cost for its grants in one ledger posting.
func (k *Kit) purchaseOffer(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := sessionFrom(ctx)
	offer := r.PathValue("offer")
	if !idPattern.MatchString(offer) {
		return httpx.NotFound("offer not found")
	}
	body, err := readBody(r, 1024)
	if err != nil {
		return err
	}
	status, resp, replayed, err := idempotency.Do(ctx, k.DB, "game:"+s.GameID+":"+s.PlayerID, r.Header.Get("Idempotency-Key"), "offer.purchase:"+offer, body,
		func(tx *sql.Tx) (idempotency.Result, error) {
			c, err := k.currentConfig(ctx, tx, s.GameID)
			if err != nil {
				return idempotency.Result{}, err
			}
			def, ok := c.Doc.Offers[offer]
			if !ok {
				return idempotency.Result{}, httpx.NotFound("offer not found")
			}
			now := k.Now()
			ms := now.UnixMilli()
			period := def.Period // once | unlimited
			if period == "daily" {
				period = SeasonID("daily", now)
			}
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(seq),0) FROM offer_purchases WHERE game_id=? AND player_id=? AND offer_id=? AND period_key=?`,
				s.GameID, s.PlayerID, offer, period).Scan(&n); err != nil {
				return idempotency.Result{}, err
			}
			if def.Period != "unlimited" && n >= def.PerPeriod {
				return idempotency.Result{}, httpx.Conflict("purchase_limit", "offer purchase limit reached")
			}
			id := httpx.RandomID("pur_")
			if _, err := tx.ExecContext(ctx, `INSERT INTO offer_purchases(purchase_id,game_id,player_id,offer_id,period_key,seq,config_revision,created_at) VALUES(?,?,?,?,?,?,?,?)`,
				id, s.GameID, s.PlayerID, offer, period, n+1, c.Revision, ms); err != nil {
				return idempotency.Result{}, err
			}
			deltas := maps.Clone(def.Grants)
			for a, v := range def.Cost {
				deltas[a] = -v
			}
			if err := ApplyLedger(ctx, tx, s.GameID, s.PlayerID, "purchase", "offer:"+id, deltas, ms); err != nil {
				return idempotency.Result{}, err
			}
			if err := checkStacks(ctx, tx, c.Doc, s.GameID, s.PlayerID, def.Grants); err != nil {
				return idempotency.Result{}, err
			}
			b, err := balances(ctx, tx, s.GameID, s.PlayerID)
			if err != nil {
				return idempotency.Result{}, err
			}
			return idempotency.Result{Body: map[string]any{"purchase_id": id, "cost": def.Cost, "grants": def.Grants, "balances": b, "config_revision": c.Revision}}, nil
		})
	if err != nil {
		return mapWriteErr(err)
	}
	idempotency.Write(w, status, resp, replayed)
	return nil
}

type inventoryEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Balance  int64  `json:"balance"`
	MaxStack int64  `json:"max_stack,omitempty"`
}

// getInventory splits balances by the items catalog: every catalog currency
// (zero included), held items, and held assets missing from the catalog.
func (k *Kit) getInventory(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := sessionFrom(ctx)
	c, err := k.currentConfig(ctx, k.DB.Read, s.GameID)
	if err != nil {
		return err
	}
	b, err := balances(ctx, k.DB.Read, s.GameID, s.PlayerID)
	if err != nil {
		return err
	}
	out := struct {
		Currencies  []inventoryEntry `json:"currencies"`
		Items       []inventoryEntry `json:"items"`
		Uncataloged map[string]int64 `json:"uncataloged"`
	}{[]inventoryEntry{}, []inventoryEntry{}, map[string]int64{}}
	for id, it := range c.Doc.Items {
		e := inventoryEntry{ID: id, Name: it.Name, Balance: b[id], MaxStack: it.MaxStack}
		if it.Kind == "currency" {
			out.Currencies = append(out.Currencies, e)
		} else if e.Balance > 0 {
			out.Items = append(out.Items, e)
		}
	}
	for a, v := range b {
		if _, ok := c.Doc.Items[a]; !ok && v != 0 {
			out.Uncataloged[a] = v
		}
	}
	byID := func(s []inventoryEntry) { sort.Slice(s, func(i, j int) bool { return s[i].ID < s[j].ID }) }
	byID(out.Currencies)
	byID(out.Items)
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// adminAdjust posts an operator correction (kind "adjust"). source_key makes it
// idempotent; the reason and admin key are kept in wallet_adjustments.
func (k *Kit) adminAdjust(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	g := CurrentGame(ctx)
	key := AdminKey(ctx)
	player := r.PathValue("player")
	if player == "" || len(player) > 64 {
		return httpx.NotFound("player not found")
	}
	body, err := readBody(r, 16<<10)
	if err != nil {
		return err
	}
	var req struct {
		Deltas    map[string]int64 `json:"deltas"`
		Reason    string           `json:"reason"`
		SourceKey string           `json:"source_key"`
	}
	if err := decodeStrict(body, &req); err != nil {
		return err
	}
	if len(req.Deltas) == 0 || len(req.Deltas) > 100 {
		return httpx.BadRequest("deltas must contain 1-100 assets")
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" || utf8.RuneCountInString(req.Reason) > 500 {
		return httpx.BadRequest("reason (1-500 characters) required")
	}
	if !idempotency.ValidKey(req.SourceKey) {
		return httpx.BadRequest("source_key must be 1-128 printable ASCII characters")
	}
	deltasJSON, _ := json.Marshal(req.Deltas) // map keys are sorted: canonical
	var out map[string]any
	err = k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		out = nil
		if err := EnsurePlayer(ctx, tx, g.ID, player); err != nil {
			return err
		}
		var prevPlayer, prevDeltas string
		err := tx.QueryRowContext(ctx, `SELECT player_id, deltas FROM wallet_adjustments WHERE game_id=? AND source_key=?`, g.ID, req.SourceKey).Scan(&prevPlayer, &prevDeltas)
		if err == nil {
			if prevPlayer != player || prevDeltas != string(deltasJSON) {
				return httpx.Conflict("source_key_reused", "source_key was used for a different adjustment")
			}
			b, err := balances(ctx, tx, g.ID, player)
			out = map[string]any{"source_key": req.SourceKey, "replayed": true, "balances": b}
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		c, err := k.currentConfig(ctx, tx, g.ID)
		if err != nil {
			return err
		}
		for a, d := range req.Deltas {
			if !idPattern.MatchString(a) || d == 0 || d > maxGrant || d < -maxGrant {
				return httpx.BadRequest("deltas must map asset ids to non-zero integers within ±1e12")
			}
			if _, ok := c.Doc.Items[a]; len(c.Doc.Items) > 0 && !ok {
				return httpx.BadRequest("asset " + a + " is not in the items catalog")
			}
		}
		now := k.Now().UnixMilli()
		if _, err := tx.ExecContext(ctx, `INSERT INTO wallet_adjustments(game_id,source_key,player_id,deltas,reason,admin_key_id,created_at) VALUES(?,?,?,?,?,?,?)`,
			g.ID, req.SourceKey, player, string(deltasJSON), req.Reason, key.ID, now); err != nil {
			return err
		}
		if err := ApplyLedger(ctx, tx, g.ID, player, "adjust", "adjust:"+req.SourceKey, req.Deltas, now); err != nil {
			return err
		}
		if err := checkStacks(ctx, tx, c.Doc, g.ID, player, req.Deltas); err != nil {
			return err
		}
		b, err := balances(ctx, tx, g.ID, player)
		out = map[string]any{"source_key": req.SourceKey, "replayed": false, "balances": b}
		return err
	})
	if err != nil {
		return mapWriteErr(err)
	}
	k.Log.InfoContext(ctx, "wallet adjusted", "game", g.ID, "player", player, "source_key", req.SourceKey, "by", key.ID)
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}
