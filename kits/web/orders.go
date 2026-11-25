package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/billing"
	"github.com/pillar/gostack/modules/idempotency"
)

// Order statuses (example state machine):
//
//	created -> paid -> fulfilled
//	created -> cancelled
//	paid | fulfilled -> refunded
const (
	OrderCreated   = "created"
	OrderPaid      = "paid"
	OrderFulfilled = "fulfilled"
	OrderCancelled = "cancelled"
	OrderRefunded  = "refunded"
)

// Order is an example order paid from a billing account.
type Order struct {
	ID            string          `json:"id"`
	Status        string          `json:"status"`
	AccountID     string          `json:"account_id"`
	Amount        int64           `json:"amount"`
	Currency      string          `json:"currency"`
	CustomerRef   string          `json:"customer_ref"`
	Items         json.RawMessage `json:"items"`
	ReservationID string          `json:"reservation_id,omitempty"`
	Version       int64           `json:"version"`
	CreatedAt     int64           `json:"created_at"`
	UpdatedAt     int64           `json:"updated_at"`
}

// Event is an append-only order event (also the webhook payload).
type Event struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	OrderID    string          `json:"order_id"`
	FromStatus string          `json:"from_status"`
	ToStatus   string          `json:"to_status"`
	CreatedAt  int64           `json:"created_at"`
	Data       json.RawMessage `json:"data"`
}

// OrderInput creates an order.
type OrderInput struct {
	AccountID   string          `json:"account_id"`
	Amount      int64           `json:"amount"`
	CustomerRef string          `json:"customer_ref"`
	Items       json.RawMessage `json:"items"`
}

const orderCols = `id,status,account_id,amount,currency,customer_ref,items,reservation_id,version,created_at,updated_at`

func scanOrder(row interface{ Scan(...any) error }) (Order, error) {
	var o Order
	var items []byte
	err := row.Scan(&o.ID, &o.Status, &o.AccountID, &o.Amount, &o.Currency, &o.CustomerRef, &items, &o.ReservationID, &o.Version, &o.CreatedAt, &o.UpdatedAt)
	o.Items = json.RawMessage(items)
	return o, err
}

