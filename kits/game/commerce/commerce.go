// Package commerce is GameKit G11: IAP server-side verification (App Store,
// Google Play, WeChat Pay API v3 JSAPI), rewarded-ad server-to-server callbacks (AdMob SSV, generic HMAC), refunds with
// shortfall tracking, and an outbox hook for delivery side effects.
//
// Provider proofs are verified before any database transaction; a short WriteTx
// then applies the inbox record, the order state machine and ApplyLedger.
// Callbacks are deduplicated by (game, provider, event_id) and store
// transactions by (provider, transaction id), so duplicates and reordering
// never grant twice.
package commerce

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/modules/jobs"
)

// Implemented providers; configuration must reject anything else.
var (
	IAPProviders = []string{"apple", "google", "wechatpay"}
	AdKinds      = []string{"admob", "hmac"}
)

// ValidateProviders rejects IAP providers and ad verifier kinds that are not
// implemented (WeChat mini-game virtual payment, Huawei, ... are not).
func ValidateProviders(iap, adKinds []string) error {
	for _, p := range iap {
		if !slices.Contains(IAPProviders, p) {
			return fmt.Errorf("commerce: iap provider %q is not implemented (supported: %v)", p, IAPProviders)
		}
	}
	for _, k := range adKinds {
		if !slices.Contains(AdKinds, k) {
			return fmt.Errorf("commerce: ad verifier %q is not implemented (supported: %v)", k, AdKinds)
		}
	}
	return nil
}

// Module serves the commerce API.
type Module struct {
	Kit *game.Kit
	DB  *store.DB
	Now func() time.Time

	// IAP verifiers by provider name ("apple").
	IAP map[string]Verifier
	// Ads verifiers by ad network name used in the callback URL.
	Ads map[string]AdVerifier
	// AllowUnboundTransactions lets a verified transaction without an
	// appAccountToken be bound to the first player who submits it. Off by
	// default: transactions must carry a token issued by POST /iap/orders.
	AllowUnboundTransactions bool
	// AdIntentTTL is how long an ad intent token can be credited (default 24h).
	AdIntentTTL time.Duration
	// Outbox, when set, receives a job of OutboxJobType (default
	// "commerce.notify") in the same transaction as every delivery and refund.
	Outbox        *jobs.Queue
	OutboxJobType string
}

func (m *Module) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Routes registers client, callback and admin routes.
func (m *Module) Routes(mux *http.ServeMux) {
	k := m.Kit
	k.Handle(mux, "POST /iap/orders", game.Player, m.createOrder)
	k.Handle(mux, "GET /iap/orders/{order}", game.Player, m.getOrder)
	k.Handle(mux, "POST /iap/wechatpay/prepay", game.Player, m.wechatPrepay)
	k.Handle(mux, "POST /iap/{provider}/verify", game.Player, m.verifyPurchase)
	k.Handle(mux, "POST /ads/{placement}/intent", game.Player, m.adIntent)
	// Signed provider callbacks are exempt from per-game request quotas.
	k.HandleCallback(mux, "POST /callbacks/{provider}", m.iapCallback)
	k.HandleCallback(mux, "GET /callbacks/ads/{provider}", m.adCallback)
	k.HandleCallback(mux, "POST /callbacks/ads/{provider}", m.adCallback)

	k.HandleAdmin(mux, "PUT /products", m.adminPutProducts)
	k.HandleAdmin(mux, "GET /products", m.adminGetProducts)
	k.HandleAdmin(mux, "PUT /ad-placements", m.adminPutPlacements)
	k.HandleAdmin(mux, "GET /ad-placements", m.adminGetPlacements)
	k.HandleAdmin(mux, "GET /orders", m.adminListOrders)
	k.HandleAdmin(mux, "POST /orders/{order}/resolve", m.adminResolveOrder)
}

// JobHandlers returns the handlers for jobs the module enqueues itself (store
// acknowledgements such as Google Play acknowledge/consume). The application's
// jobs.Worker must run them; OutboxJobType is handled by the application.
func (m *Module) JobHandlers() map[string]jobs.Handler {
	out := map[string]jobs.Handler{}
	for provider, v := range m.IAP {
		if f, ok := v.(finisher); ok {
			out[f.finishJobType()] = m.finishHandler(provider, f)
		}
	}
	return out
}

func (m *Module) log() *slog.Logger {
	if m.Kit != nil && m.Kit.Log != nil {
		return m.Kit.Log
	}
	return slog.Default()
}

func (m *Module) counter(name, labels string) {
	if m.Kit != nil && m.Kit.Metrics != nil {
		m.Kit.Metrics.Counter(name, labels).Add(1)
	}
}

