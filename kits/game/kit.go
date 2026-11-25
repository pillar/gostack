// Package game is GameKit: game registry, player identity, cloud saves,
// leaderboards, wallet/ledger rewards, remote config and analytics ingestion.
//
// Isolation rule: game_id and player_id used by every query come from the
// authenticated session (or the URL game_id checked against the session), never
// from request bodies.
package game

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/core/observability"
	"github.com/pillar/gostack/modules/ratelimit"
)

var (
	idPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	slotPattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
)

// Kit wires GameKit handlers to storage.
type Kit struct {
	DB        *store.DB
	Analytics *Analytics
	Cfg       config.GameConfig
	ClientIP  *httpx.ClientIP
	Platforms map[string]PlatformVerifier
	Metrics   *observability.Registry
	Log       *slog.Logger
	Now       func() time.Time
	// SaveMerge, when set, resolves save conflicts for PUTs that send "merge": true.
	SaveMerge SaveMergeFunc
	// Limits builds the rate limiters (nil: process-local). The app passes a
	// Valkey-backed factory when rate_limit.driver is valkey.
	Limits ratelimit.Factory

	authLimit  ratelimit.Allower
	scoreLimit ratelimit.Allower
	eventLimit ratelimit.Allower
	apiKeys    auth.APIKeys
	configs    *configCache
}

// Init prepares limiters and caches; call before Routes.
func (k *Kit) Init() {
	if k.Now == nil {
		k.Now = time.Now
	}
	// Failure policies when a global limiter's backend is unavailable:
	// auth (guest/platform login per IP) and score/run submissions degrade to a
	// per-instance bucket (still bounded, still available); analytics events
	// fail open (P2 data, bounded by the analytics buffer which drops on overflow).
	k.authLimit = k.NewLimiter("game_auth", k.Cfg.AuthRatePerMinute, ratelimit.FailLocal)
	k.scoreLimit = k.NewLimiter("game_score", k.Cfg.ScoreRatePerMinute, ratelimit.FailLocal)
	k.eventLimit = k.NewLimiter("game_events", k.Cfg.AuthRatePerMinute*4, ratelimit.FailOpen)
	k.apiKeys = auth.APIKeys{DB: k.DB}
	k.configs = newConfigCache()
}

type handler = httpx.HandlerFunc

// Routes registers the GameKit API on mux.
func (k *Kit) Routes(mux *http.ServeMux) {
	g := func(pattern string, h handler) {
		method, path, _ := strings.Cut(pattern, " ")
		mux.Handle(method+" /v1/games/{game}"+path, k.cors(k.withGame(h)))
	}
	// CORS preflight for every game route.
	mux.Handle("OPTIONS /v1/games/{game}/{rest...}", k.cors(handler(func(w http.ResponseWriter, r *http.Request) error {
		w.WriteHeader(http.StatusNoContent)
		return nil
	})))
	g("POST /auth/guest", k.guestLogin)
	g("POST /auth/platform", k.platformLogin)
	g("POST /auth/bind", k.authed(k.bindPlatform))
	g("POST /auth/logout", k.authed(k.logout))
	g("GET /me", k.authed(k.me))
	g("GET /config", k.getConfig)
	g("GET /saves/{slot}", k.authed(k.getSave))
	g("PUT /saves/{slot}", k.authed(k.putSave))
	g("DELETE /saves/{slot}", k.authed(k.deleteSave))
	g("GET /saves/{slot}/revisions", k.authed(k.listSaveRevisions))
	g("POST /saves/{slot}/restore", k.authed(k.restoreSave))
	g("GET /boards/{board}", k.optionalAuth(k.getBoard))
	g("POST /boards/{board}/scores", k.authed(k.submitScore))
	g("POST /boards/{board}/runs", k.authed(k.startRun))
	g("GET /wallet", k.authed(k.getWallet))
	g("GET /inventory", k.authed(k.getInventory))
	g("POST /rewards/{reward}/claim", k.authed(k.claimReward))
	g("POST /offers/{offer}/purchase", k.authed(k.purchaseOffer))
	g("POST /events/batch", k.optionalAuth(k.eventsBatch))

	mux.Handle("POST /v1/admin/games", k.admin(k.adminCreateGame))
	mux.Handle("PUT /v1/admin/games/{game}/config", k.admin(k.adminPublishConfig))
	mux.Handle("POST /v1/admin/games/{game}/config/rollback", k.admin(k.adminRollbackConfig))
	k.HandleAdmin(mux, "POST /players/{player}/adjust", k.adminAdjust)
	k.HandleAdmin(mux, "GET /quarantine", k.adminListQuarantine)
	k.HandleAdmin(mux, "POST /quarantine/{id}/approve", k.adminReviewQuarantine("approved"))
	k.HandleAdmin(mux, "POST /quarantine/{id}/reject", k.adminReviewQuarantine("rejected"))
	k.HandleAdmin(mux, "POST /boards/{board}/seasons/{season}/settle", k.adminSettle)
	k.HandleAdmin(mux, "GET /boards/{board}/seasons/{season}/settlement", k.adminGetSettlement)
}

