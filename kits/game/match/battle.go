package match

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/modules/idempotency"
)

// battle authenticates a battle server: an API key with the `battle` scope. The
// key's tenant bounds every room it can see.
func (m *Module) battle(next func(w http.ResponseWriter, r *http.Request, key auth.APIKey) error) http.Handler {
	return httpx.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		key, err := m.apiKeys.Verify(r.Context(), httpx.BearerToken(r))
		if err != nil {
			return err
		}
		if !key.Has("battle") {
			return httpx.Forbidden("api key lacks battle scope")
		}
		return next(w, r, key)
	})
}

type room struct {
	ID     string
	Game   string
	Queue  string
	Status string
	Epoch  int64
	Lease  int64
}

// loadRoom returns the room only when its game belongs to tenant; other
// tenants' rooms are indistinguishable from missing ones.
func loadRoom(ctx context.Context, tx *sql.Tx, id, tenant string) (room, error) {
	var rm room
	err := tx.QueryRowContext(ctx, `SELECT r.room_id, r.game_id, r.queue_id, r.status, r.epoch, r.lease_until FROM match_rooms r JOIN games g ON g.game_id=r.game_id
		WHERE r.room_id=? AND g.tenant_id=?`, id, tenant).Scan(&rm.ID, &rm.Game, &rm.Queue, &rm.Status, &rm.Epoch, &rm.Lease)
	if isNoRows(err) {
		return rm, httpx.NotFound("room not found")
	}
	return rm, err
}

var (
	errStale     = httpx.Conflict("stale_token", "fencing token does not match the room's current token")
	errAbandoned = httpx.Conflict("room_abandoned", "room lease expired; the room was abandoned")
	errSettled   = httpx.Conflict("room_settled", "room is already settled")
)

// fenced runs fn on a room the caller holds the current fencing token for. A
// room whose lease already ran out is abandoned (and that is committed) before
// the request is refused with 409 room_abandoned.
func (m *Module) fenced(ctx context.Context, key auth.APIKey, roomID string, token int64, allowSettled bool, fn func(tx *sql.Tx, rm room, now int64) error) error {
	var after error
	err := m.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		after = nil
		now := m.Now().UnixMilli()
		rm, err := loadRoom(ctx, tx, roomID, key.TenantID)
		if err != nil {
			return err
		}
		if token <= 0 || token != rm.Epoch {
			return errStale
		}
		switch rm.Status {
		case "abandoned":
			return errAbandoned
		case "settled":
			if !allowSettled {
				return errSettled
			}
		case "active":
			if rm.Lease <= now {
				if err := abandonRoom(ctx, tx, rm.ID, now); err != nil {
					return err
				}
				after = errAbandoned
				return nil
			}
		default:
			return errStale // never claimed: no token can be valid
		}
		return fn(tx, rm, now)
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	return after
}