func (k *Kit) order(ctx context.Context, q querier, tenant, id string) (Order, error) {
	o, err := scanOrder(q.QueryRowContext(ctx, `SELECT `+orderCols+` FROM web_orders WHERE tenant_id=? AND id=?`, tenant, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Order{}, httpx.NotFound("order not found")
	}
	return o, err
}

// GetOrder returns an order.
func (k *Kit) GetOrder(ctx context.Context, tenant, id string) (Order, error) {
	return k.order(ctx, k.DB.Read, tenant, id)
}

// ListOrders lists orders newest first.
func (k *Kit) ListOrders(ctx context.Context, tenant, status string, limit int) ([]Order, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := `SELECT ` + orderCols + ` FROM web_orders WHERE tenant_id=?`
	args := []any{tenant}
	if status != "" {
		q += ` AND status=?`
		args = append(args, status)
	}
	rows, err := k.DB.Read.QueryContext(ctx, q+` ORDER BY created_at DESC, id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OrderEvents lists an order's events in order.
func (k *Kit) OrderEvents(ctx context.Context, tenant, id string) ([]Event, error) {
	if _, err := k.order(ctx, k.DB.Read, tenant, id); err != nil {
		return nil, err
	}
	rows, err := k.DB.Read.QueryContext(ctx, `SELECT id,type,order_id,from_status,to_status,created_at,payload FROM web_events
		WHERE tenant_id=? AND order_id=? ORDER BY order_version`, tenant, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var payload []byte
		if err := rows.Scan(&e.ID, &e.Type, &e.OrderID, &e.FromStatus, &e.ToStatus, &e.CreatedAt, &payload); err != nil {
			return nil, err
		}
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(payload, &env)
		e.Data = env.Data
		out = append(out, e)
	}
	return out, rows.Err()
}

// createOrderTx validates and inserts an order with its order.created event.
func (k *Kit) createOrderTx(ctx context.Context, tx *sql.Tx, tenant, currency string, in OrderInput) (Order, error) {
	now := k.now().UnixMilli()
	o := Order{ID: httpx.RandomID("ord_"), Status: OrderCreated, AccountID: in.AccountID, Amount: in.Amount, Currency: currency,
		CustomerRef: in.CustomerRef, Items: in.Items, Version: 1, CreatedAt: now, UpdatedAt: now}
	if _, err := tx.ExecContext(ctx, `INSERT INTO web_orders(tenant_id,id,status,account_id,amount,currency,customer_ref,items,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, tenant, o.ID, o.Status, o.AccountID, o.Amount, o.Currency, o.CustomerRef, []byte(o.Items), now, now); err != nil {
		return Order{}, err
	}
	return o, k.emit(ctx, tx, tenant, "order.created", "", o, now)
}

// validateOrder checks an order request and returns the billing account currency.
func (k *Kit) validateOrder(ctx context.Context, tenant string, in *OrderInput) (string, error) {
	if !billing.ValidID(in.AccountID) {
		return "", httpx.BadRequest("account_id must be a billing account id")
	}
	if in.Amount < 1 || in.Amount > billing.MaxAmount {
		return "", httpx.BadRequest("amount must be a positive integer (minor units)")
	}
	if len(in.CustomerRef) > 128 {
		return "", httpx.BadRequest("customer_ref at most 128 bytes")
	}
	if len(in.Items) == 0 {
		in.Items = json.RawMessage(`[]`)
	}
	if len(in.Items) > 32<<10 || !json.Valid(in.Items) {
		return "", httpx.BadRequest("items must be JSON of at most 32 KiB")
	}
	acct, err := k.Billing.GetAccount(ctx, tenant, in.AccountID)
	if err != nil {
		return "", err
	}
	return acct.Currency, nil
}

func (k *Kit) transitionTx(ctx context.Context, tenant, id string, from []string, to, event string) (Order, bool, error) {
	var o Order
	changed := false
	err := k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		changed = false
		cur, err := k.order(ctx, tx, tenant, id)
		if err != nil {
			return err
		}
		o = cur
		if cur.Status == to {
			return nil
		}
		ok := false
		for _, f := range from {
			ok = ok || cur.Status == f
		}
		if !ok {
			return httpx.Conflict("invalid_transition", fmt.Sprintf("order is %s; cannot become %s", cur.Status, to))
		}
		now := k.now().UnixMilli()
		if _, err := tx.ExecContext(ctx, `UPDATE web_orders SET status=?, version=version+1, updated_at=? WHERE tenant_id=? AND id=? AND status=?`,
			to, now, tenant, id, cur.Status); err != nil {
			return err
		}
		o.Status, o.Version, o.UpdatedAt = to, cur.Version+1, now
		changed = true
		return k.emit(ctx, tx, tenant, event, cur.Status, o, now)
	})
	return o, changed, err
}

func billingKey(id, step string) string { return "web-order:" + id + ":" + step }

func refundNote(id string) string { return "web order " + id + " refund" }

