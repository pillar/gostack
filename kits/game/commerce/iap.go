package commerce

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/modules/idempotency"
)

// order is a commerce_orders row.
type order struct {
	ID, GameID, PlayerID, Provider, ProductID, ProviderProductID string
	Token, TxnID, State, PrepayID                                string
	Quantity, RefundAttempts                                     int
	Amount                                                       int64
	Grants, Outstanding                                          grants
	CreatedAt, UpdatedAt                                         int64
	DeliveredAt, RefundedAt, FinishedAt                          sql.NullInt64
	FinishState, FinishError                                     string // store-side completion (Google acknowledge/consume)
}

const orderCols = `order_id, game_id, coalesce(player_id,''), provider, coalesce(product_id,''), coalesce(provider_product_id,''),
	coalesce(app_account_token,''), coalesce(provider_txn_id,''), state, quantity, refund_attempts, grants, outstanding,
	created_at, updated_at, delivered_at, refunded_at, amount, prepay_id, finish_state, finish_error, finished_at`

type rowScanner interface{ Scan(...any) error }

func scanOrder(r rowScanner) (*order, error) {
	var o order
	var g, out string
	err := r.Scan(&o.ID, &o.GameID, &o.PlayerID, &o.Provider, &o.ProductID, &o.ProviderProductID, &o.Token, &o.TxnID,
		&o.State, &o.Quantity, &o.RefundAttempts, &g, &out, &o.CreatedAt, &o.UpdatedAt, &o.DeliveredAt, &o.RefundedAt, &o.Amount, &o.PrepayID,
		&o.FinishState, &o.FinishError, &o.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o.Grants, o.Outstanding = parseGrants(g), parseGrants(out)
	return &o, nil
}

func (o *order) view() map[string]any {
	v := map[string]any{
		"order_id": o.ID, "provider": o.Provider, "product_id": o.ProductID, "provider_product_id": o.ProviderProductID,
		"provider_txn_id": o.TxnID, "state": o.State, "quantity": o.Quantity, "grants": o.Grants,
		"outstanding": o.Outstanding, "player_id": o.PlayerID, "created_at": o.CreatedAt, "updated_at": o.UpdatedAt,
	}
	if o.DeliveredAt.Valid {
		v["delivered_at"] = o.DeliveredAt.Int64
	}
	if o.RefundedAt.Valid {
		v["refunded_at"] = o.RefundedAt.Int64
	}
	if o.Amount > 0 {
		v["amount"] = o.Amount
	}
	return v
}

// adminView adds operational fields (store-side completion) to view.
func (o *order) adminView() map[string]any {
	v := o.view()
	if o.FinishState != "" {
		v["finish_state"] = o.FinishState
	}
	if o.FinishError != "" {
		v["finish_error"] = o.FinishError
	}
	if o.FinishedAt.Valid {
		v["finished_at"] = o.FinishedAt.Int64
	}
	return v
}

func orderByTxn(ctx context.Context, tx *sql.Tx, provider, txnID string) (*order, error) {
	return scanOrder(tx.QueryRowContext(ctx, `SELECT `+orderCols+` FROM commerce_orders WHERE provider=? AND provider_txn_id=?`, provider, txnID))
}

func orderByID(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, gameID, id string) (*order, error) {
	return scanOrder(q.QueryRowContext(ctx, `SELECT `+orderCols+` FROM commerce_orders WHERE game_id=? AND order_id=?`, gameID, id))
}

// tokenOrder returns the earliest order issued with token in the game: its
// player is the token's owner. Prefers an unfilled (created) order.
func tokenOrder(ctx context.Context, tx *sql.Tx, gameID, token string) (*order, error) {
	if token == "" {
		return nil, nil
	}
	return scanOrder(tx.QueryRowContext(ctx, `SELECT `+orderCols+` FROM commerce_orders WHERE game_id=? AND app_account_token=?
		ORDER BY (state='created') DESC, created_at LIMIT 1`, gameID, token))
}

// productFor maps a store product id to the game's product and its unit grants.
func productFor(ctx context.Context, tx *sql.Tx, gameID, provider, providerProductID string) (string, grants, error) {
	var pid, g string
	err := tx.QueryRowContext(ctx, `SELECT p.product_id, p.grants FROM commerce_provider_products m
		JOIN commerce_products p ON p.game_id=m.game_id AND p.product_id=m.product_id
		WHERE m.game_id=? AND m.provider=? AND m.provider_product_id=?`, gameID, provider, providerProductID).Scan(&pid, &g)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	return pid, parseGrants(g), err
}

var (
	errNotConfigured  = httpx.NewError(http.StatusNotFound, "provider_not_configured", "provider is not configured")
	errOtherPlayer    = httpx.NewError(http.StatusForbidden, "transaction_owned_by_other_player", "transaction belongs to another player")
	errUnknownToken   = httpx.NewError(http.StatusForbidden, "unknown_app_account_token", "transaction was not started from an order of this player")
	errTokenRequired  = httpx.BadRequest("transaction has no appAccountToken; create an order first")
	errRefunded       = httpx.Conflict("transaction_refunded", "transaction was refunded or revoked")
	errUsed           = httpx.Conflict("transaction_already_used", "transaction was already used")
	errNotMapped      = httpx.NewError(http.StatusUnprocessableEntity, "product_not_mapped", "store product is not mapped to a product of this game")
	errProductChanged = httpx.Conflict("product_mismatch", "transaction product differs from the order product")
	errAmountChanged  = httpx.Conflict("amount_mismatch", "paid amount differs from the order amount")
)

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// createOrder issues an order and its app_account_token (pass it to StoreKit as
// appAccountToken); a transaction carrying it can only be delivered to this player.
func (m *Module) createOrder(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := game.CurrentSession(ctx)
	body, err := game.ReadBody(r, 1024)
	if err != nil {
		return err
	}
	var in struct {
		ProductID string `json:"product_id"`
	}
	if err := game.DecodeStrict(body, &in); err != nil {
		return err
	}
	if !game.ValidID(in.ProductID) {
		return httpx.BadRequest("product_id is invalid")
	}
	status, resp, replayed, err := idempotency.Do(ctx, m.DB, "game:"+s.GameID+":"+s.PlayerID, r.Header.Get("Idempotency-Key"), "commerce.order", body,
		func(tx *sql.Tx) (idempotency.Result, error) {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM commerce_products WHERE game_id=? AND product_id=?`, s.GameID, in.ProductID).Scan(&n); err != nil {
				return idempotency.Result{}, err
			}
			if n == 0 {
				return idempotency.Result{}, httpx.NotFound("product not found")
			}
			ids := map[string]string{}
			rows, err := tx.QueryContext(ctx, `SELECT provider, provider_product_id FROM commerce_provider_products WHERE game_id=? AND product_id=?`, s.GameID, in.ProductID)
			if err != nil {
				return idempotency.Result{}, err
			}
			for rows.Next() {
				var p, id string
				if err := rows.Scan(&p, &id); err != nil {
					rows.Close()
					return idempotency.Result{}, err
				}
				ids[p] = id
			}
			rows.Close()
			now := m.now().UnixMilli()
			id, tok := httpx.RandomID("ord_"), newUUID()
			if _, err := tx.ExecContext(ctx, `INSERT INTO commerce_orders(order_id,game_id,player_id,provider,product_id,app_account_token,state,created_at,updated_at)
				VALUES(?,?,?,'',?,?,'created',?,?)`, id, s.GameID, s.PlayerID, in.ProductID, tok, now, now); err != nil {
				return idempotency.Result{}, err
			}
			return idempotency.Result{Status: http.StatusCreated, Body: map[string]any{
				"order_id": id, "product_id": in.ProductID, "app_account_token": tok, "provider_product_ids": ids, "state": "created"}}, nil
		})
	if err != nil {
		return game.MapWriteErr(err)
	}
	idempotency.Write(w, status, resp, replayed)
	return nil
}

// verifyPurchase verifies a client-submitted store transaction and delivers it once.
func (m *Module) verifyPurchase(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	provider := r.PathValue("provider")
	v, ok := m.IAP[provider]
	if !ok {
		return errNotConfigured
	}
	g, s := game.CurrentGame(ctx), game.CurrentSession(ctx)
	body, err := game.ReadBody(r, 128<<10)
	if err != nil {
		return err
	}
	var in struct {
		SignedTransaction string `json:"signed_transaction"`
		// Google Play: the Play product id and the purchase token from the client.
		ProductID     string `json:"product_id"`
		PurchaseToken string `json:"purchase_token"`
	}
	if err := game.DecodeStrict(body, &in); err != nil {
		return err
	}
	proof := in.SignedTransaction
	if in.PurchaseToken != "" || in.ProductID != "" {
		if proof != "" {
			return httpx.BadRequest("send either signed_transaction or product_id and purchase_token")
		}
		b, _ := json.Marshal(googleProof{ProductID: in.ProductID, PurchaseToken: in.PurchaseToken})
		proof = string(b)
	}
	if proof == "" {
		return httpx.BadRequest("signed_transaction (or product_id and purchase_token) is required")
	}
	// Verification (crypto, possibly network) happens before the transaction.
	txn, err := v.VerifyTransaction(ctx, g.ID, proof)
	if err != nil {
		return err
	}
	var o *order
	var replayed bool
	err = m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		o, replayed, err = m.clientDeliver(ctx, tx, g, s.PlayerID, provider, txn)
		return err
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"order": o.view(), "replayed": replayed})
	return nil
}

func (m *Module) clientDeliver(ctx context.Context, tx *sql.Tx, g game.Game, playerID, provider string, txn *Transaction) (*order, bool, error) {
	o, err := orderByTxn(ctx, tx, provider, txn.TransactionID)
	if err != nil {
		return nil, false, err
	}
	if o != nil {
		switch {
		case o.GameID != g.ID:
			return nil, false, errUsed
		case o.State == "refunded" && !o.DeliveredAt.Valid:
			return nil, false, errRefunded
		case o.State == "paid" && o.PlayerID == "":
			// verified by a notification but not bound yet: bind below
		case o.PlayerID != playerID:
			return nil, false, errOtherPlayer
		default:
			return o, true, nil // already delivered (or later state) to this player
		}
	}
	if txn.Revoked {
		return nil, false, errRefunded
	}
	tok, err := tokenOrder(ctx, tx, g.ID, txn.AppAccountToken)
	if err != nil {
		return nil, false, err
	}
	switch {
	case txn.AppAccountToken != "" && tok == nil:
		return nil, false, errUnknownToken
	case tok != nil && tok.PlayerID != playerID:
		return nil, false, errOtherPlayer
	case txn.AppAccountToken == "" && !m.AllowUnboundTransactions:
		return nil, false, errTokenRequired
	}
	pid, unit, err := productFor(ctx, tx, g.ID, provider, txn.ProviderProductID)
	if err != nil {
		return nil, false, err
	}
	if pid == "" {
		return nil, false, errNotMapped
	}
	if tok != nil && tok.ProductID != pid {
		return nil, false, errProductChanged
	}
	if err := amountMatches(tok, provider, txn); err != nil {
		return nil, false, err
	}
	o, err = m.deliver(ctx, tx, g, playerID, provider, txn, pid, unit, o, tok)
	return o, false, err
}

// deliver grants the product to playerID and moves the order to delivered.
// existing is a paid order for this txn; tok is the token's order (filled when still created).
func (m *Module) deliver(ctx context.Context, tx *sql.Tx, g game.Game, playerID, provider string, txn *Transaction, productID string, unit grants, existing, tok *order) (*order, error) {
	now := m.now().UnixMilli()
	gr := unit.times(txn.Quantity)
	var id string
	switch {
	case existing != nil:
		id = existing.ID
		if _, err := tx.ExecContext(ctx, `UPDATE commerce_orders SET player_id=?, product_id=?, state='delivered', grants=?, delivered_at=?, updated_at=? WHERE order_id=?`,
			playerID, productID, gr.json(), now, now, id); err != nil {
			return nil, err
		}
	case tok != nil && tok.State == "created":
		id = tok.ID
		if _, err := tx.ExecContext(ctx, `UPDATE commerce_orders SET provider=?, provider_product_id=?, provider_txn_id=?, original_txn_id=?, environment=?, quantity=?,
			state='delivered', grants=?, delivered_at=?, updated_at=? WHERE order_id=?`,
			provider, txn.ProviderProductID, txn.TransactionID, txn.OriginalTransactionID, txn.Environment, txn.Quantity, gr.json(), now, now, id); err != nil {
			return nil, err
		}
	default:
		id = httpx.RandomID("ord_")
		if _, err := tx.ExecContext(ctx, `INSERT INTO commerce_orders(order_id,game_id,player_id,provider,product_id,provider_product_id,app_account_token,provider_txn_id,original_txn_id,environment,quantity,state,grants,created_at,delivered_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,'delivered',?,?,?,?)`, id, g.ID, playerID, provider, productID, txn.ProviderProductID, nullIf(txn.AppAccountToken),
			txn.TransactionID, txn.OriginalTransactionID, txn.Environment, txn.Quantity, gr.json(), now, now, now); err != nil {
			return nil, err
		}
	}
	if err := game.ApplyLedger(ctx, tx, g.ID, playerID, "iap", "iap:"+provider+":"+txn.TransactionID, gr, now); err != nil {
		return nil, err
	}
	if err := m.enqueue(ctx, tx, g.TenantID, "delivered:"+id, OutboxEvent{Event: "delivered", GameID: g.ID, PlayerID: playerID, OrderID: id, ProductID: productID, Grants: gr, At: now}); err != nil {
		return nil, err
	}
	// Store-side completion (Google Play acknowledge/consume) runs as a job
	// committed with the delivery: no network IO inside this transaction.
	// The order records its finish state (pending -> done | needs_attention)
	// and the purchase token, so Maintain can re-enqueue a dead or missing job.
	if f, ok := m.IAP[provider].(finisher); ok && m.Outbox != nil {
		if j, ok := f.finishJob(g.ID, id, txn); ok {
			j.TenantID, j.DedupeKey = g.TenantID, "finish:"+id
			if _, err := m.Outbox.Enqueue(ctx, tx, j); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE commerce_orders SET finish_state='pending', purchase_token=? WHERE order_id=?`, txn.PurchaseToken, id); err != nil {
				return nil, err
			}
		} else if txn.Finished {
			if _, err := tx.ExecContext(ctx, `UPDATE commerce_orders SET finish_state='done', finished_at=? WHERE order_id=?`, now, id); err != nil {
				return nil, err
			}
		}
	}
	return orderByID(ctx, tx, g.ID, id)
}