func (m *Module) claim(w http.ResponseWriter, r *http.Request, key auth.APIKey) error {
	ctx := r.Context()
	body, err := game.ReadBody(r, 4096)
	if err != nil {
		return err
	}
	var req struct {
		ServerID   string          `json:"server_id"`
		GameID     string          `json:"game_id"`
		Queue      string          `json:"queue"`
		Connection json.RawMessage `json:"connection"`
	}
	if err := game.DecodeStrict(body, &req); err != nil {
		return err
	}
	if !idRe.MatchString(req.ServerID) {
		return httpx.BadRequest("server_id is required (1-64 chars: letters, digits, _ -)")
	}
	if req.GameID != "" && !game.ValidID(req.GameID) || req.Queue != "" && !game.ValidID(req.Queue) {
		return httpx.BadRequest("invalid game_id or queue")
	}
	conn := req.Connection
	if len(conn) == 0 {
		conn = json.RawMessage("{}")
	}
	var obj map[string]any
	if len(conn) > 1024 || json.Unmarshal(conn, &obj) != nil || obj == nil {
		return httpx.BadRequest("connection must be a JSON object of at most 1024 bytes")
	}
	status, resp, replayed, err := idempotency.Do(ctx, m.DB, "match.claim:"+key.ID, r.Header.Get("Idempotency-Key"), "room.claim", body,
		func(tx *sql.Tx) (idempotency.Result, error) {
			now := m.Now().UnixMilli()
			q := `SELECT r.room_id, r.game_id, r.queue_id FROM match_rooms r JOIN games g ON g.game_id=r.game_id
				WHERE g.tenant_id=? AND r.status='open' AND r.lease_until>?`
			args := []any{key.TenantID, now}
			if req.GameID != "" {
				q += ` AND r.game_id=?`
				args = append(args, req.GameID)
			}
			if req.Queue != "" {
				q += ` AND r.queue_id=?`
				args = append(args, req.Queue)
			}
			var id, gid, queue string
			err := tx.QueryRowContext(ctx, q+` ORDER BY r.created_at, r.room_id LIMIT 1`, args...).Scan(&id, &gid, &queue)
			if isNoRows(err) {
				return idempotency.Result{Status: http.StatusNoContent}, nil
			}
			if err != nil {
				return idempotency.Result{}, err
			}
			var epoch int64
			if err := tx.QueryRowContext(ctx, `INSERT INTO match_epochs(game_id,epoch) VALUES(?,1) ON CONFLICT(game_id) DO UPDATE SET epoch=epoch+1 RETURNING epoch`, gid).Scan(&epoch); err != nil {
				return idempotency.Result{}, err
			}
			lease := now + m.LeaseTTL.Milliseconds()
			if _, err := tx.ExecContext(ctx, `UPDATE match_rooms SET status='active', epoch=?, server_id=?, connection=?, lease_until=?, updated_at=? WHERE room_id=? AND status='open'`,
				epoch, req.ServerID, string(conn), lease, now, id); err != nil {
				return idempotency.Result{}, err
			}
			rv, err := roomInfo(ctx, tx, id, now)
			if err != nil {
				return idempotency.Result{}, err
			}
			return idempotency.Result{Body: map[string]any{"room_id": id, "game_id": gid, "queue": queue, "token": epoch, "lease_until": lease,
				"lease_ttl_ms": m.LeaseTTL.Milliseconds(), "members": rv.Members}}, nil
		})
	if err != nil {
		return game.MapWriteErr(err)
	}
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return nil
	}
	idempotency.Write(w, status, resp, replayed)
	return nil
}

func decodeToken(r *http.Request, v any) error {
	body, err := game.ReadBody(r, 1<<20)
	if err != nil {
		return err
	}
	return game.DecodeStrict(body, v)
}

func (m *Module) heartbeat(w http.ResponseWriter, r *http.Request, key auth.APIKey) error {
	var req struct {
		Token int64 `json:"token"`
	}
	if err := decodeToken(r, &req); err != nil {
		return err
	}
	var lease int64
	err := m.fenced(r.Context(), key, r.PathValue("room"), req.Token, false, func(tx *sql.Tx, rm room, now int64) error {
		lease = now + m.LeaseTTL.Milliseconds()
		_, err := tx.ExecContext(r.Context(), `UPDATE match_rooms SET lease_until=?, updated_at=? WHERE room_id=?`, lease, now, rm.ID)
		return err
	})
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"room_id": r.PathValue("room"), "lease_until": lease})
	return nil
}

// memberState records a player leaving or rejoining the battle. The slot stays
// reserved; the battle server decides what that means for the final result.
func (m *Module) memberState(w http.ResponseWriter, r *http.Request, key auth.APIKey) error {
	var req struct {
		Token int64  `json:"token"`
		State string `json:"state"`
	}
	if err := decodeToken(r, &req); err != nil {
		return err
	}
	if req.State != "connected" && req.State != "left" {
		return httpx.BadRequest("state must be connected or left")
	}
	player := r.PathValue("player")
	err := m.fenced(r.Context(), key, r.PathValue("room"), req.Token, false, func(tx *sql.Tx, rm room, now int64) error {
		res, err := tx.ExecContext(r.Context(), `UPDATE match_room_members SET state=?, updated_at=? WHERE room_id=? AND player_id=?`, req.State, now, rm.ID, player)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return httpx.NotFound("player is not a member of this room")
		}
		return nil
	})
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"room_id": r.PathValue("room"), "player_id": player, "state": req.State})
	return nil
}

