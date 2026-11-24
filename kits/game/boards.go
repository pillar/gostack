package game

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/idempotency"
)

// SeasonID derives the season from the board period in UTC.
func SeasonID(period string, t time.Time) string {
	t = t.UTC()
	switch period {
	case "daily":
		return t.Format("2006-01-02")
	case "weekly":
		y, w := t.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	}
	return "all"
}

type entry struct {
	Rank       int64  `json:"rank"`
	PlayerID   string `json:"player_id"`
	Score      int64  `json:"score"`
	AchievedAt int64  `json:"achieved_at"`
}

type cursor struct {
	Score      int64  `json:"s"`
	AchievedAt int64  `json:"a"`
	PlayerID   string `json:"p"`
	Rank       int64  `json:"r"`
}

func (k *Kit) boardDef(ctx context.Context, q queryer, gameID, board string) (BoardDef, int64, error) {
	if !idPattern.MatchString(board) {
		return BoardDef{}, 0, httpx.NotFound("board not found")
	}
	c, err := k.currentConfig(ctx, q, gameID)
	if err != nil {
		return BoardDef{}, 0, err
	}
	def, ok := c.Doc.Boards[board]
	if !ok {
		return BoardDef{}, 0, httpx.NotFound("board not found")
	}
	return def, c.Revision, nil
}

// better returns the SQL predicate "row ranks strictly ahead of (?,?,?)" and the score ORDER BY.
func ordering(order string) (ahead, orderBy string) {
	cmp, dir := ">", "DESC"
	if order == "asc" {
		cmp, dir = "<", "ASC"
	}
	ahead = fmt.Sprintf(`(score %s ? OR (score = ? AND (achieved_at < ? OR (achieved_at = ? AND player_id < ?))))`, cmp)
	orderBy = fmt.Sprintf(`score %s, achieved_at ASC, player_id ASC`, dir)
	return
}