func code(err error) string {
	var e *httpx.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// Pay charges the order amount: billing reserve, then settle (both keyed by
// the order, so retries never charge twice), then created -> paid. If the
// order was cancelled meanwhile, the charge is refunded. Paying a paid or
// fulfilled order is a no-op. If a previous attempt stalled past the
// reservation TTL, the expired reservation is retired and the payment is
// reserved again under the next payment generation's keys.
func (k *Kit) Pay(ctx context.Context, tenant, id string) (Order, error) {
	o, err := k.order(ctx, k.DB.Read, tenant, id)
	if err != nil {
		return Order{}, err
	}
	switch o.Status {
	case OrderPaid, OrderFulfilled:
		return o, nil
	case OrderCreated:
	default:
		return Order{}, httpx.Conflict("invalid_transition", "order is "+o.Status+"; cannot become paid")
	}
	for attempt := 1; ; attempt++ {
		var gen int64
		if err := k.DB.Read.QueryRowContext(ctx, `SELECT pay_generation FROM web_orders WHERE tenant_id=? AND id=?`, tenant, id).Scan(&gen); err != nil {
			return Order{}, err
		}
		res, err := k.Billing.Reserve(ctx, tenant, payKey(id, "reserve", gen), o.AccountID, o.Amount, 0, "web order "+id)
		if err != nil {
			return Order{}, err
		}
		rsv := res.Reservation.ID
		if err := k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE web_orders SET reservation_id=? WHERE tenant_id=? AND id=? AND reservation_id='' AND pay_generation=?`,
				rsv, tenant, id, gen)
			return err
		}); err != nil {
			return Order{}, err
		}
		_, err = k.Billing.Settle(ctx, tenant, payKey(id, "settle", gen), rsv, o.Amount, "web order "+id)
		if err == nil {
			break
		}
		if c := code(err); c != "invalid_state" && c != "reservation_expired" {
			return Order{}, err
		}
		// The reservation is no longer settleable: released by a concurrent
		// cancel, or expired because this payment stalled past its TTL (a
		// retry with the same keys would replay the dead reservation forever).
		cur, gerr := k.order(ctx, k.DB.Read, tenant, id)
		if gerr != nil {
			return Order{}, gerr
		}
		switch cur.Status {
		case OrderCancelled:
			return Order{}, httpx.Conflict("invalid_transition", "order was cancelled during payment")
		case OrderPaid, OrderFulfilled:
			return cur, nil // a concurrent Pay completed it
		case OrderCreated:
		default:
			return Order{}, httpx.Conflict("invalid_transition", "order is "+cur.Status+"; cannot become paid")
		}
		if attempt >= 3 {
			return Order{}, err
		}
		if err := k.renewPayment(ctx, tenant, cur, rsv, gen); err != nil {
			return Order{}, err
		}
	}
	paid, _, err := k.transitionTx(ctx, tenant, id, []string{OrderCreated}, OrderPaid, "order.paid")
	if code(err) == "invalid_transition" {
		cur, gerr := k.order(ctx, k.DB.Read, tenant, id)
		if gerr == nil && cur.Status == OrderFulfilled {
			return cur, nil
		}
		if gerr == nil && cur.Status == OrderCancelled {
			if cerr := k.compensate(ctx, tenant, cur); cerr != nil {
				return Order{}, cerr
			}
		}
	}
	return paid, err
}

// payKey is the billing idempotency key of a payment step. Generation 0 keeps
// the original keys; later generations (after an expired reservation) get a
// suffix so they reserve and settle anew instead of replaying.
func payKey(id, step string, gen int64) string {
	if gen == 0 {
		return billingKey(id, step)
	}
	return billingKey(id, step+":"+strconv.FormatInt(gen, 10))
}

// renewPayment retires the unsettleable reservation rsv of payment generation
// gen of a still-created order: a reservation that expired but was not yet
// swept is released (its funds come back now), then the order moves to the
// next generation with no reservation. A concurrent renewal is harmless (the
// generation only moves once from gen).
func (k *Kit) renewPayment(ctx context.Context, tenant string, o Order, rsv string, gen int64) error {
	r, err := k.Billing.GetReservation(ctx, tenant, rsv)
	if err != nil {
		return err
	}
	if r.Status == billing.StatusReserved {
		_, err := k.Billing.Release(ctx, tenant, payKey(o.ID, "release-expired", gen), rsv, "web order "+o.ID+": reservation expired before settlement")
		if err != nil && code(err) != "invalid_state" {
			return err
		}
		if r, err = k.Billing.GetReservation(ctx, tenant, rsv); err != nil {
			return err
		}
	}
	switch r.Status {
	case billing.StatusSettled:
		return nil // settled concurrently with this generation's key: the retry replays it
	case billing.StatusReleased, billing.StatusExpired:
	default:
		return httpx.Conflict("invalid_state", "order reservation is "+r.Status)
	}
	return k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE web_orders SET pay_generation=pay_generation+1, reservation_id='', updated_at=?
			WHERE tenant_id=? AND id=? AND status=? AND pay_generation=? AND reservation_id IN ('', ?)`,
			k.now().UnixMilli(), tenant, o.ID, OrderCreated, gen, rsv)
		return err
	})
}