// finishHandler runs a store finish job: the store call (no transaction),
// then a short WriteTx recording the outcome on the order. A rejection that
// needs attention is logged at error level, counted in
// commerce_store_finish_attention_total{provider,status}, flagged on the order
// (finish_state needs_attention, visible in the admin order list) and retried.
func (m *Module) finishHandler(provider string, f finisher) jobs.Handler {
	return func(ctx context.Context, job *jobs.Claimed) error {
		r := f.finish(ctx, job.Payload)
		if r.Attention != "" {
			m.log().Error("commerce: store rejected purchase finish; needs attention", "provider", provider, "game", r.GameID,
				"order", r.OrderID, "job", job.ID, "attempt", job.Attempt, "err", r.Attention)
			m.counter("commerce_store_finish_attention_total", `provider="`+provider+`",status="`+strconv.Itoa(r.Status)+`"`)
		}
		if r.OrderID != "" && (r.Done || r.Attention != "") {
			now := m.now().UnixMilli()
			err := m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
				var err error
				if r.Done {
					_, err = tx.ExecContext(ctx, `UPDATE commerce_orders SET finish_state='done', finish_error='', finished_at=?
						WHERE game_id=? AND order_id=? AND finish_state<>'done'`, now, r.GameID, r.OrderID)
				} else {
					_, err = tx.ExecContext(ctx, `UPDATE commerce_orders SET finish_state='needs_attention', finish_error=?, updated_at=?
						WHERE game_id=? AND order_id=? AND finish_state<>'done'`, r.Attention, now, r.GameID, r.OrderID)
				}
				return err
			})
			if err != nil {
				return err // the store call is idempotent; retry records it
			}
		}
		return r.Err
	}
}

// Maintain purges expired ad intents and inbox records older than 90 days
// (orders and ad reward rows keep the permanent uniqueness guarantees), then
// reconciles store-side work that has no live job (reconcileFinish) and, for
// WeChat Pay, queries unpaid orders (reconcileWeChatPay).
func (m *Module) Maintain(ctx context.Context) error {
	now := m.now()
	err := m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM commerce_ad_intents WHERE expires_at < ?`, now.Add(-24*time.Hour).UnixMilli()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM commerce_inbox WHERE received_at < ? AND status <> 'failed'`, now.Add(-90*24*time.Hour).UnixMilli())
		return err
	})
	return errors.Join(err, m.reconcileFinish(ctx), m.reconcileWeChatPay(ctx))
}

// reconcileFinish re-enqueues the finish job of every delivered order of a
// finisher provider whose completion is not recorded as done (pending or
// needs_attention), delivered within googleFinishWindow, and whose job is dead
// (Replay: fresh attempt budget) or missing (re-created from the stored
// purchase token). Bounded to 500 orders per run.
func (m *Module) reconcileFinish(ctx context.Context) error {
	if m.Outbox == nil {
		return nil
	}
	type cand struct{ id, gameID, tenant, provider, ppid, token string }
	rows, err := m.DB.Read.QueryContext(ctx, `SELECT o.order_id, o.game_id, g.tenant_id, o.provider, coalesce(o.provider_product_id,''), coalesce(o.purchase_token,'')
		FROM commerce_orders o JOIN games g ON g.game_id=o.game_id
		WHERE o.finish_state IN ('pending','needs_attention') AND o.state='delivered' AND o.delivered_at>=?
		ORDER BY o.delivered_at, o.order_id LIMIT 500`, m.now().Add(-googleFinishWindow).UnixMilli())
	if err != nil {
		return err
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.gameID, &c.tenant, &c.provider, &c.ppid, &c.token); err != nil {
			rows.Close()
			return err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var errs []error
	for _, c := range cands {
		f, ok := m.IAP[c.provider].(finisher)
		if !ok {
			continue
		}
		var jobID, status string
		err := m.DB.Read.QueryRowContext(ctx, `SELECT id, status FROM jobs WHERE tenant_id=? AND type=? AND dedupe_key=?`,
			c.tenant, f.finishJobType(), "finish:"+c.id).Scan(&jobID, &status)
		switch {
		case err == nil && status != "dead":
			continue // live (or succeeded: the handler records done)
		case err == nil:
			err = m.Outbox.Replay(ctx, jobID)
		case errors.Is(err, sql.ErrNoRows):
			j, ok := f.finishJob(c.gameID, c.id, &Transaction{ProviderProductID: c.ppid, PurchaseToken: c.token})
			if !ok {
				m.log().Error("commerce: cannot re-create finish job (no purchase token or app)", "provider", c.provider, "order", c.id)
				continue
			}
			j.TenantID, j.DedupeKey = c.tenant, "finish:"+c.id
			_, err = m.Outbox.EnqueueNow(ctx, j)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("commerce: reconcile finish of %s: %w", c.id, err))
			continue
		}
		m.log().Warn("commerce: re-enqueued store finish job", "provider", c.provider, "game", c.gameID, "order", c.id, "job_was", status)
		m.counter("commerce_store_finish_requeued_total", `provider="`+c.provider+`"`)
	}
	return errors.Join(errs...)
}