// getBoard returns a stable, cursor-paginated page and optionally the caller's rank (G04).
func (k *Kit) getBoard(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	g := gameFrom(ctx)
	board := r.PathValue("board")
	def, _, err := k.boardDef(ctx, k.DB.Read, g.ID, board)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 100 {
			return httpx.BadRequest("limit must be within [1,100]")
		}
	}
	season := q.Get("season")
	if season == "" {
		season = SeasonID(def.Period, k.Now())
	} else if len(season) > 16 {
		return httpx.BadRequest("invalid season")
	}
	ahead, orderBy := ordering(def.Order)
	args := []any{g.ID, board, season}
	where := `game_id=? AND board_id=? AND season_id=? AND status='accepted'`
	var start int64
	if c := q.Get("cursor"); c != "" {
		var cur cursor
		b, err := base64.RawURLEncoding.DecodeString(c)
		if err != nil || json.Unmarshal(b, &cur) != nil || cur.Rank < 0 {
			return httpx.BadRequest("invalid cursor")
		}
		// Rows after the cursor = rows NOT ahead of it and not the cursor row itself.
		where += ` AND NOT ` + ahead + ` AND NOT (score = ? AND achieved_at = ? AND player_id = ?)`
		args = append(args, cur.Score, cur.Score, cur.AchievedAt, cur.AchievedAt, cur.PlayerID, cur.Score, cur.AchievedAt, cur.PlayerID)
		start = cur.Rank
	}
	var out struct {
		Board      string  `json:"board"`
		Season     string  `json:"season"`
		Entries    []entry `json:"entries"`
		NextCursor *string `json:"next_cursor"`
		Me         *struct {
			Rank  int64 `json:"rank"`
			Score int64 `json:"score"`
		} `json:"me,omitempty"`
	}
	out.Board, out.Season, out.Entries = board, season, []entry{}
	err = k.DB.ReadTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT player_id, score, achieved_at FROM scores WHERE `+where+` ORDER BY `+orderBy+` LIMIT ?`, append(args, limit+1)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e entry
			if err := rows.Scan(&e.PlayerID, &e.Score, &e.AchievedAt); err != nil {
				return err
			}
			e.Rank = start + int64(len(out.Entries)) + 1
			out.Entries = append(out.Entries, e)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(out.Entries) > limit {
			out.Entries = out.Entries[:limit]
			last := out.Entries[limit-1]
			b, _ := json.Marshal(cursor{Score: last.Score, AchievedAt: last.AchievedAt, PlayerID: last.PlayerID, Rank: last.Rank})
			s := base64.RawURLEncoding.EncodeToString(b)
			out.NextCursor = &s
		}
		if sess := sessionFrom(ctx); sess != nil {
			var score, at int64
			err := tx.QueryRowContext(ctx, `SELECT score, achieved_at FROM scores WHERE game_id=? AND board_id=? AND season_id=? AND player_id=? AND status='accepted'`,
				g.ID, board, season, sess.PlayerID).Scan(&score, &at)
			if err == nil {
				// Exact rank via an index range count; see PRD review D5 for the precomputed fallback.
				var n int64
				if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM scores WHERE game_id=? AND board_id=? AND season_id=? AND status='accepted' AND `+ahead,
					g.ID, board, season, score, score, at, at, sess.PlayerID).Scan(&n); err != nil {
					return err
				}
				out.Me = &struct {
					Rank  int64 `json:"rank"`
					Score int64 `json:"score"`
				}{n + 1, score}
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// submitScore validates against the board rules; implausible scores are
// quarantined and never ranked or rewarded automatically (G08).
func (k *Kit) submitScore(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := sessionFrom(ctx)
	board := r.PathValue("board")
	if err := k.rateLimit(k.scoreLimit, "score:"+s.GameID+":"+s.PlayerID); err != nil {
		return err
	}
	body, err := readBody(r, 4096)
	if err != nil {
		return err
	}
	var req struct {
		Score       *int64 `json:"score"`
		RuleVersion int64  `json:"rule_version"`
		RunToken    string `json:"run_token"`
	}
	if err := decodeStrict(body, &req); err != nil {
		return err
	}
	if req.Score == nil {
		return httpx.BadRequest("score required")
	}
	def, _, err := k.boardDef(ctx, k.DB.Read, s.GameID, board)
	if err != nil {
		return err
	}
	if def.RuleVersion > 0 && req.RuleVersion != def.RuleVersion {
		e := httpx.Conflict("rule_version_mismatch", "board rules changed; update the client")
		e.Details = map[string]any{"rule_version": def.RuleVersion}
		return e
	}
	if def.RequireRun && req.RunToken == "" {
		return httpx.BadRequest("run_token required for this board")
	}
	if len(req.RunToken) > 128 {
		return httpx.BadRequest("invalid run_token")
	}
	score := *req.Score
	if score < def.MinScore || score > def.MaxScore {
		return httpx.BadRequest(fmt.Sprintf("score must be within [%d,%d]", def.MinScore, def.MaxScore))
	}
	now := k.Now()
	season := SeasonID(def.Period, now)
	status, resp, replayed, err := idempotency.Do(ctx, k.DB, "game:"+s.GameID+":"+s.PlayerID, r.Header.Get("Idempotency-Key"), "score.submit:"+board, body,
		func(tx *sql.Tx) (idempotency.Result, error) {
			type result struct {
				Status string `json:"status"`
				Best   *int64 `json:"best"`
				Season string `json:"season"`
			}
			res := result{Status: "accepted", Season: season}
			reason := ""
			if req.RunToken != "" {
				elapsed, err := k.consumeRun(ctx, tx, s, board, req.RunToken, now.UnixMilli())
				if err != nil {
					return idempotency.Result{}, err
				}
				if elapsed < def.MinRunMs {
					reason = "run shorter than min_run_ms"
				}
			}
			if def.QuarantineAbove != nil && score > *def.QuarantineAbove { // per submission, not the accumulated sum
				reason = "above quarantine threshold"
			}
			if reason != "" {
				if _, err := tx.ExecContext(ctx, `INSERT INTO score_quarantine(game_id,board_id,season_id,player_id,score,reason,created_at) VALUES(?,?,?,?,?,?,?)`,
					s.GameID, board, season, s.PlayerID, score, reason, now.UnixMilli()); err != nil {
					return idempotency.Result{}, err
				}
				if k.Metrics != nil {
					k.Metrics.Counter("game_scores_quarantined_total", "").Add(1)
				}
				res.Status = "quarantined"
				var cur int64
				err := tx.QueryRowContext(ctx, `SELECT score FROM scores WHERE game_id=? AND board_id=? AND season_id=? AND player_id=?`, s.GameID, board, season, s.PlayerID).Scan(&cur)
				if err == nil {
					res.Best = &cur
				} else if !errors.Is(err, sql.ErrNoRows) {
					return idempotency.Result{}, err
				}
				return idempotency.Result{Body: res}, nil
			}
			best, err := applyScore(ctx, tx, def, s.GameID, board, season, s.PlayerID, score, now.UnixMilli())
			if err != nil {
				return idempotency.Result{}, err
			}
			res.Best = &best
			return idempotency.Result{Body: res}, nil
		})
	if err != nil {
		return mapWriteErr(err)
	}
	idempotency.Write(w, status, resp, replayed)
	return nil
}

// applyScore records an accepted score respecting the board mode (best keeps
// the better value, sum accumulates) and returns the player's resulting score.
func applyScore(ctx context.Context, tx *sql.Tx, def BoardDef, gameID, board, season, playerID string, score, at int64) (int64, error) {
	var cur int64
	err := tx.QueryRowContext(ctx, `SELECT score FROM scores WHERE game_id=? AND board_id=? AND season_id=? AND player_id=?`, gameID, board, season, playerID).Scan(&cur)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	candidate := score
	if def.Mode == "sum" && exists {
		candidate = cur + score
	}
	improves := !exists || def.Mode == "sum" ||
		(def.Order == "desc" && candidate > cur) || (def.Order == "asc" && candidate < cur)
	if !improves {
		return cur, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO scores(game_id,board_id,season_id,player_id,score,achieved_at,status,updated_at)
		VALUES(?,?,?,?,?,?,'accepted',?) ON CONFLICT(game_id,board_id,season_id,player_id) DO UPDATE SET
		score=excluded.score, achieved_at=excluded.achieved_at, updated_at=excluded.updated_at`,
		gameID, board, season, playerID, candidate, at, at); err != nil {
		return 0, err
	}
	return candidate, nil
}
