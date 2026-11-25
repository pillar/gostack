// Package ops implements GameKit live operations (G10): announcements, mail,
// daily check-in, tasks, events and redeem codes. Rewards are always posted
// through game.ApplyLedger with a unique source key, so a claim can never pay twice.
package ops

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"
	_ "time/tzdata" // IANA zones must not depend on the host image

	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/modules/ratelimit"
)

// Module wires the ops feature package.
type Module struct {
	Kit *game.Kit
	DB  *store.DB
	Now func() time.Time // defaults to time.Now
	// Redeem attempts per minute per player and per client IP (defaults 10 / 60).
	RedeemPerPlayerPerMinute int
	RedeemPerIPPerMinute     int

	once                 sync.Once
	playerLimit, ipLimit *ratelimit.Limiter
}

func (m *Module) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Module) init() {
	m.once.Do(func() {
		p, i := m.RedeemPerPlayerPerMinute, m.RedeemPerIPPerMinute
		if p <= 0 {
			p = 10
		}
		if i <= 0 {
			i = 60
		}
		m.playerLimit = ratelimit.PerMinute(p, 100_000)
		m.ipLimit = ratelimit.PerMinute(i, 100_000)
	})
}

// Routes registers the client and admin routes.
func (m *Module) Routes(mux *http.ServeMux) {
	m.init()
	k := m.Kit
	// announcements and events
	k.Handle(mux, "GET /announcements", game.OptionalPlayer, m.listActiveAnnouncements)
	k.HandleAdmin(mux, "GET /announcements", m.adminListAnnouncements)
	k.HandleAdmin(mux, "PUT /announcements/{id}", m.adminPutAnnouncement)
	k.HandleAdmin(mux, "DELETE /announcements/{id}", m.adminDeleteAnnouncement)
	k.Handle(mux, "GET /events", game.OptionalPlayer, m.listActiveEvents)
	k.HandleAdmin(mux, "GET /events", m.adminListEvents)
	k.HandleAdmin(mux, "PUT /events/{id}", m.adminPutEvent)
	k.HandleAdmin(mux, "DELETE /events/{id}", m.adminDeleteEvent)
	// mail
	k.Handle(mux, "GET /mail", game.Player, m.listMail)
	k.Handle(mux, "POST /mail/{id}/read", game.Player, m.readMail)
	k.Handle(mux, "POST /mail/{id}/claim", game.Player, m.claimMail)
	k.Handle(mux, "DELETE /mail/{id}", game.Player, m.deleteMail)
	k.HandleAdmin(mux, "POST /mail", m.adminSendMail)
	k.HandleAdmin(mux, "GET /mail", m.adminListMail)
	k.HandleAdmin(mux, "DELETE /mail/{id}", m.adminDeleteMail)
	// check-in
	k.Handle(mux, "GET /checkin", game.Player, m.getCheckin)
	k.Handle(mux, "POST /checkin", game.Player, m.postCheckin)
	k.HandleAdmin(mux, "GET /checkin", m.adminGetCheckin)
	k.HandleAdmin(mux, "PUT /checkin", m.adminPutCheckin)
	// tasks
	k.Handle(mux, "GET /tasks", game.Player, m.listTasks)
	k.Handle(mux, "POST /tasks/{id}/claim", game.Player, m.claimTask)
	k.HandleAdmin(mux, "GET /tasks", m.adminListTasks)
	k.HandleAdmin(mux, "PUT /tasks/{id}", m.adminPutTask)
	k.HandleAdmin(mux, "DELETE /tasks/{id}", m.adminDeleteTask)
	k.HandleAdmin(mux, "POST /progress", m.adminProgress)
	// redeem codes
	k.Handle(mux, "POST /redeem", game.Player, m.redeem)
	k.HandleAdmin(mux, "POST /redeem-batches", m.adminCreateBatch)
	k.HandleAdmin(mux, "GET /redeem-batches", m.adminListBatches)
	k.HandleAdmin(mux, "DELETE /redeem-batches/{id}", m.adminDisableBatch)
}

// Maintain removes mail that expired more than 7 days ago (with its per-player state).
func (m *Module) Maintain(ctx context.Context) error {
	cutoff := m.now().Add(-7 * 24 * time.Hour).UnixMilli()
	return m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM ops_mail_state WHERE EXISTS (
			SELECT 1 FROM ops_mail m WHERE m.game_id=ops_mail_state.game_id AND m.mail_id=ops_mail_state.mail_id
			AND m.expires_at>0 AND m.expires_at<?)`, cutoff); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM ops_mail WHERE expires_at>0 AND expires_at<?`, cutoff)
		return err
	})
}

// ---- shared helpers ----

// Instant is an epoch-millisecond time that decodes from a JSON number
// (epoch ms) or an RFC3339 string (the offset carries the time zone).
type Instant int64

