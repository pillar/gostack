package game

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/httpx"
)

// startRun issues a one-time run token for a board (G08). Only its SHA-256 is
// stored; the score submission consumes it.
func (k *Kit) startRun(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := sessionFrom(ctx)
	board := r.PathValue("board")
	if err := k.rateLimit(k.scoreLimit, "run:"+s.GameID+":"+s.PlayerID); err != nil {
		return err
	}
	def, _, err := k.boardDef(ctx, k.DB.Read, s.GameID, board)
	if err != nil {
		return err
	}
	tok := httpx.RandomID("run_")
	started := k.Now().UnixMilli()
	expires := started + def.runTTL()
	err = k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO score_runs(token_hash,game_id,board_id,player_id,started_at,expires_at) VALUES(?,?,?,?,?,?)`,
			auth.Hash(tok), s.GameID, board, s.PlayerID, started, expires)
		return err
	})
	if err != nil {
		return mapWriteErr(err)
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"run_token": tok, "board": board, "started_at": started, "expires_at": expires})
	return nil
}

// consumeRun marks a run token used and returns the elapsed run time.
func (k *Kit) consumeRun(ctx context.Context, tx *sql.Tx, s *session, board, tok string, now int64) (int64, error) {
	var gameID, boardID, playerID string
	var started, expires int64
	var used sql.NullInt64
	h := auth.Hash(tok)
	err := tx.QueryRowContext(ctx, `SELECT game_id, board_id, player_id, started_at, expires_at, used_at FROM score_runs WHERE token_hash=?`, h).
		Scan(&gameID, &boardID, &playerID, &started, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (gameID != s.GameID || boardID != board || playerID != s.PlayerID)) {
		return 0, httpx.Conflict("run_token_invalid", "run token is not valid for this player and board")
	}
	if err != nil {
		return 0, err
	}
	if used.Valid {
		return 0, httpx.Conflict("run_token_used", "run token was already used")
	}
	if now > expires {
		return 0, httpx.Conflict("run_token_expired", "run token expired")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE score_runs SET used_at=? WHERE token_hash=? AND used_at IS NULL`, now, h); err != nil {
		return 0, err
	}
	return now - started, nil
}

// PurgeRuns deletes run tokens that expired more than a day ago (call from a
// maintenance loop); used tokens are kept until then so replays stay detectable.
func (k *Kit) PurgeRuns(ctx context.Context) (int64, error) {
	var n int64
	err := k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM score_runs WHERE expires_at<?`, k.Now().Add(-24*time.Hour).UnixMilli())
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

type quarantineItem struct {
	ID         int64   `json:"id"`
	Board      string  `json:"board"`
	Season     string  `json:"season"`
	PlayerID   string  `json:"player_id"`
	Score      int64   `json:"score"`
	Reason     string  `json:"reason"`
	CreatedAt  int64   `json:"created_at"`
	Review     string  `json:"review"`
	ReviewedBy *string `json:"reviewed_by"`
	ReviewedAt *int64  `json:"reviewed_at"`
}

const quarantineCols = `id, board_id, season_id, player_id, score, reason, created_at, review, reviewed_by, reviewed_at`

func scanQuarantine(sc interface{ Scan(...any) error }) (quarantineItem, error) {
	var q quarantineItem
	err := sc.Scan(&q.ID, &q.Board, &q.Season, &q.PlayerID, &q.Score, &q.Reason, &q.CreatedAt, &q.Review, &q.ReviewedBy, &q.ReviewedAt)
	return q, err
}

// adminListQuarantine pages quarantined submissions by id (oldest first).
func (k *Kit) adminListQuarantine(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	g := CurrentGame(ctx)
	q := r.URL.Query()
	status := q.Get("status")
	if status == "" {
		status = "pending"
	}
	if status != "pending" && status != "approved" && status != "rejected" {
		return httpx.BadRequest("status must be pending, approved or rejected")
	}
	limit, after := 50, int64(0)
	var err error
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 200 {
			return httpx.BadRequest("limit must be within [1,200]")
		}
	}
	if v := q.Get("after"); v != "" {
		if after, err = strconv.ParseInt(v, 10, 64); err != nil || after < 0 {
			return httpx.BadRequest("invalid after")
		}
	}
	rows, err := k.DB.Read.QueryContext(ctx, `SELECT `+quarantineCols+` FROM score_quarantine WHERE game_id=? AND review=? AND id>? ORDER BY id LIMIT ?`,
		g.ID, status, after, limit+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := struct {
		Items     []quarantineItem `json:"items"`
		NextAfter *int64           `json:"next_after"`
	}{Items: []quarantineItem{}}
	for rows.Next() {
		it, err := scanQuarantine(rows)
		if err != nil {
			return err
		}
		out.Items = append(out.Items, it)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		out.NextAfter = &out.Items[limit-1].ID
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// adminReviewQuarantine approves (applying the score to its original season with
// the board's best/sum rule) or rejects a quarantined submission. Repeating the
// same decision is a no-op; the reviewer's API key id is recorded.
func (k *Kit) adminReviewQuarantine(decision string) httpx.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		ctx := r.Context()
		g := CurrentGame(ctx)
		key := AdminKey(ctx)
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id < 1 {
			return httpx.NotFound("quarantine entry not found")
		}
		var item quarantineItem
		var replayed bool
		var best *int64
		err = k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
			replayed, best = false, nil
			it, err := scanQuarantine(tx.QueryRowContext(ctx, `SELECT `+quarantineCols+` FROM score_quarantine WHERE game_id=? AND id=?`, g.ID, id))
			if errors.Is(err, sql.ErrNoRows) {
				return httpx.NotFound("quarantine entry not found")
			}
			if err != nil {
				return err
			}
			item = it
			if item.Review == decision {
				replayed = true
				return nil
			}
			if item.Review != "pending" {
				return httpx.Conflict("already_reviewed", "entry was already "+item.Review)
			}
			if decision == "approved" {
				var n int
				if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM season_settlements WHERE game_id=? AND board_id=? AND season_id=?`,
					g.ID, item.Board, item.Season).Scan(&n); err != nil {
					return err
				}
				if n > 0 {
					return httpx.Conflict("season_settled", "the season was already settled")
				}
				c, err := k.currentConfig(ctx, tx, g.ID)
				if err != nil {
					return err
				}
				def, ok := c.Doc.Boards[item.Board]
				if !ok {
					return httpx.Conflict("board_removed", "board is no longer configured")
				}
				v, err := applyScore(ctx, tx, def, g.ID, item.Board, item.Season, item.PlayerID, item.Score, item.CreatedAt)
				if err != nil {
					return err
				}
				best = &v
			}
			now := k.Now().UnixMilli()
			if _, err := tx.ExecContext(ctx, `UPDATE score_quarantine SET review=?, reviewed_by=?, reviewed_at=? WHERE game_id=? AND id=? AND review='pending'`,
				decision, key.ID, now, g.ID, id); err != nil {
				return err
			}
			item.Review, item.ReviewedBy, item.ReviewedAt = decision, &key.ID, &now
			return nil
		})
		if err != nil {
			return mapWriteErr(err)
		}
		if !replayed {
			k.Log.InfoContext(ctx, "quarantine reviewed", "game", g.ID, "id", id, "decision", decision, "by", key.ID)
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"entry": item, "best": best, "replayed": replayed})
		return nil
	}
}
