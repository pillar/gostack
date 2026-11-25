package game

import (
	"context"
	"database/sql"
	"net/http"
	"strings"

	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/ratelimit"
)

// Extension API for GameKit feature packages (kits/game/...). Feature packages
// register their routes through Handle/HandleAdmin so they inherit game
// resolution, CORS, session authentication and admin tenant checks, and read
// identity only from CurrentGame/CurrentSession (never from request bodies).

// AuthMode selects how a game route authenticates the caller.
type AuthMode int

const (
	Public         AuthMode = iota // no session required (session ignored)
	Player                         // valid session for this game required
	OptionalPlayer                 // session used when present and valid
)

// Game is the resolved game of the current request.
type Game struct {
	ID       string
	TenantID string
}

// Session is the authenticated player of the current request.
type Session struct {
	GameID   string
	PlayerID string
}

// Handle registers pattern ("METHOD /path") under /v1/games/{game}.
func (k *Kit) Handle(mux *http.ServeMux, pattern string, mode AuthMode, h httpx.HandlerFunc) {
	method, path, _ := strings.Cut(pattern, " ")
	switch mode {
	case Player:
		h = k.authed(h)
	case OptionalPlayer:
		h = k.optionalAuth(h)
	}
	mux.Handle(method+" /v1/games/{game}"+path, k.cors(k.withGame(h)))
}

// HandleAdmin registers pattern ("METHOD /path") under /v1/admin/games/{game}.
// The caller needs an API key with the admin scope whose tenant owns the game.
func (k *Kit) HandleAdmin(mux *http.ServeMux, pattern string, h httpx.HandlerFunc) {
	method, path, _ := strings.Cut(pattern, " ")
	mux.Handle(method+" /v1/admin/games/{game}"+path, k.admin(func(w http.ResponseWriter, r *http.Request) error {
		g, _, err := k.adminGame(r)
		if err != nil {
			return err
		}
		return h(w, r.WithContext(context.WithValue(r.Context(), gameKey, g)))
	}))
}

// CurrentGame returns the game resolved for this request.
func CurrentGame(ctx context.Context) Game {
	g := gameFrom(ctx)
	return Game{ID: g.ID, TenantID: g.TenantID}
}

// CurrentSession returns the authenticated player, or nil.
func CurrentSession(ctx context.Context) *Session {
	s := sessionFrom(ctx)
	if s == nil {
		return nil
	}
	return &Session{GameID: s.GameID, PlayerID: s.PlayerID}
}

// AdminKey returns the API key of an admin request registered with HandleAdmin.
func AdminKey(ctx context.Context) auth.APIKey {
	k, _ := ctx.Value(adminKey).(auth.APIKey)
	return k
}

// ValidID reports whether s is a valid game-scoped identifier ([a-z0-9][a-z0-9_-]{0,31}).
func ValidID(s string) bool { return idPattern.MatchString(s) }

// ReadBody reads at most limit bytes of the request body (413 when larger).
func ReadBody(r *http.Request, limit int64) ([]byte, error) { return readBody(r, limit) }

// DecodeStrict decodes JSON rejecting unknown fields and trailing data (400).
func DecodeStrict(b []byte, v any) error { return decodeStrict(b, v) }

// MapWriteErr converts write-queue saturation into 503 overloaded.
func MapWriteErr(err error) error { return mapWriteErr(err) }

// RateLimit applies l to key, returning 429 with Retry-After when exhausted.
func (k *Kit) RateLimit(l *ratelimit.Limiter, key string) error { return k.rateLimit(l, key) }

// CurrentDocument returns the game's current published configuration and its
// revision (0 and an empty document when none is published). q may be a
// transaction so the revision read is the one applied.
func (k *Kit) CurrentDocument(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, gameID string) (Document, int64, error) {
	c, err := k.currentConfig(ctx, q, gameID)
	if err != nil {
		return Document{}, 0, err
	}
	return c.Doc, c.Revision, nil
}

// EnsurePlayer reports whether the player exists in the game (inside tx or a read pool).
func EnsurePlayer(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, gameID, playerID string) error {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM players WHERE game_id=? AND player_id=?`, gameID, playerID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return httpx.NotFound("player not found")
	}
	return nil
}

// SetRates retunes the GameKit rate limiters (config hot reload): auth per IP,
// score/run per player, and analytics events per IP (4x auth).
func (k *Kit) SetRates(authPerMinute, scorePerMinute int) {
	k.authLimit.SetPerMinute(authPerMinute)
	k.scoreLimit.SetPerMinute(scorePerMinute)
	k.eventLimit.SetPerMinute(authPerMinute * 4)
}

// Grant posts player-claimed grants (mail, check-in, tasks, redeem codes,
// invites) through ApplyLedger and enforces the game's items catalog like reward
// claims do: when a catalog exists every granted asset must be in it (409
// unknown_asset), and items may not exceed max_stack (409 stack_limit). It reads
// the configuration inside tx. Server-authoritative grants for purchases, ad
// callbacks and match results use ApplyLedger directly and are not capped,
// because failing them would strand a paid or externally confirmed event.
func (k *Kit) Grant(ctx context.Context, tx *sql.Tx, gameID, playerID, kind, sourceKey string, deltas map[string]int64, now int64) error {
	c, err := k.currentConfig(ctx, tx, gameID)
	if err != nil {
		return err
	}
	if len(c.Doc.Items) > 0 {
		for asset := range deltas {
			if _, ok := c.Doc.Items[asset]; !ok {
				e := httpx.Conflict("unknown_asset", "granted asset is not in the items catalog")
				e.Details = map[string]any{"asset": asset}
				return e
			}
		}
	}
	if err := ApplyLedger(ctx, tx, gameID, playerID, kind, sourceKey, deltas, now); err != nil {
		return err
	}
	return checkStacks(ctx, tx, c.Doc, gameID, playerID, deltas)
}