// --- inbox -----------------------------------------------------------------

type inboxState struct {
	Status  string
	Error   string
	OrderID string
}

// inboxGet returns the recorded state of an event (nil when unseen).
func inboxGet(ctx context.Context, tx *sql.Tx, gameID, provider, eventID string) (*inboxState, error) {
	var s inboxState
	var oid sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT status, error, order_id FROM commerce_inbox WHERE game_id=? AND provider=? AND event_id=?`,
		gameID, provider, eventID).Scan(&s.Status, &s.Error, &oid)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	s.OrderID = oid.String
	return &s, err
}

func inboxPut(ctx context.Context, tx *sql.Tx, gameID, provider, eventID, kind, hash string, s inboxState, now int64) error {
	var oid any
	if s.OrderID != "" {
		oid = s.OrderID
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO commerce_inbox(game_id,provider,event_id,kind,payload_hash,status,error,order_id,received_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(game_id,provider,event_id) DO UPDATE SET status=excluded.status, error=excluded.error,
		order_id=excluded.order_id, payload_hash=excluded.payload_hash, updated_at=excluded.updated_at`,
		gameID, provider, eventID, kind, hash, s.Status, s.Error, oid, now, now)
	return err
}

func hashOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// --- grants ----------------------------------------------------------------

type grants map[string]int64

func (g grants) validate(what string) error {
	if len(g) == 0 || len(g) > 32 {
		return httpx.BadRequest(what + ": grants must have 1-32 assets")
	}
	for a, n := range g {
		if !game.ValidID(a) || n <= 0 || n > 1e12 {
			return httpx.BadRequest(what + ": grants must map asset ids to positive integers")
		}
	}
	return nil
}

func (g grants) times(q int) grants {
	out := grants{}
	for a, n := range g {
		out[a] = n * int64(q)
	}
	return out
}

func (g grants) neg() map[string]int64 {
	out := map[string]int64{}
	for a, n := range g {
		out[a] = -n
	}
	return out
}

func (g grants) json() string {
	b, _ := json.Marshal(map[string]int64(g))
	return string(b)
}

func parseGrants(s string) grants {
	g := grants{}
	_ = json.Unmarshal([]byte(s), &g)
	return g
}

func balanceOf(ctx context.Context, tx *sql.Tx, gameID, playerID, asset string) (int64, error) {
	var b int64
	err := tx.QueryRowContext(ctx, `SELECT balance FROM wallet_balances WHERE game_id=? AND player_id=? AND asset=?`, gameID, playerID, asset).Scan(&b)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return b, err
}

// recover deducts up to want from the player's balances without going negative
// and returns what is still outstanding.
func recoverGrants(ctx context.Context, tx *sql.Tx, gameID, playerID, sourceKey string, want grants, now int64) (grants, error) {
	take, left := map[string]int64{}, grants{}
	assets := make([]string, 0, len(want))
	for a := range want {
		assets = append(assets, a)
	}
	sort.Strings(assets)
	for _, a := range assets {
		bal, err := balanceOf(ctx, tx, gameID, playerID, a)
		if err != nil {
			return nil, err
		}
		t := min(bal, want[a])
		if t > 0 {
			take[a] = -t
		}
		if want[a]-t > 0 {
			left[a] = want[a] - t
		}
	}
	if err := game.ApplyLedger(ctx, tx, gameID, playerID, "refund", sourceKey, take, now); err != nil {
		return nil, err
	}
	return left, nil
}

// --- outbox ----------------------------------------------------------------

// OutboxEvent is the JSON payload of outbox jobs.
type OutboxEvent struct {
	Event       string           `json:"event"` // delivered | refunded | refund_shortfall | refund_compensated | ad_reward
	GameID      string           `json:"game_id"`
	PlayerID    string           `json:"player_id"`
	OrderID     string           `json:"order_id,omitempty"`
	ProductID   string           `json:"product_id,omitempty"`
	PlacementID string           `json:"placement_id,omitempty"`
	Grants      map[string]int64 `json:"grants,omitempty"`
	Outstanding map[string]int64 `json:"outstanding,omitempty"`
	At          int64            `json:"at"`
}

func (m *Module) enqueue(ctx context.Context, tx *sql.Tx, tenant, dedupe string, ev OutboxEvent) error {
	if m.Outbox == nil {
		return nil
	}
	typ := m.OutboxJobType
	if typ == "" {
		typ = "commerce.notify"
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = m.Outbox.Enqueue(ctx, tx, jobs.Job{TenantID: tenant, Type: typ, Payload: b, DedupeKey: dedupe})
	return err
}
