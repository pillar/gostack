package match

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/modules/idempotency"
)

const maxRating = 1_000_000

type queueDef struct {
	ID           string `json:"queue"`
	Size         int    `json:"size"`
	RatingWindow int64  `json:"rating_window"`
	WidenPerSec  int64  `json:"widen_per_sec"`
	MaxWindow    int64  `json:"max_window"`
	TTL          int64  `json:"ttl"`
	Board        string `json:"board"`
}

const queueCols = `queue_id, size, rating_window, widen_per_sec, max_window, ttl_sec, board`

func scanQueue(s interface{ Scan(...any) error }) (queueDef, error) {
	var q queueDef
	err := s.Scan(&q.ID, &q.Size, &q.RatingWindow, &q.WidenPerSec, &q.MaxWindow, &q.TTL, &q.Board)
	return q, err
}

// ---- admin ----

func (m *Module) putQueue(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	g := game.CurrentGame(ctx)
	id := r.PathValue("queue")
	if !game.ValidID(id) {
		return httpx.BadRequest("invalid queue id")
	}
	body, err := game.ReadBody(r, 4096)
	if err != nil {
		return err
	}
	var req struct {
		Size         *int   `json:"size"`
		RatingWindow *int64 `json:"rating_window"`
		WidenPerSec  *int64 `json:"widen_per_sec"`
		MaxWindow    int64  `json:"max_window"`
		TTL          *int64 `json:"ttl"`
		Board        string `json:"board"`
	}
	if err := game.DecodeStrict(body, &req); err != nil {
		return err
	}
	if req.Size == nil || req.RatingWindow == nil || req.WidenPerSec == nil || req.TTL == nil {
		return httpx.BadRequest("size, rating_window, widen_per_sec and ttl are required")
	}
	q := queueDef{ID: id, Size: *req.Size, RatingWindow: *req.RatingWindow, WidenPerSec: *req.WidenPerSec, MaxWindow: req.MaxWindow, TTL: *req.TTL, Board: req.Board}
	switch {
	case q.Size < 2 || q.Size > 64:
		return httpx.BadRequest("size must be within [2,64]")
	case q.RatingWindow < 0 || q.RatingWindow > 2*maxRating || q.WidenPerSec < 0 || q.WidenPerSec > 2*maxRating || q.MaxWindow < 0 || q.MaxWindow > 2*maxRating:
		return httpx.BadRequest("rating_window, widen_per_sec and max_window must be within [0,2000000]")
	case q.TTL < 5 || q.TTL > 3600:
		return httpx.BadRequest("ttl must be within [5,3600] seconds")
	}
	if q.Board != "" {
		doc, _, err := m.Kit.CurrentDocument(ctx, m.DB.Read, g.ID)
		if err != nil {
			return err
		}
		if _, ok := doc.Boards[q.Board]; !ok {
			return httpx.BadRequest("board is not defined in the game config")
		}
	}
	err = m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO match_queues(game_id,queue_id,size,rating_window,widen_per_sec,max_window,ttl_sec,board,updated_at) VALUES(?,?,?,?,?,?,?,?,?)
			ON CONFLICT(game_id,queue_id) DO UPDATE SET size=excluded.size, rating_window=excluded.rating_window, widen_per_sec=excluded.widen_per_sec,
			max_window=excluded.max_window, ttl_sec=excluded.ttl_sec, board=excluded.board, updated_at=excluded.updated_at`,
			g.ID, q.ID, q.Size, q.RatingWindow, q.WidenPerSec, q.MaxWindow, q.TTL, q.Board, m.Now().UnixMilli())
		return err
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, q)
	return nil
}

func (m *Module) listQueues(w http.ResponseWriter, r *http.Request) error {
	g := game.CurrentGame(r.Context())
	rows, err := m.DB.Read.QueryContext(r.Context(), `SELECT `+queueCols+` FROM match_queues WHERE game_id=? ORDER BY queue_id`, g.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []queueDef{}
	for rows.Next() {
		q, err := scanQueue(rows)
		if err != nil {
			return err
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"queues": out})
	return nil
}

// ---- tickets ----

// releaseStale settles the player's active ticket if time already ended it
// (ticket TTL or room lease), so a stale ticket never blocks a new one.
func releaseStale(ctx context.Context, tx *sql.Tx, gid, pid string, now int64) error {
	var id, status string
	var room sql.NullString
	var exp int64
	err := tx.QueryRowContext(ctx, `SELECT ticket_id, status, room_id, expires_at FROM match_tickets WHERE game_id=? AND player_id=? AND status IN ('waiting','matched')`, gid, pid).Scan(&id, &status, &room, &exp)
	if isNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if status == "waiting" && exp <= now {
		_, err = tx.ExecContext(ctx, `UPDATE match_tickets SET status='expired', updated_at=? WHERE ticket_id=?`, now, id)
		return err
	}
	if status == "matched" && room.Valid {
		var lease int64
		if err := tx.QueryRowContext(ctx, `SELECT lease_until FROM match_rooms WHERE room_id=? AND status IN ('open','active')`, room.String).Scan(&lease); isNoRows(err) {
			return nil
		} else if err != nil {
			return err
		}
		if lease <= now {
			return abandonRoom(ctx, tx, room.String, now)
		}
	}
	return nil
}

func (m *Module) createTicket(w http.ResponseWriter, r *http.Request) error {
	s := game.CurrentSession(r.Context())
	if err := m.Kit.RateLimit(m.limiter, "ticket:"+s.GameID+":"+s.PlayerID); err != nil {
		return err
	}
	body, err := game.ReadBody(r, 2048)
	if err != nil {
		return err
	}
	var req struct {
		Queue      string `json:"queue"`
		Attributes struct {
			Rating int64 `json:"rating"`
		} `json:"attributes"`
	}
	if err := game.DecodeStrict(body, &req); err != nil {
		return err
	}
	if !game.ValidID(req.Queue) {
		return httpx.NotFound("queue not found")
	}
	if req.Attributes.Rating < -maxRating || req.Attributes.Rating > maxRating {
		return httpx.BadRequest("attributes.rating must be within [-1000000,1000000]")
	}
	return m.mutate(w, r, "ticket.create", body, func(c *call) (idempotency.Result, error) {
		q, err := scanQueue(c.tx.QueryRowContext(c.ctx, `SELECT `+queueCols+` FROM match_queues WHERE game_id=? AND queue_id=?`, c.gid, req.Queue))
		if isNoRows(err) {
			return idempotency.Result{}, httpx.NotFound("queue not found")
		}
		if err != nil {
			return idempotency.Result{}, err
		}
		if err := releaseStale(c.ctx, c.tx, c.gid, c.pid, c.now); err != nil {
			return idempotency.Result{}, err
		}
		if n, err := count(c.ctx, c.tx, `SELECT count(*) FROM match_tickets WHERE game_id=? AND player_id=? AND status IN ('waiting','matched')`, c.gid, c.pid); err != nil {
			return idempotency.Result{}, err
		} else if n > 0 {
			return idempotency.Result{}, httpx.Conflict("ticket_active", "player already has an active match ticket")
		}
		id := httpx.RandomID("tkt_")
		exp := c.now + q.TTL*1000
		if _, err := c.tx.ExecContext(c.ctx, `INSERT INTO match_tickets(ticket_id,game_id,queue_id,player_id,rating,status,created_at,expires_at,updated_at) VALUES(?,?,?,?,?,'waiting',?,?,?)`,
			id, c.gid, q.ID, c.pid, req.Attributes.Rating, c.now, exp, c.now); err != nil {
			return idempotency.Result{}, err
		}
		return idempotency.Result{Status: http.StatusCreated, Body: map[string]any{"ticket_id": id, "queue": q.ID, "status": "waiting", "expires_at": exp}}, nil
	})
}

type member struct {
	PlayerID string `json:"player_id"`
	State    string `json:"state"`
}

type roomView struct {
	RoomID     string          `json:"room_id"`
	Status     string          `json:"status"`
	LeaseUntil int64           `json:"lease_until"`
	Connection json.RawMessage `json:"connection"` // null until a battle server claims the room
	Members    []member        `json:"members"`
}

func roomInfo(ctx context.Context, tx *sql.Tx, id string, now int64) (*roomView, error) {
	v := &roomView{RoomID: id}
	var conn string
	if err := tx.QueryRowContext(ctx, `SELECT status, lease_until, connection FROM match_rooms WHERE room_id=?`, id).Scan(&v.Status, &v.LeaseUntil, &conn); err != nil {
		return nil, err
	}
	if (v.Status == "open" || v.Status == "active") && v.LeaseUntil <= now {
		v.Status = "abandoned" // reported as such until the sweep persists it
	}
	v.Connection = json.RawMessage("null")
	if v.Status == "active" || v.Status == "settled" {
		v.Connection = json.RawMessage(conn)
	}
	rows, err := tx.QueryContext(ctx, `SELECT player_id, state FROM match_room_members WHERE room_id=? ORDER BY player_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	v.Members = []member{}
	for rows.Next() {
		var mb member
		if err := rows.Scan(&mb.PlayerID, &mb.State); err != nil {
			return nil, err
		}
		v.Members = append(v.Members, mb)
	}
	return v, rows.Err()
}

