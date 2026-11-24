package commerce

import (
	"database/sql"
	"net/http"
	"strconv"

	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
)

const maxCatalog = 500

type productDef struct {
	Grants             grants            `json:"grants"`
	ProviderProductIDs map[string]string `json:"provider_product_ids"`
}

// adminPutProducts replaces the game's product catalog and store mappings.
// Existing orders keep the grants snapshot taken at delivery.
func (m *Module) adminPutProducts(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	g := game.CurrentGame(ctx)
	body, err := game.ReadBody(r, 512<<10)
	if err != nil {
		return err
	}
	var in map[string]productDef
	if err := game.DecodeStrict(body, &in); err != nil {
		return err
	}
	if len(in) > maxCatalog {
		return httpx.BadRequest("too many products")
	}
	seen := map[string]string{}
	for id, p := range in {
		if !game.ValidID(id) {
			return httpx.BadRequest("product id " + id + " is invalid")
		}
		if err := p.Grants.validate("product " + id); err != nil {
			return err
		}
		for prov, ppid := range p.ProviderProductIDs {
			if _, ok := m.IAP[prov]; !ok {
				return httpx.BadRequest("product " + id + ": provider " + prov + " is not configured")
			}
			if ppid == "" || len(ppid) > 128 {
				return httpx.BadRequest("product " + id + ": provider product id must be 1-128 characters")
			}
			if other, dup := seen[prov+"\x00"+ppid]; dup {
				return httpx.BadRequest("provider product id " + ppid + " is mapped by both " + other + " and " + id)
			}
			seen[prov+"\x00"+ppid] = id
		}
	}
	now := m.now().UnixMilli()
	err = m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM commerce_provider_products WHERE game_id=?`, g.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM commerce_products WHERE game_id=?`, g.ID); err != nil {
			return err
		}
		for id, p := range in {
			if _, err := tx.ExecContext(ctx, `INSERT INTO commerce_products(game_id,product_id,grants,updated_at) VALUES(?,?,?,?)`, g.ID, id, p.Grants.json(), now); err != nil {
				return err
			}
			for prov, ppid := range p.ProviderProductIDs {
				if _, err := tx.ExecContext(ctx, `INSERT INTO commerce_provider_products(game_id,provider,provider_product_id,product_id) VALUES(?,?,?,?)`, g.ID, prov, ppid, id); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	return m.adminGetProducts(w, r)
}

func (m *Module) adminGetProducts(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	g := game.CurrentGame(ctx)
	out := map[string]productDef{}
	rows, err := m.DB.Read.QueryContext(ctx, `SELECT product_id, grants FROM commerce_products WHERE game_id=?`, g.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, gr string
		if err := rows.Scan(&id, &gr); err != nil {
			rows.Close()
			return err
		}
		out[id] = productDef{Grants: parseGrants(gr), ProviderProductIDs: map[string]string{}}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = m.DB.Read.QueryContext(ctx, `SELECT product_id, provider, provider_product_id FROM commerce_provider_products WHERE game_id=?`, g.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, prov, ppid string
		if err := rows.Scan(&id, &prov, &ppid); err != nil {
			return err
		}
		if p, ok := out[id]; ok {
			p.ProviderProductIDs[prov] = ppid
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"products": out})
	return nil
}

type placementDef struct {
	Grants   grants `json:"grants"`
	DailyCap int    `json:"daily_cap"`
}

// adminPutPlacements replaces the game's rewarded-ad placements.
func (m *Module) adminPutPlacements(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	g := game.CurrentGame(ctx)
	body, err := game.ReadBody(r, 128<<10)
	if err != nil {
		return err
	}
	var in map[string]placementDef
	if err := game.DecodeStrict(body, &in); err != nil {
		return err
	}
	if len(in) > maxCatalog {
		return httpx.BadRequest("too many placements")
	}
	for id, p := range in {
		if !game.ValidID(id) {
			return httpx.BadRequest("placement id " + id + " is invalid")
		}
		if err := p.Grants.validate("placement " + id); err != nil {
			return err
		}
		if p.DailyCap <= 0 || p.DailyCap > 1000 {
			return httpx.BadRequest("placement " + id + ": daily_cap must be 1-1000")
		}
	}
	now := m.now().UnixMilli()
	err = m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM commerce_ad_placements WHERE game_id=?`, g.ID); err != nil {
			return err
		}
		for id, p := range in {
			if _, err := tx.ExecContext(ctx, `INSERT INTO commerce_ad_placements(game_id,placement_id,grants,daily_cap,updated_at) VALUES(?,?,?,?,?)`,
				g.ID, id, p.Grants.json(), p.DailyCap, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	return m.adminGetPlacements(w, r)
}

func (m *Module) adminGetPlacements(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	g := game.CurrentGame(ctx)
	rows, err := m.DB.Read.QueryContext(ctx, `SELECT placement_id, grants, daily_cap FROM commerce_ad_placements WHERE game_id=?`, g.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := map[string]placementDef{}
	for rows.Next() {
		var id, gr string
		var p placementDef
		if err := rows.Scan(&id, &gr, &p.DailyCap); err != nil {
			return err
		}
		p.Grants = parseGrants(gr)
		out[id] = p
	}
	if err := rows.Err(); err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"placements": out})
	return nil
}

var orderStates = map[string]bool{"created": true, "paid": true, "delivered": true, "refunded": true, "refund_compensated": true, "refund_shortfall": true}

// adminListOrders lists orders, newest update first (?state=&player_id=&limit=).
func (m *Module) adminListOrders(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	g := game.CurrentGame(ctx)
	q := r.URL.Query()
	state, player := q.Get("state"), q.Get("player_id")
	if state != "" && !orderStates[state] {
		return httpx.BadRequest("unknown state")
	}
	limit := 50
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 200 {
			return httpx.BadRequest("limit must be 1-200")
		}
		limit = n
	}
	rows, err := m.DB.Read.QueryContext(ctx, `SELECT `+orderCols+` FROM commerce_orders
		WHERE game_id=? AND (?='' OR state=?) AND (?='' OR player_id=?) ORDER BY updated_at DESC, order_id LIMIT ?`,
		g.ID, state, state, player, player, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return err
		}
		out = append(out, o.view())
	}
	if err := rows.Err(); err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"orders": out})
	return nil
}

// adminResolveOrder retries recovering a refund shortfall from the player's
// current balances; {"waive": true} writes off whatever is still outstanding.
// The order becomes refund_compensated once nothing is outstanding or it was waived.
func (m *Module) adminResolveOrder(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	g := game.CurrentGame(ctx)
	body, err := game.ReadBody(r, 1024)
	if err != nil {
		return err
	}
	var in struct {
		Waive bool `json:"waive"`
	}
	if len(body) > 0 {
		if err := game.DecodeStrict(body, &in); err != nil {
			return err
		}
	}
	var o *order
	err = m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		if o, err = orderByID(ctx, tx, g.ID, r.PathValue("order")); err != nil {
			return err
		}
		if o == nil {
			return httpx.NotFound("order not found")
		}
		if o.State != "refund_shortfall" {
			return httpx.Conflict("invalid_state", "order is not in refund_shortfall")
		}
		now := m.now().UnixMilli()
		n := o.RefundAttempts + 1
		left, err := recoverGrants(ctx, tx, g.ID, o.PlayerID, "refund:"+o.Provider+":"+o.TxnID+":"+strconv.Itoa(n), o.Outstanding, now)
		if err != nil {
			return err
		}
		state := "refund_shortfall"
		if len(left) == 0 || in.Waive {
			state = "refund_compensated"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE commerce_orders SET state=?, outstanding=?, refund_attempts=?, updated_at=? WHERE order_id=?`,
			state, left.json(), n, now, o.ID); err != nil {
			return err
		}
		if state == "refund_compensated" {
			if err := m.enqueue(ctx, tx, g.TenantID, "compensated:"+o.ID, OutboxEvent{Event: "refund_compensated", GameID: g.ID, PlayerID: o.PlayerID, OrderID: o.ID, ProductID: o.ProductID, Outstanding: left, At: now}); err != nil {
				return err
			}
		}
		o, err = orderByID(ctx, tx, g.ID, o.ID)
		return err
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"order": o.view()})
	return nil
}