// ---- settlement ----

type resultEntry struct {
	PlayerID string           `json:"player_id"`
	Score    *int64           `json:"score"`
	Grants   map[string]int64 `json:"grants"`
}

type resultOut struct {
	PlayerID    string           `json:"player_id"`
	Granted     map[string]int64 `json:"granted"`
	ScoreStatus string           `json:"score_status,omitempty"`
	Best        *int64           `json:"best,omitempty"`
}

const maxGrant = 1_000_000_000

func (m *Module) result(w http.ResponseWriter, r *http.Request, key auth.APIKey) error {
	ctx := r.Context()
	body, err := game.ReadBody(r, 256<<10)
	if err != nil {
		return err
	}
	var req struct {
		Token   int64         `json:"token"`
		Results []resultEntry `json:"results"`
	}
	if err := game.DecodeStrict(body, &req); err != nil {
		return err
	}
	if len(req.Results) == 0 || len(req.Results) > 64 {
		return httpx.BadRequest("results must contain 1-64 entries")
	}
	seen := map[string]bool{}
	for _, e := range req.Results {
		if !idRe.MatchString(e.PlayerID) || seen[e.PlayerID] {
			return httpx.BadRequest("results need distinct, valid player_id values")
		}
		seen[e.PlayerID] = true
		if len(e.Grants) > 8 {
			return httpx.BadRequest("at most 8 grant assets per player")
		}
		for a, v := range e.Grants {
			if !game.ValidID(a) || v < 0 || v > maxGrant {
				return httpx.BadRequest("grants must map valid asset ids to integers within [0,1000000000]")
			}
		}
	}
	hash := idempotency.RequestHash("room.result", body)
	roomID := r.PathValue("room")
	var out []byte
	var replayed bool
	err = m.fenced(ctx, key, roomID, req.Token, true, func(tx *sql.Tx, rm room, now int64) error {
		out, replayed = nil, false
		if rm.Status == "settled" {
			var prev, resp string
			if err := tx.QueryRowContext(ctx, `SELECT request_hash, response FROM match_settlements WHERE room_id=?`, rm.ID).Scan(&prev, &resp); err != nil {
				return err
			}
			if prev != hash {
				return httpx.Conflict("already_settled", "room was settled with a different result")
			}
			out, replayed = []byte(resp), true
			return nil
		}
		members := map[string]bool{}
		rs, err := tx.QueryContext(ctx, `SELECT player_id FROM match_room_members WHERE room_id=?`, rm.ID)
		if err != nil {
			return err
		}
		for rs.Next() {
			var p string
			if err := rs.Scan(&p); err != nil {
				rs.Close()
				return err
			}
			members[p] = true
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return err
		}
		wantScore := false
		for _, e := range req.Results {
			if !members[e.PlayerID] {
				return httpx.NewError(http.StatusBadRequest, "not_a_member", "player "+e.PlayerID+" is not a member of this room")
			}
			wantScore = wantScore || e.Score != nil
		}
		var board string
		var def game.BoardDef
		if wantScore {
			if err := tx.QueryRowContext(ctx, `SELECT board FROM match_queues WHERE game_id=? AND queue_id=?`, rm.Game, rm.Queue).Scan(&board); err != nil && !isNoRows(err) {
				return err
			}
			if board == "" {
				return httpx.NewError(http.StatusBadRequest, "no_board", "the room's queue has no board configured for scores")
			}
			doc, _, err := m.Kit.CurrentDocument(ctx, tx, rm.Game)
			if err != nil {
				return err
			}
			var ok bool
			if def, ok = doc.Boards[board]; !ok {
				return httpx.Conflict("board_unavailable", "the configured board no longer exists")
			}
		}
		sort.Slice(req.Results, func(i, j int) bool { return req.Results[i].PlayerID < req.Results[j].PlayerID })
		outs := make([]resultOut, 0, len(req.Results))
		for _, e := range req.Results {
			o := resultOut{PlayerID: e.PlayerID, Granted: map[string]int64{}}
			if len(e.Grants) > 0 {
				if err := game.ApplyLedger(ctx, tx, rm.Game, e.PlayerID, "match", "match:"+rm.ID+":"+e.PlayerID, e.Grants, now); err != nil {
					return err
				}
				for a, v := range e.Grants {
					if v != 0 {
						o.Granted[a] = v
					}
				}
			}
			if e.Score != nil {
				if o.ScoreStatus, o.Best, err = submitScore(ctx, tx, rm.Game, board, def, e.PlayerID, *e.Score, m.Now()); err != nil {
					return err
				}
			}
			outs = append(outs, o)
		}
		resp, err := json.Marshal(map[string]any{"room_id": rm.ID, "status": "settled", "results": outs})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO match_settlements(room_id,game_id,epoch,request_hash,response,created_at) VALUES(?,?,?,?,?,?)`,
			rm.ID, rm.Game, rm.Epoch, hash, string(resp), now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE match_rooms SET status='settled', updated_at=? WHERE room_id=?`, now, rm.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE match_tickets SET status='done', updated_at=? WHERE room_id=? AND status='matched'`, now, rm.ID); err != nil {
			return err
		}
		out = resp
		return nil
	})
	if err != nil {
		return err
	}
	idempotency.Write(w, http.StatusOK, out, replayed)
	return nil
}

// submitScore applies the leaderboard rules of kits/game/boards.go (range,
// quarantine per submission, best/sum, asc/desc, season) for a settled score.
func submitScore(ctx context.Context, tx *sql.Tx, gid, board string, def game.BoardDef, pid string, score int64, now time.Time) (string, *int64, error) {
	if score < def.MinScore || score > def.MaxScore {
		return "", nil, httpx.BadRequest(fmt.Sprintf("score for %s must be within [%d,%d]", pid, def.MinScore, def.MaxScore))
	}
	season := game.SeasonID(def.Period, now)
	ms := now.UnixMilli()
	var cur int64
	err := tx.QueryRowContext(ctx, `SELECT score FROM scores WHERE game_id=? AND board_id=? AND season_id=? AND player_id=?`, gid, board, season, pid).Scan(&cur)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", nil, err
	}
	candidate := score
	if def.Mode == "sum" && exists {
		candidate = cur + score
	}
	if def.QuarantineAbove != nil && score > *def.QuarantineAbove {
		if _, err := tx.ExecContext(ctx, `INSERT INTO score_quarantine(game_id,board_id,season_id,player_id,score,reason,created_at) VALUES(?,?,?,?,?,?,?)`,
			gid, board, season, pid, score, "above quarantine threshold", ms); err != nil {
			return "", nil, err
		}
		if exists {
			return "quarantined", &cur, nil
		}
		return "quarantined", nil, nil
	}
	improves := !exists || def.Mode == "sum" || (def.Order == "desc" && candidate > cur) || (def.Order == "asc" && candidate < cur)
	if improves {
		if _, err := tx.ExecContext(ctx, `INSERT INTO scores(game_id,board_id,season_id,player_id,score,achieved_at,status,updated_at)
			VALUES(?,?,?,?,?,?,'accepted',?) ON CONFLICT(game_id,board_id,season_id,player_id) DO UPDATE SET
			score=excluded.score, achieved_at=excluded.achieved_at, updated_at=excluded.updated_at`, gid, board, season, pid, candidate, ms, ms); err != nil {
			return "", nil, err
		}
		cur = candidate
	}
	return "accepted", &cur, nil
}