type gameRow struct {
	ID             string
	TenantID       string
	Status         string
	AllowedOrigins []string
}

type ctxKey int

const (
	gameKey ctxKey = iota
	sessionKey
)

type session struct {
	GameID    string
	PlayerID  string
	TokenHash string
}

func gameFrom(ctx context.Context) gameRow     { return ctx.Value(gameKey).(gameRow) }
func sessionFrom(ctx context.Context) *session { s, _ := ctx.Value(sessionKey).(*session); return s }

func (k *Kit) loadGame(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (gameRow, error) {
	var g gameRow
	var origins string
	err := q.QueryRowContext(ctx, `SELECT game_id, tenant_id, status, allowed_origins FROM games WHERE game_id=?`, id).
		Scan(&g.ID, &g.TenantID, &g.Status, &origins)
	if errors.Is(err, sql.ErrNoRows) {
		return g, httpx.NotFound("game not found")
	}
	if err != nil {
		return g, err
	}
	if err := json.Unmarshal([]byte(origins), &g.AllowedOrigins); err != nil {
		return g, err
	}
	return g, nil
}

// withGame resolves the URL game and rejects unknown or disabled games (G01).
func (k *Kit) withGame(next handler) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		id := r.PathValue("game")
		if !idPattern.MatchString(id) {
			return httpx.NotFound("game not found")
		}
		g, ok := r.Context().Value(gameKey).(gameRow)
		if !ok {
			var err error
			if g, err = k.loadGame(r.Context(), k.DB.Read, id); err != nil {
				return err
			}
		}
		if g.Status != "active" {
			return httpx.Forbidden("game is disabled")
		}
		return next(w, r.WithContext(context.WithValue(r.Context(), gameKey, g)))
	}
}

// cors allows only the game's configured origins. Requests without Origin
// (WeChat mini-games, servers) are unaffected.
func (k *Kit) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Origin")
		id := r.PathValue("game")
		g, err := k.loadGame(r.Context(), k.DB.Read, id)
		if err == nil && slices.Contains(g.AllowedOrigins, origin) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, If-None-Match")
			h.Set("Access-Control-Expose-Headers", "ETag, Retry-After, X-Request-ID, Idempotent-Replayed")
			h.Set("Access-Control-Max-Age", "600")
		}
		if err == nil {
			r = r.WithContext(context.WithValue(r.Context(), gameKey, g))
		}
		next.ServeHTTP(w, r)
	})
}

func (k *Kit) resolveSession(r *http.Request) (*session, error) {
	tok := httpx.BearerToken(r)
	if tok == "" {
		return nil, nil
	}
	g := gameFrom(r.Context())
	s := &session{TokenHash: auth.Hash(tok)}
	var exp int64
	err := k.DB.Read.QueryRowContext(r.Context(), `SELECT game_id, player_id, expires_at FROM sessions WHERE token_hash=?`, s.TokenHash).
		Scan(&s.GameID, &s.PlayerID, &exp)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && exp <= k.Now().UnixMilli()) {
		return nil, httpx.Unauthorized("session invalid or expired")
	}
	if err != nil {
		return nil, err
	}
	if s.GameID != g.ID {
		return nil, httpx.Forbidden("session belongs to another game")
	}
	return s, nil
}

func (k *Kit) authed(next handler) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		s, err := k.resolveSession(r)
		if err != nil {
			return err
		}
		if s == nil {
			return httpx.Unauthorized("missing bearer token")
		}
		return next(w, r.WithContext(context.WithValue(r.Context(), sessionKey, s)))
	}
}

func (k *Kit) optionalAuth(next handler) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		s, err := k.resolveSession(r)
		if err != nil {
			return err
		}
		if s != nil {
			r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))
		}
		return next(w, r)
	}
}

func (k *Kit) admin(next handler) http.Handler {
	return handler(func(w http.ResponseWriter, r *http.Request) error {
		key, err := k.apiKeys.Verify(r.Context(), httpx.BearerToken(r))
		if err != nil {
			return err
		}
		if !key.Has("admin") {
			return httpx.Forbidden("api key lacks admin scope")
		}
		return next(w, r.WithContext(context.WithValue(r.Context(), adminKey, key)))
	})
}

const adminKey ctxKey = 100

func (k *Kit) rateLimit(l ratelimit.Allower, key string) error {
	if ok, retry := l.Allow(key); !ok {
		if k.Metrics != nil {
			k.Metrics.Counter("game_rate_limited_total", "").Add(1)
		}
		return httpx.RateLimited(retry)
	}
	return nil
}

// readBody reads the (size-limited) request body for hashing and decoding.
func readBody(r *http.Request, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) || int64(len(b)) > limit {
		return nil, httpx.TooLarge("request body too large")
	}
	return b, err
}

func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return httpx.BadRequest("invalid JSON: " + err.Error())
	}
	if dec.More() {
		return httpx.BadRequest("trailing data after JSON value")
	}
	return nil
}