// amountMatches checks the paid amount against an order whose price was fixed
// by this provider before payment (WeChat Pay prepay).
func amountMatches(tok *order, provider string, txn *Transaction) error {
	if tok != nil && tok.Provider == provider && tok.Amount > 0 && txn.Amount != tok.Amount {
		return errAmountChanged
	}
	return nil
}

func nullIf(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// iapCallback handles a store server notification (e.g. App Store Server Notifications V2).
func (m *Module) iapCallback(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	provider := r.PathValue("provider")
	v, ok := m.IAP[provider]
	if !ok {
		return errNotConfigured
	}
	g := game.CurrentGame(ctx)
	res, err := m.handleCallback(r, g, provider, v)
	if cw, ok := v.(callbackWriter); ok {
		cw.writeCallback(w, res, err)
		return nil
	}
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, res)
	return nil
}

func (m *Module) handleCallback(r *http.Request, g game.Game, provider string, v Verifier) (map[string]any, error) {
	ctx := r.Context()
	body, err := game.ReadBody(r, 256<<10)
	if err != nil {
		return nil, err
	}
	var n *Notification
	if rv, ok := v.(RequestVerifier); ok {
		n, err = rv.VerifyNotificationRequest(ctx, g.ID, r.Header, body)
	} else {
		n, err = v.VerifyNotification(ctx, g.ID, body)
	}
	if err != nil {
		return nil, err
	}
	kind := n.Kind
	return m.processEvent(ctx, g.ID, provider, n.EventID, kind, hashOf(body), func(tx *sql.Tx) (inboxState, error) {
		switch n.Kind {
		case NotifyPurchase:
			return m.notifyPurchase(ctx, tx, g, provider, n.Txn)
		case NotifyRefund:
			return m.refund(ctx, tx, g, provider, n.Txn)
		}
		reason := "notification type " + n.Type + " is not handled"
		if n.Reason != "" {
			reason = n.Reason
		}
		return inboxState{Status: "ignored", Error: reason}, nil
	})
}