func (m *Module) getTicket(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := game.CurrentSession(ctx)
	id := r.PathValue("id")
	now := m.Now().UnixMilli()
	var out map[string]any
	err := m.DB.ReadTx(ctx, func(tx *sql.Tx) error {
		var queue, status string
		var rating, created, exp int64
		var room sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT queue_id, status, rating, room_id, created_at, expires_at FROM match_tickets WHERE ticket_id=? AND game_id=? AND player_id=?`,
			id, s.GameID, s.PlayerID).Scan(&queue, &status, &rating, &room, &created, &exp)
		if isNoRows(err) {
			return httpx.NotFound("ticket not found")
		}
		if err != nil {
			return err
		}
		if status == "waiting" && exp <= now {
			status = "expired"
		}
		out = map[string]any{"ticket_id": id, "queue": queue, "status": status, "rating": rating, "created_at": created, "expires_at": exp}
		if room.Valid {
			rv, err := roomInfo(ctx, tx, room.String, now)
			if err != nil {
				return err
			}
			if status == "matched" && rv.Status == "abandoned" {
				out["status"] = "abandoned"
			}
			out["room"] = rv
		}
		return nil
	})
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// cancelTicket withdraws a waiting ticket. A matched ticket belongs to a room
// and can only end by settlement or abandonment.
func (m *Module) cancelTicket(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	return m.mutate(w, r, "ticket.cancel:"+id, nil, func(c *call) (idempotency.Result, error) {
		var status string
		var exp int64
		err := c.tx.QueryRowContext(c.ctx, `SELECT status, expires_at FROM match_tickets WHERE ticket_id=? AND game_id=? AND player_id=?`, id, c.gid, c.pid).Scan(&status, &exp)
		if isNoRows(err) {
			return idempotency.Result{}, httpx.NotFound("ticket not found")
		}
		if err != nil {
			return idempotency.Result{}, err
		}
		switch status {
		case "waiting":
			if _, err := c.tx.ExecContext(c.ctx, `UPDATE match_tickets SET status='cancelled', updated_at=? WHERE ticket_id=?`, c.now, id); err != nil {
				return idempotency.Result{}, err
			}
			return idempotency.Result{Body: map[string]any{"ticket_id": id, "status": "cancelled"}}, nil
		case "cancelled":
			return idempotency.Result{Body: map[string]any{"ticket_id": id, "status": "cancelled"}}, nil
		case "matched":
			return idempotency.Result{}, httpx.Conflict("already_matched", "ticket is already in a room")
		}
		return idempotency.Result{}, httpx.Conflict("ticket_closed", "ticket is "+status)
	})
}
