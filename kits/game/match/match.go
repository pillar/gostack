// Package match is GameKit G15: matchmaking tickets, rooms with fenced leases
// and authoritative settlement by battle servers.
//
// Players only talk to /v1/games/{game}/match/...; battle servers are separate
// processes that authenticate with an API key carrying the `battle` scope and
// talk to /v1/match/rooms/.... Nothing here runs per frame: rooms are written
// at creation, claim, heartbeat (once per lease interval), member state change
// and settlement.
package match

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/modules/idempotency"
	"github.com/pillar/gostack/modules/ratelimit"
)

// Module serves the matchmaking API. Zero options take the documented defaults.
type Module struct {
	Kit *game.Kit
	DB  *store.DB
	Now func() time.Time
	Log *slog.Logger

	LeaseTTL         time.Duration // room lease after claim / each heartbeat (default 30s)
	ClaimTimeout     time.Duration // an unclaimed room is abandoned after this (default 60s)
	MatchInterval    time.Duration // Run loop period (default 1s)
	Retention        time.Duration // terminal tickets and abandoned rooms are purged after this (default 7d)
	MaxMatchBatch    int           // waiting tickets considered per queue per pass (default 500)
	TicketsPerMinute int           // per-player ticket creation limit (default 60)

	once    sync.Once
	limiter ratelimit.Allower
	apiKeys auth.APIKeys
}

func (m *Module) init() {
	m.once.Do(func() {
		if m.Now == nil {
			m.Now = time.Now
		}
		if m.Log == nil {
			m.Log = slog.Default()
		}
		dur := func(p *time.Duration, v time.Duration) {
			if *p <= 0 {
				*p = v
			}
		}
		dur(&m.LeaseTTL, 30*time.Second)
		dur(&m.ClaimTimeout, 60*time.Second)
		dur(&m.MatchInterval, time.Second)
		dur(&m.Retention, 7*24*time.Hour)
		if m.MaxMatchBatch <= 0 {
			m.MaxMatchBatch = 500
		}
		if m.TicketsPerMinute <= 0 {
			m.TicketsPerMinute = 60
		}
		m.limiter = m.Kit.NewLimiter("match_tickets", m.TicketsPerMinute, ratelimit.FailLocal)
		m.apiKeys = auth.APIKeys{DB: m.DB}
	})
}

// Routes registers player, admin and battle-server routes on mux.
func (m *Module) Routes(mux *http.ServeMux) {
	m.init()
	m.Kit.Handle(mux, "POST /match/tickets", game.Player, m.createTicket)
	m.Kit.Handle(mux, "GET /match/tickets/{id}", game.Player, m.getTicket)
	m.Kit.Handle(mux, "DELETE /match/tickets/{id}", game.Player, m.cancelTicket)
	m.Kit.HandleAdmin(mux, "PUT /match/queues/{queue}", m.putQueue)
	m.Kit.HandleAdmin(mux, "GET /match/queues", m.listQueues)

	mux.Handle("POST /v1/match/rooms/claim", m.battle(m.claim))
	mux.Handle("POST /v1/match/rooms/{room}/heartbeat", m.battle(m.heartbeat))
	mux.Handle("POST /v1/match/rooms/{room}/members/{player}/state", m.battle(m.memberState))
	mux.Handle("POST /v1/match/rooms/{room}/result", m.battle(m.result))
}

// Maintain runs periodic cleanup (called by the app maintenance loop).
func (m *Module) Maintain(ctx context.Context) error {
	m.init()
	if err := m.Sweep(ctx); err != nil {
		return err
	}
	return m.purge(ctx)
}

// Run matches and sweeps every MatchInterval until ctx ends.
func (m *Module) Run(ctx context.Context) {
	m.init()
	t := time.NewTicker(m.MatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := m.MatchOnce(ctx); err != nil && ctx.Err() == nil {
				m.Log.Error("match pass failed", "err", err)
			}
			if err := m.Sweep(ctx); err != nil && ctx.Err() == nil {
				m.Log.Error("match sweep failed", "err", err)
			}
		}
	}
}

// ---- shared helpers ----

var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func count(ctx context.Context, q queryer, query string, args ...any) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, query, args...).Scan(&n)
	return n, err
}

// call is the identity and transaction of one idempotent player mutation.
type call struct {
	ctx context.Context
	tx  *sql.Tx
	gid string
	pid string
	now int64
}

func (m *Module) mutate(w http.ResponseWriter, r *http.Request, op string, body []byte, fn func(c *call) (idempotency.Result, error)) error {
	ctx := r.Context()
	s := game.CurrentSession(ctx)
	status, resp, replayed, err := idempotency.Do(ctx, m.DB, "match:"+s.GameID+":"+s.PlayerID, r.Header.Get("Idempotency-Key"), op, body,
		func(tx *sql.Tx) (idempotency.Result, error) {
			return fn(&call{ctx: ctx, tx: tx, gid: s.GameID, pid: s.PlayerID, now: m.Now().UnixMilli()})
		})
	if err != nil {
		return game.MapWriteErr(err)
	}
	idempotency.Write(w, status, resp, replayed)
	return nil
}

// abandonRoom ends a room whose lease ran out and releases its players'
// tickets as 'abandoned' (visible through ticket status).
func abandonRoom(ctx context.Context, tx *sql.Tx, roomID string, now int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE match_rooms SET status='abandoned', updated_at=? WHERE room_id=? AND status IN ('open','active')`, now, roomID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE match_tickets SET status='abandoned', updated_at=? WHERE room_id=? AND status='matched'`, now, roomID)
	return err
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