// getOrder returns one of the session player's orders (e.g. to poll a WeChat
// Pay order until the payment notification delivered it).
func (m *Module) getOrder(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := game.CurrentSession(ctx)
	o, err := orderByID(ctx, m.DB.Read, s.GameID, r.PathValue("order"))
	if err != nil {
		return err
	}
	if o == nil || o.PlayerID != s.PlayerID {
		return httpx.NotFound("order not found")
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"order": o.view()})
	return nil
}

// processEvent applies a verified callback once. Duplicates of a processed or
// ignored event return the recorded outcome. A business error (httpx.Error)
// from fn rolls back fn's writes, records the event as failed (so a provider
// re-delivery is processed again) and is returned so the provider retries.
func (m *Module) processEvent(ctx context.Context, gameID, provider, eventID, kind, hash string, fn func(*sql.Tx) (inboxState, error)) (map[string]any, error) {
	var res map[string]any
	var bizErr error
	err := m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		res, bizErr = nil, nil
		prev, err := inboxGet(ctx, tx, gameID, provider, eventID)
		if err != nil {
			return err
		}
		if prev != nil && prev.Status != "failed" {
			res = map[string]any{"status": prev.Status, "duplicate": true, "reason": prev.Error, "order_id": prev.OrderID}
			return nil
		}
		now := m.now().UnixMilli()
		if _, err := tx.ExecContext(ctx, `SAVEPOINT commerce_event`); err != nil {
			return err
		}
		st, err := fn(tx)
		var he *httpx.Error
		if errors.As(err, &he) && he.Status < 500 {
			bizErr = err
			if _, err := tx.ExecContext(ctx, `ROLLBACK TO commerce_event`); err != nil {
				return err
			}
			st = inboxState{Status: "failed", Error: he.Code}
		} else if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `RELEASE commerce_event`); err != nil {
			return err
		}
		if err := inboxPut(ctx, tx, gameID, provider, eventID, kind, hash, st, now); err != nil {
			return err
		}
		res = map[string]any{"status": st.Status, "duplicate": false, "reason": st.Error, "order_id": st.OrderID}
		return nil
	})
	if err != nil {
		return nil, game.MapWriteErr(err)
	}
	if bizErr != nil {
		return nil, bizErr
	}
	return res, nil
}