func (i *Instant) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return errors.New("time must be epoch milliseconds or RFC3339")
		}
		*i = Instant(t.UnixMilli())
		return nil
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil || n < 0 {
		return errors.New("time must be epoch milliseconds or RFC3339")
	}
	*i = Instant(n)
	return nil
}

// Grants maps asset to a positive integer amount.
type Grants map[string]int64

func validGrants(g Grants, required bool) error {
	if len(g) == 0 {
		if required {
			return httpx.BadRequest("grants must not be empty")
		}
		return nil
	}
	if len(g) > 20 {
		return httpx.BadRequest("at most 20 assets per grant")
	}
	for a, n := range g {
		if !game.ValidID(a) {
			return httpx.BadRequest("invalid asset id " + strconv.Quote(a))
		}
		if n <= 0 || n > 1_000_000_000_000 {
			return httpx.BadRequest("grant amounts must be positive integers")
		}
	}
	return nil
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func parseGrants(s string) Grants {
	g := Grants{}
	_ = json.Unmarshal([]byte(s), &g)
	return g
}

func decodeBody(r *http.Request, limit int64, v any) error {
	b, err := game.ReadBody(r, limit)
	if err != nil {
		return err
	}
	return game.DecodeStrict(b, v)
}

func loadZone(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil || name == "Local" {
		return nil, httpx.BadRequest("unknown IANA timezone " + strconv.Quote(name))
	}
	return loc, nil
}

// zoneOrUTC loads a stored (already validated) zone.
func zoneOrUTC(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil || name == "" {
		return time.UTC
	}
	return loc
}

func pathID(r *http.Request) (string, error) {
	id := r.PathValue("id")
	if !game.ValidID(id) {
		return "", httpx.NotFound("not found")
	}
	return id, nil
}

func writeNoContent(w http.ResponseWriter) error {
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- announcements ----

type announcementReq struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Priority  int      `json:"priority"`
	StartsAt  Instant  `json:"starts_at"`
	EndsAt    Instant  `json:"ends_at"`
	Platforms []string `json:"platforms"`
}

type announcementOut struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Priority  int      `json:"priority"`
	StartsAt  int64    `json:"starts_at"`
	EndsAt    int64    `json:"ends_at"`
	Platforms []string `json:"platforms"`
}

func (m *Module) adminPutAnnouncement(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var req announcementReq
	if err := decodeBody(r, 16<<10, &req); err != nil {
		return err
	}
	if req.Title == "" || len(req.Title) > 200 || len(req.Body) > 8000 {
		return httpx.BadRequest("title is required (<=200 bytes) and body must be <=8000 bytes")
	}
	if req.EndsAt != 0 && req.EndsAt <= req.StartsAt {
		return httpx.BadRequest("ends_at must be after starts_at")
	}
	if len(req.Platforms) > 8 {
		return httpx.BadRequest("at most 8 platforms")
	}
	if req.Platforms == nil {
		req.Platforms = []string{}
	}
	g := game.CurrentGame(r.Context())
	now := m.now().UnixMilli()
	err = m.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.Context(), `INSERT INTO ops_announcements(game_id,id,title,body,priority,starts_at,ends_at,platforms,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(game_id,id) DO UPDATE SET title=excluded.title, body=excluded.body, priority=excluded.priority,
			  starts_at=excluded.starts_at, ends_at=excluded.ends_at, platforms=excluded.platforms, updated_at=excluded.updated_at`,
			g.ID, id, req.Title, req.Body, req.Priority, int64(req.StartsAt), int64(req.EndsAt), jsonString(req.Platforms), now, now)
		return err
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, announcementOut{id, req.Title, req.Body, req.Priority, int64(req.StartsAt), int64(req.EndsAt), req.Platforms})
	return nil
}

func (m *Module) queryAnnouncements(ctx context.Context, gameID string, onlyActive bool, platform string) ([]announcementOut, error) {
	now := m.now().UnixMilli()
	rows, err := m.DB.Read.QueryContext(ctx, `SELECT id,title,body,priority,starts_at,ends_at,platforms FROM ops_announcements
		WHERE game_id=? AND (?=0 OR (starts_at<=? AND (ends_at=0 OR ends_at>?)))
		ORDER BY priority DESC, starts_at DESC, id LIMIT 200`, gameID, boolInt(onlyActive), now, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []announcementOut{}
	for rows.Next() {
		var a announcementOut
		var pl string
		if err := rows.Scan(&a.ID, &a.Title, &a.Body, &a.Priority, &a.StartsAt, &a.EndsAt, &pl); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(pl), &a.Platforms)
		if a.Platforms == nil {
			a.Platforms = []string{}
		}
		if onlyActive && len(a.Platforms) > 0 && !slices.Contains(a.Platforms, platform) {
			continue
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (m *Module) listActiveAnnouncements(w http.ResponseWriter, r *http.Request) error {
	out, err := m.queryAnnouncements(r.Context(), game.CurrentGame(r.Context()).ID, true, r.URL.Query().Get("platform"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"announcements": out})
	return nil
}

func (m *Module) adminListAnnouncements(w http.ResponseWriter, r *http.Request) error {
	out, err := m.queryAnnouncements(r.Context(), game.CurrentGame(r.Context()).ID, false, "")
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"announcements": out})
	return nil
}

func (m *Module) adminDeleteAnnouncement(w http.ResponseWriter, r *http.Request) error {
	return m.adminDelete(w, r, "ops_announcements")
}

// adminDelete deletes one row of a (game_id,id)-keyed table; 404 when absent.
func (m *Module) adminDelete(w http.ResponseWriter, r *http.Request, table string) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	g := game.CurrentGame(r.Context())
	var n int64
	err = m.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), `DELETE FROM `+table+` WHERE game_id=? AND id=?`, g.ID, id)
		if err == nil {
			n, err = res.RowsAffected()
		}
		return err
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	if n == 0 {
		return httpx.NotFound("not found")
	}
	return writeNoContent(w)
}

