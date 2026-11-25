// Package realtime is the GameKit WebSocket gateway (G14): authenticated
// per-player connections with heartbeats, per-player message sequence numbers,
// bounded replay for reconnects and backpressure.
//
// Without a bus (profile lite) the Hub keeps connections, sequence numbers and
// replay buffers in process memory and nothing is written per message.
//
// With a PostgresBus (profile scale, game.realtime.bus: postgres) several
// instances share one message stream per player: PostgreSQL assigns every seq
// (realtime_streams), keeps the replay window (realtime_messages) and fans
// messages out with LISTEN/NOTIFY to every instance, which delivers them to its
// own connections. See docs/adr/0005-realtime-scale.md.
package realtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/core/observability"
	"github.com/pillar/gostack/kits/game"
)

// Options is the gateway's resource budget, independent of the HTTP request
// limits (connections do not hold an HTTP concurrency slot or request deadline).
// Zero values take the defaults noted on each field.
type Options struct {
	MaxConnections  int           // total connections incl. unauthenticated (4000)
	MaxPerPlayer    int           // per player; a new connection replaces the oldest (2)
	MaxMessageBytes int64         // inbound message size; larger closes with 1009 (4096)
	InboundRate     float64       // inbound messages per second per connection (10)
	InboundBurst    int           // inbound burst per connection (20)
	SendQueue       int           // max undelivered live messages per connection before 4008 (32)
	MaxPayloadBytes int           // outbound payload size accepted by Send/Broadcast (4096)
	ReplayBuffer    int           // per-player replay buffer, messages (64)
	ReplayMaxBytes  int           // per-player replay buffer, bytes (16384)
	ReplayMaxAge    time.Duration // replay entries and idle player streams expire after (2m)
	MaxStreams      int           // player streams (buffers) kept in memory (4000)
	PingInterval    time.Duration // server ping interval (25s)
	PongTimeout     time.Duration // pong wait; dead peers are dropped (10s)
	AuthTimeout     time.Duration // time to send the auth message (5s)
	WriteTimeout    time.Duration // per-frame write deadline (5s)
	SessionRecheck  time.Duration // re-validate the session while connected (1m)
	MessageTimeout  time.Duration // OnMessage handler deadline (5s)
	BusQueue        int           // bus only: Send/Broadcast calls queued for the publisher (4096)
}

func (o Options) withDefaults() Options {
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	dur := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&o.MaxConnections, 4000)
	def(&o.MaxPerPlayer, 2)
	if o.MaxMessageBytes <= 0 {
		o.MaxMessageBytes = 4096
	}
	if o.InboundRate <= 0 {
		o.InboundRate = 10
	}
	def(&o.InboundBurst, 20)
	def(&o.SendQueue, 32)
	def(&o.MaxPayloadBytes, 4096)
	def(&o.ReplayBuffer, 64)
	def(&o.ReplayMaxBytes, 16384)
	def(&o.MaxStreams, 4000)
	def(&o.BusQueue, 4096)
	dur(&o.ReplayMaxAge, 2*time.Minute)
	dur(&o.PingInterval, 25*time.Second)
	dur(&o.PongTimeout, 10*time.Second)
	dur(&o.AuthTimeout, 5*time.Second)
	dur(&o.WriteTimeout, 5*time.Second)
	dur(&o.SessionRecheck, time.Minute)
	dur(&o.MessageTimeout, 5*time.Second)
	if o.SendQueue > o.ReplayBuffer {
		o.SendQueue = o.ReplayBuffer
	}
	return o
}

// Message is an application message received from a client.
type Message struct {
	Type string          // client-chosen type ([a-z][a-z0-9_.:-]{0,63}, not reserved)
	ID   string          // optional client correlation id, echoed as "ref"
	Data json.RawMessage // raw "data" field (may be empty)
}

// Handler processes an application message from an authenticated player.
// gameID/playerID come from the session. Returning an *httpx.Error sends its
// code and message to the client; other errors are reported as "internal".
type Handler func(ctx context.Context, gameID, playerID string, msg Message) error

// Module wires the gateway into GameKit.
type Module struct {
	Kit       *game.Kit
	DB        *store.DB
	Now       func() time.Time
	Metrics   *observability.Registry
	Log       *slog.Logger
	Options   Options
	OnMessage Handler // nil: application messages are rejected (unknown_type)
	// Bus connects the hubs of several instances (profile scale). nil keeps
	// the in-process hub. DB must be PostgreSQL when Bus is set.
	Bus *PostgresBus

	once sync.Once
	hub  *Hub
}

// Hub returns the module's hub (created on first use).
func (m *Module) Hub() *Hub {
	m.once.Do(func() {
		db, now, met, log := m.DB, m.Now, m.Metrics, m.Log
		if m.Kit != nil {
			if db == nil {
				db = m.Kit.DB
			}
			if now == nil {
				now = m.Kit.Now
			}
			if met == nil {
				met = m.Kit.Metrics
			}
			if log == nil {
				log = m.Kit.Log
			}
		}
		if now == nil {
			now = time.Now
		}
		if log == nil {
			log = slog.New(slog.DiscardHandler)
		}
		m.hub = newHub(m.Options.withDefaults(), db, now, met, log, m.OnMessage, m.Bus)
	})
	return m.hub
}