// notifyPurchase records a purchase reported by the store. When the
// transaction's token identifies a player it is delivered now; otherwise the
// order waits in state paid for the client's verify call.
func (m *Module) notifyPurchase(ctx context.Context, tx *sql.Tx, g game.Game, provider string, txn *Transaction) (inboxState, error) {
	if txn.Revoked {
		return m.refund(ctx, tx, g, provider, txn)
	}
	o, err := orderByTxn(ctx, tx, provider, txn.TransactionID)
	if err != nil {
		return inboxState{}, err
	}
	if o != nil {
		if o.GameID != g.ID {
			return inboxState{Status: "ignored", Error: "transaction_already_used"}, nil
		}
		return inboxState{Status: "ignored", Error: "transaction already " + o.State, OrderID: o.ID}, nil
	}
	tok, err := tokenOrder(ctx, tx, g.ID, txn.AppAccountToken)
	if err != nil {
		return inboxState{}, err
	}
	pid, unit, err := productFor(ctx, tx, g.ID, provider, txn.ProviderProductID)
	if err != nil {
		return inboxState{}, err
	}
	if tok != nil {
		if pid == "" {
			return inboxState{}, errNotMapped
		}
		if tok.ProductID != pid {
			return inboxState{}, errProductChanged
		}
		if err := amountMatches(tok, provider, txn); err != nil {
			return inboxState{}, err
		}
		d, err := m.deliver(ctx, tx, g, tok.PlayerID, provider, txn, pid, unit, nil, tok)
		if err != nil {
			return inboxState{}, err
		}
		return inboxState{Status: "processed", OrderID: d.ID}, nil
	}
	now := m.now().UnixMilli()
	id := httpx.RandomID("ord_")
	if _, err := tx.ExecContext(ctx, `INSERT INTO commerce_orders(order_id,game_id,provider,product_id,provider_product_id,app_account_token,provider_txn_id,original_txn_id,environment,quantity,state,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,'paid',?,?)`, id, g.ID, provider, nullIf(pid), txn.ProviderProductID, nullIf(txn.AppAccountToken), txn.TransactionID,
		txn.OriginalTransactionID, txn.Environment, txn.Quantity, now, now); err != nil {
		return inboxState{}, err
	}
	return inboxState{Status: "processed", Error: "awaiting client verification", OrderID: id}, nil
}

