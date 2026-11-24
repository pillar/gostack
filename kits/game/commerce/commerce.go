// Package commerce is GameKit G11: IAP server-side verification (App Store),
// rewarded-ad server-to-server callbacks (AdMob SSV, generic HMAC), refunds with
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
	"fmt"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/modules/jobs"
)

// Implemented providers; configuration must reject anything else.
var (
	IAPProviders = []string{"apple"}
	AdKinds      = []string{"admob", "hmac"}
)

// ValidateProviders rejects IAP providers and ad verifier kinds that are not
// implemented (Google Play, WeChat Pay, ... are not).
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
	k.Handle(mux, "POST /iap/{provider}/verify", game.Player, m.verifyPurchase)
	k.Handle(mux, "POST /callbacks/{provider}", game.Public, m.iapCallback)
	k.Handle(mux, "POST /ads/{placement}/intent", game.Player, m.adIntent)
	k.Handle(mux, "GET /callbacks/ads/{provider}", game.Public, m.adCallback)
	k.Handle(mux, "POST /callbacks/ads/{provider}", game.Public, m.adCallback)

	k.HandleAdmin(mux, "PUT /products", m.adminPutProducts)
	k.HandleAdmin(mux, "GET /products", m.adminGetProducts)
	k.HandleAdmin(mux, "PUT /ad-placements", m.adminPutPlacements)
	k.HandleAdmin(mux, "GET /ad-placements", m.adminGetPlacements)
	k.HandleAdmin(mux, "GET /orders", m.adminListOrders)
	k.HandleAdmin(mux, "POST /orders/{order}/resolve", m.adminResolveOrder)
}

// Maintain purges expired ad intents and inbox records older than 90 days
// (orders and ad reward rows keep the permanent uniqueness guarantees).
func (m *Module) Maintain(ctx context.Context) error {
	now := m.now()
	return m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM commerce_ad_intents WHERE expires_at < ?`, now.Add(-24*time.Hour).UnixMilli()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM commerce_inbox WHERE received_at < ? AND status <> 'failed'`, now.Add(-90*24*time.Hour).UnixMilli())
		return err
	})
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