// ---- events ----

type eventReq struct {
	Title    string          `json:"title"`
	StartsAt Instant         `json:"starts_at"`
	EndsAt   Instant         `json:"ends_at"`
	Timezone string          `json:"timezone"`
	Metadata json.RawMessage `json:"metadata"`
}

type eventOut struct {
	ID       string          `json:"id"`
	Title    string          `json:"title"`
	StartsAt int64           `json:"starts_at"`
	EndsAt   int64           `json:"ends_at"`
	Timezone string          `json:"timezone"`
	Metadata json.RawMessage `json:"metadata"`
}

func (m *Module) adminPutEvent(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var req eventReq
	if err := decodeBody(r, 16<<10, &req); err != nil {
		return err
	}
	if req.Title == "" || len(req.Title) > 200 {
		return httpx.BadRequest("title is required (<=200 bytes)")
	}
	if req.StartsAt == 0 || req.EndsAt <= req.StartsAt {
		return httpx.BadRequest("starts_at and ends_at are required and ends_at must be after starts_at")
	}
	if _, err := loadZone(req.Timezone); err != nil {
		return err
	}
	if req.Timezone == "" {
		req.Timezone = "UTC"
	}
	if len(req.Metadata) == 0 {
		req.Metadata = json.RawMessage("{}")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(req.Metadata, &obj) != nil || obj == nil || len(req.Metadata) > 8<<10 {
		return httpx.BadRequest("metadata must be a JSON object up to 8KiB")
	}
	g := game.CurrentGame(r.Context())
	now := m.now().UnixMilli()
	err = m.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.Context(), `INSERT INTO ops_events(game_id,id,title,starts_at,ends_at,timezone,metadata,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?)
			ON CONFLICT(game_id,id) DO UPDATE SET title=excluded.title, starts_at=excluded.starts_at, ends_at=excluded.ends_at,
			  timezone=excluded.timezone, metadata=excluded.metadata, updated_at=excluded.updated_at`,
			g.ID, id, req.Title, int64(req.StartsAt), int64(req.EndsAt), req.Timezone, string(req.Metadata), now, now)
		return err
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, eventOut{id, req.Title, int64(req.StartsAt), int64(req.EndsAt), req.Timezone, req.Metadata})
	return nil
}

func (m *Module) queryEvents(ctx context.Context, gameID string, onlyActive bool) ([]eventOut, error) {
	now := m.now().UnixMilli()
	rows, err := m.DB.Read.QueryContext(ctx, `SELECT id,title,starts_at,ends_at,timezone,metadata FROM ops_events
		WHERE game_id=? AND (?=0 OR (starts_at<=? AND ends_at>?)) ORDER BY starts_at, id LIMIT 200`, gameID, boolInt(onlyActive), now, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []eventOut{}
	for rows.Next() {
		var e eventOut
		var md string
		if err := rows.Scan(&e.ID, &e.Title, &e.StartsAt, &e.EndsAt, &e.Timezone, &md); err != nil {
			return nil, err
		}
		e.Metadata = json.RawMessage(md)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (m *Module) listActiveEvents(w http.ResponseWriter, r *http.Request) error {
	out, err := m.queryEvents(r.Context(), game.CurrentGame(r.Context()).ID, true)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"events": out})
	return nil
}

func (m *Module) adminListEvents(w http.ResponseWriter, r *http.Request) error {
	out, err := m.queryEvents(r.Context(), game.CurrentGame(r.Context()).ID, false)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"events": out})
	return nil
}

func (m *Module) adminDeleteEvent(w http.ResponseWriter, r *http.Request) error {
	return m.adminDelete(w, r, "ops_events")
}

func eventActive(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, gameID, eventID string, nowMs int64) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM ops_events WHERE game_id=? AND id=? AND starts_at<=? AND ends_at>?`, gameID, eventID, nowMs, nowMs).Scan(&n)
	return n > 0, err
}