// compensate returns money taken for an order that ends without delivery:
// a held reservation is released, a settled one is refunded by a credit with
// the order's refund key (shared with Refund, so it happens at most once).
func (k *Kit) compensate(ctx context.Context, tenant string, o Order) error {
	cur, err := k.order(ctx, k.DB.Read, tenant, o.ID)
	if err != nil {
		return err
	}
	if cur.ReservationID == "" {
		return nil
	}
	for i := 0; i < 2; i++ {
		r, err := k.Billing.GetReservation(ctx, tenant, cur.ReservationID)
		if err != nil {
			return err
		}
		switch r.Status {
		case billing.StatusReserved:
			_, err := k.Billing.Release(ctx, tenant, billingKey(o.ID, "release"), r.ID, "web order "+o.ID+" cancelled")
			if code(err) == "invalid_state" {
				continue // settled concurrently: refund instead
			}
			return err
		case billing.StatusSettled:
			_, err := k.Billing.Credit(ctx, tenant, billingKey(o.ID, "refund"), cur.AccountID, cur.Amount, refundNote(o.ID))
			return err
		default:
			return nil // released or expired: nothing was charged
		}
	}
	return nil
}

// Fulfill: paid -> fulfilled (no-op when already fulfilled).
func (k *Kit) Fulfill(ctx context.Context, tenant, id string) (Order, error) {
	o, _, err := k.transitionTx(ctx, tenant, id, []string{OrderPaid}, OrderFulfilled, "order.fulfilled")
	return o, err
}

// Cancel: created -> cancelled, then compensation of any payment in flight.
// Repeating it re-runs the (idempotent) compensation.
func (k *Kit) Cancel(ctx context.Context, tenant, id string) (Order, error) {
	o, _, err := k.transitionTx(ctx, tenant, id, []string{OrderCreated}, OrderCancelled, "order.cancelled")
	if err != nil {
		return Order{}, err
	}
	return o, k.compensate(ctx, tenant, o)
}

// Refund: paid|fulfilled -> refunded, then a billing credit with the order's
// refund key. Repeating it re-runs the (idempotent) credit.
func (k *Kit) Refund(ctx context.Context, tenant, id string) (Order, error) {
	o, _, err := k.transitionTx(ctx, tenant, id, []string{OrderPaid, OrderFulfilled}, OrderRefunded, "order.refunded")
	if err != nil {
		return Order{}, err
	}
	_, err = k.Billing.Credit(ctx, tenant, billingKey(id, "refund"), o.AccountID, o.Amount, refundNote(id))
	return o, err
}

func (k *Kit) httpCreateOrder(w http.ResponseWriter, r *http.Request, a actor) error {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return httpx.BadRequest("Idempotency-Key header is required")
	}
	var in OrderInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	currency, err := k.validateOrder(r.Context(), a.Tenant, &in)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(in)
	status, resp, replayed, err := idempotency.Do(r.Context(), k.DB, "web:"+a.Tenant, key, "create_order", body,
		func(tx *sql.Tx) (idempotency.Result, error) {
			o, err := k.createOrderTx(r.Context(), tx, a.Tenant, currency, in)
			if err != nil {
				return idempotency.Result{}, err
			}
			return idempotency.Result{Status: http.StatusCreated, Body: o}, nil
		})
	if err != nil {
		return err
	}
	idempotency.Write(w, status, resp, replayed)
	return nil
}

func (k *Kit) httpListOrders(w http.ResponseWriter, r *http.Request, a actor) error {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	os, err := k.ListOrders(r.Context(), a.Tenant, r.URL.Query().Get("status"), limit)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"orders": os})
	return nil
}

func (k *Kit) httpGetOrder(w http.ResponseWriter, r *http.Request, a actor) error {
	o, err := k.GetOrder(r.Context(), a.Tenant, r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, o)
	return nil
}

func (k *Kit) httpOrderEvents(w http.ResponseWriter, r *http.Request, a actor) error {
	es, err := k.OrderEvents(r.Context(), a.Tenant, r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"events": es})
	return nil
}

func (k *Kit) transitionHandler(fn func(ctx context.Context, tenant, id string) (Order, error)) func(w http.ResponseWriter, r *http.Request, a actor) error {
	return func(w http.ResponseWriter, r *http.Request, a actor) error {
		o, err := fn(r.Context(), a.Tenant, r.PathValue("id"))
		if err != nil {
			return err
		}
		httpx.WriteJSON(w, http.StatusOK, o)
		return nil
	}
}