// refund applies a store refund/revocation. A delivered order has its grants
// reversed as far as balances allow (never negative); the rest is recorded as
// outstanding with state refund_shortfall. A refund that arrives before the
// purchase was delivered records the order as refunded so it can never be
// delivered afterwards.
func (m *Module) refund(ctx context.Context, tx *sql.Tx, g game.Game, provider string, txn *Transaction) (inboxState, error) {
	now := m.now().UnixMilli()
	o, err := orderByTxn(ctx, tx, provider, txn.TransactionID)
	if err != nil {
		return inboxState{}, err
	}
	if o != nil && o.GameID != g.ID {
		return inboxState{Status: "ignored", Error: "transaction_already_used"}, nil
	}
	if o != nil {
		switch o.State {
		case "delivered":
			left, err := recoverGrants(ctx, tx, g.ID, o.PlayerID, "refund:"+provider+":"+txn.TransactionID, o.Grants, now)
			if err != nil {
				return inboxState{}, err
			}
			state, ev := "refunded", "refunded"
			if len(left) > 0 {
				state, ev = "refund_shortfall", "refund_shortfall"
			}
			if _, err := tx.ExecContext(ctx, `UPDATE commerce_orders SET state=?, outstanding=?, refund_attempts=1, refunded_at=?, updated_at=? WHERE order_id=?`,
				state, left.json(), now, now, o.ID); err != nil {
				return inboxState{}, err
			}
			if err := m.enqueue(ctx, tx, g.TenantID, "refund:"+o.ID, OutboxEvent{Event: ev, GameID: g.ID, PlayerID: o.PlayerID, OrderID: o.ID, ProductID: o.ProductID, Grants: o.Grants, Outstanding: left, At: now}); err != nil {
				return inboxState{}, err
			}
			return inboxState{Status: "processed", OrderID: o.ID}, nil
		case "paid", "created":
			if _, err := tx.ExecContext(ctx, `UPDATE commerce_orders SET state='refunded', refunded_at=?, updated_at=? WHERE order_id=?`, now, now, o.ID); err != nil {
				return inboxState{}, err
			}
			return inboxState{Status: "processed", OrderID: o.ID}, nil
		default:
			return inboxState{Status: "ignored", Error: "order already " + o.State, OrderID: o.ID}, nil
		}
	}
	// Refund before any purchase record: remember it so the transaction is never delivered.
	tok, err := tokenOrder(ctx, tx, g.ID, txn.AppAccountToken)
	if err != nil {
		return inboxState{}, err
	}
	pid, _, err := productFor(ctx, tx, g.ID, provider, txn.ProviderProductID)
	if err != nil {
		return inboxState{}, err
	}
	if tok != nil && tok.State == "created" {
		if _, err := tx.ExecContext(ctx, `UPDATE commerce_orders SET provider=?, provider_product_id=?, provider_txn_id=?, original_txn_id=?, environment=?, quantity=?,
			state='refunded', refunded_at=?, updated_at=? WHERE order_id=?`,
			provider, txn.ProviderProductID, txn.TransactionID, txn.OriginalTransactionID, txn.Environment, txn.Quantity, now, now, tok.ID); err != nil {
			return inboxState{}, err
		}
		return inboxState{Status: "processed", Error: "refunded before delivery", OrderID: tok.ID}, nil
	}
	var player any
	if tok != nil {
		player = tok.PlayerID
	}
	id := httpx.RandomID("ord_")
	if _, err := tx.ExecContext(ctx, `INSERT INTO commerce_orders(order_id,game_id,player_id,provider,product_id,provider_product_id,app_account_token,provider_txn_id,original_txn_id,environment,quantity,state,created_at,refunded_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,'refunded',?,?,?)`, id, g.ID, player, provider, nullIf(pid), txn.ProviderProductID, nullIf(txn.AppAccountToken), txn.TransactionID,
		txn.OriginalTransactionID, txn.Environment, txn.Quantity, now, now, now); err != nil {
		return inboxState{}, err
	}
	return inboxState{Status: "processed", Error: "refunded before delivery", OrderID: id}, nil
}