// Close closes the hub if it was created (see Hub.Close); it is a no-op
// otherwise and safe to call more than once.
func (m *Module) Close(ctx context.Context) error {
	m.once.Do(func() {}) // a later Hub() call must not start a hub after Close
	if m.hub == nil {
		return nil
	}
	return m.hub.Close(ctx)
}

// Routes registers GET /v1/games/{game}/realtime.
func (m *Module) Routes(mux *http.ServeMux) {
	h := m.Hub()
	m.Kit.Handle(mux, "GET /realtime", game.Public, h.serveHTTP)
}

// Close codes sent by the server (4xxx are application codes).
const (
	CodeUnauthorized websocket.StatusCode = 4001 // auth missing/invalid/timeout, session revoked
	CodeForbidden    websocket.StatusCode = 4003 // token of another game
	CodeSlowConsumer websocket.StatusCode = 4008 // outbound queue overflow / write timeout
	CodeReplaced     websocket.StatusCode = 4009 // replaced by a newer connection of the player
	CodeStreamReset  websocket.StatusCode = 4012 // the player's stream changed epoch (bus degraded/restored, gap): reconnect and resume
	CodeRateLimited  websocket.StatusCode = 4029 // inbound rate limit exceeded
)

var urlTokenParams = []string{"token", "access_token", "authorization", "auth", "bearer"}

// serveHTTP runs the pre-upgrade checks, upgrades and hands the connection to
// the hub. It returns right after the upgrade so the connection does not keep
// the HTTP concurrency slot or the request deadline.
func (h *Hub) serveHTTP(w http.ResponseWriter, r *http.Request) error {
	g := game.CurrentGame(r.Context())
	q := r.URL.Query()
	for _, k := range urlTokenParams {
		if q.Has(k) {
			h.count("realtime_rejected_total", "token_in_url")
			return httpx.NewError(http.StatusBadRequest, "token_in_url", "access token must not be sent in the URL")
		}
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		ok, err := h.originAllowed(r.Context(), g.ID, origin)
		if err != nil {
			return err
		}
		if !ok {
			h.count("realtime_rejected_total", "origin")
			return httpx.NewError(http.StatusForbidden, "origin_not_allowed", "origin not allowed for this game")
		}
	}
	var player, tokenHash string
	if r.Header.Get("Authorization") != "" {
		tok := httpx.BearerToken(r)
		if tok == "" {
			h.count("realtime_rejected_total", "auth")
			return httpx.Unauthorized("malformed Authorization header")
		}
		var err error
		if player, tokenHash, err = h.authenticate(r.Context(), g.ID, tok); err != nil {
			h.count("realtime_rejected_total", "auth")
			return err
		}
	}
	if !h.reserve() {
		h.count("realtime_rejected_total", "max_connections")
		return httpx.Overloaded("realtime connection limit reached")
	}
	// Origin was verified above against the game's allowed_origins.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		h.release()
		return nil // Accept already wrote the response
	}
	c := newConn(h, ws, g.ID)
	go c.run(player, tokenHash)
	return nil
}

func (h *Hub) originAllowed(ctx context.Context, gameID, origin string) (bool, error) {
	var raw string
	err := h.db.Read.QueryRowContext(ctx, `SELECT allowed_origins FROM games WHERE game_id=?`, gameID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, httpx.NotFound("game not found")
	}
	if err != nil {
		return false, err
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return false, err
	}
	return slices.Contains(list, origin), nil
}

// authenticate validates an access token for gameID with the same semantics as
// GameKit sessions (hash lookup, expiry, game match, game active).
func (h *Hub) authenticate(ctx context.Context, gameID, token string) (player, tokenHash string, err error) {
	tokenHash = auth.Hash(token)
	player, err = h.checkSession(ctx, gameID, tokenHash)
	return player, tokenHash, err
}

func (h *Hub) checkSession(ctx context.Context, gameID, tokenHash string) (string, error) {
	var gid, pid, status string
	var exp int64
	err := h.db.Read.QueryRowContext(ctx, `SELECT s.game_id, s.player_id, s.expires_at, g.status
		FROM sessions s JOIN games g ON g.game_id=s.game_id WHERE s.token_hash=?`, tokenHash).Scan(&gid, &pid, &exp, &status)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && exp <= h.now().UnixMilli()) {
		return "", httpx.Unauthorized("session invalid or expired")
	}
	if err != nil {
		return "", err
	}
	if gid != gameID {
		return "", httpx.Forbidden("session belongs to another game")
	}
	if status != "active" {
		return "", httpx.Forbidden("game is disabled")
	}
	return pid, nil
}
