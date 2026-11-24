package game

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/jobs"
)

// JobSettleSeason is the job type that settles one ended season.
const JobSettleSeason = "game.settle_season"

// settleGrace is how long after a season ends it may be settled, so score
// submissions that computed the old season just before the boundary have committed.
const settleGrace = time.Minute

const settleBatch = 100

// SettlementResult is the state of a season settlement.
type SettlementResult struct {
	GameID         string `json:"game_id"`
	Board          string `json:"board"`
	Season         string `json:"season"`
	Status         string `json:"status"` // running | completed
	ConfigRevision int64  `json:"config_revision"`
	PaidPlayers    int64  `json:"paid_players"`
	CreatedAt      int64  `json:"created_at"`
	CompletedAt    *int64 `json:"completed_at"`
}

// seasonBounds returns the [start,end) UTC interval of a daily/weekly season id.
func seasonBounds(period, season string) (time.Time, time.Time, bool) {
	switch period {
	case "daily":
		t, err := time.Parse("2006-01-02", season)
		if err != nil || SeasonID("daily", t) != season {
			return time.Time{}, time.Time{}, false
		}
		return t, t.AddDate(0, 0, 1), true
	case "weekly":
		var y, w int
		if n, err := fmt.Sscanf(season, "%04d-W%02d", &y, &w); n != 2 || err != nil || w < 1 || w > 53 {
			return time.Time{}, time.Time{}, false
		}
		jan4 := time.Date(y, 1, 4, 0, 0, 0, 0, time.UTC)
		start := jan4.AddDate(0, 0, -((int(jan4.Weekday())+6)%7)+(w-1)*7)
		if SeasonID("weekly", start) != season {
			return time.Time{}, time.Time{}, false
		}
		return start, start.AddDate(0, 0, 7), true
	}
	return time.Time{}, time.Time{}, false
}

func loadSettlement(ctx context.Context, q queryer, gameID, board, season string) (SettlementResult, error) {
	r := SettlementResult{GameID: gameID, Board: board, Season: season}
	err := q.QueryRowContext(ctx, `SELECT status, config_revision, paid_players, created_at, completed_at FROM season_settlements
		WHERE game_id=? AND board_id=? AND season_id=?`, gameID, board, season).Scan(&r.Status, &r.ConfigRevision, &r.PaidPlayers, &r.CreatedAt, &r.CompletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, httpx.NotFound("settlement not found")
	}
	return r, err
}

type rankedScore struct {
	rank     int64
	playerID string
	score    int64
}

// SettleSeason pays the board's rank rewards for an ended season exactly once.
// The settlement row (unique per game, board, season) pins the config revision;
// each payout row and its ledger source key season:<board>:<season>:<player> are
// unique, so concurrent or repeated runs and resumption after a crash never pay
// twice. Ranks follow getBoard's stable order over accepted scores only, so
// quarantined submissions are never rewarded.
func (k *Kit) SettleSeason(ctx context.Context, gameID, board, season string) (SettlementResult, error) {
	if !idPattern.MatchString(gameID) || !idPattern.MatchString(board) || len(season) > 16 {
		return SettlementResult{}, httpx.NotFound("board not found")
	}
	var rev int64
	var done bool
	err := k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		cur, err := loadSettlement(ctx, tx, gameID, board, season)
		if err == nil {
			rev, done = cur.ConfigRevision, cur.Status == "completed"
			return nil
		}
		var he *httpx.Error
		if !errors.As(err, &he) {
			return err
		}
		c, err := k.currentConfig(ctx, tx, gameID)
		if err != nil {
			return err
		}
		def, ok := c.Doc.Boards[board]
		if !ok {
			return httpx.NotFound("board not found")
		}
		if def.Settlement == nil {
			return httpx.Conflict("settlement_not_configured", "board has no settlement rewards")
		}
		_, end, ok := seasonBounds(def.Period, season)
		if !ok {
			return httpx.BadRequest("invalid season for a " + def.Period + " board")
		}
		now := k.Now()
		if now.Before(end.Add(settleGrace)) {
			e := httpx.Conflict("season_not_ended", "season has not ended")
			e.Details = map[string]any{"settle_after": end.Add(settleGrace).UnixMilli()}
			return e
		}
		rev, done = c.Revision, false
		_, err = tx.ExecContext(ctx, `INSERT INTO season_settlements(game_id,board_id,season_id,status,config_revision,created_at,updated_at) VALUES(?,?,?,'running',?,?,?)`,
			gameID, board, season, rev, now.UnixMilli(), now.UnixMilli())
		return err
	})
	if err != nil {
		return SettlementResult{}, err
	}
	if done {
		return loadSettlement(ctx, k.DB.Read, gameID, board, season)
	}
	c, err := k.configAt(ctx, k.DB.Read, gameID, rev)
	if err != nil {
		return SettlementResult{}, err
	}
	def := c.Doc.Boards[board]
	rewards := def.Settlement.Rewards
	maxRank := rewards[len(rewards)-1].ToRank
	_, orderBy := ordering(def.Order)
	rows, err := k.DB.Read.QueryContext(ctx, `SELECT player_id, score FROM scores WHERE game_id=? AND board_id=? AND season_id=? AND status='accepted'
		ORDER BY `+orderBy+` LIMIT ?`, gameID, board, season, maxRank)
	if err != nil {
		return SettlementResult{}, err
	}
	var ranked []rankedScore
	for rows.Next() {
		e := rankedScore{rank: int64(len(ranked)) + 1}
		if err := rows.Scan(&e.playerID, &e.score); err != nil {
			rows.Close()
			return SettlementResult{}, err
		}
		ranked = append(ranked, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return SettlementResult{}, err
	}
	for i := 0; i < len(ranked); i += settleBatch {
		batch := ranked[i:min(i+settleBatch, len(ranked))]
		if err := k.DB.WriteTx(ctx, func(tx *sql.Tx) error { return k.payBatch(ctx, tx, c.Doc, gameID, board, season, rewards, batch) }); err != nil {
			return SettlementResult{}, mapWriteErr(err)
		}
	}
	err = k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		now := k.Now().UnixMilli()
		_, err := tx.ExecContext(ctx, `UPDATE season_settlements SET status='completed', completed_at=?, updated_at=?,
			paid_players=(SELECT count(*) FROM season_payouts WHERE game_id=? AND board_id=? AND season_id=?)
			WHERE game_id=? AND board_id=? AND season_id=? AND status='running'`, now, now, gameID, board, season, gameID, board, season)
		return err
	})
	if err != nil {
		return SettlementResult{}, mapWriteErr(err)
	}
	if k.Metrics != nil {
		k.Metrics.Counter("game_season_settlements_total", "").Add(1)
	}
	return loadSettlement(ctx, k.DB.Read, gameID, board, season)
}

func (k *Kit) payBatch(ctx context.Context, tx *sql.Tx, doc Document, gameID, board, season string, rewards []RankReward, batch []rankedScore) error {
	now := k.Now().UnixMilli()
	for _, e := range batch {
		var grants map[string]int64
		for _, rr := range rewards {
			if e.rank >= rr.FromRank && e.rank <= rr.ToRank {
				grants = rr.Grants
				break
			}
		}
		if grants == nil {
			continue
		}
		// Stack caps clamp settlement grants instead of failing the whole settlement.
		granted, err := clampStacks(ctx, tx, doc, gameID, e.playerID, grants)
		if err != nil {
			return err
		}
		gj, _ := json.Marshal(granted)
		res, err := tx.ExecContext(ctx, `INSERT INTO season_payouts(game_id,board_id,season_id,player_id,rank,score,grants,created_at) VALUES(?,?,?,?,?,?,?,?)
			ON CONFLICT DO NOTHING`, gameID, board, season, e.playerID, e.rank, e.score, string(gj), now)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // already paid by an earlier or concurrent run
		}
		if err := ApplyLedger(ctx, tx, gameID, e.playerID, "season", "season:"+board+":"+season+":"+e.playerID, granted, now); err != nil {
			return err
		}
	}
	return nil
}

// ScheduleSettlements enqueues settlement jobs for the season that most recently
// ended on every daily/weekly board with settlement rewards in active games.
// Dedupe keys make repeated scheduling harmless; it returns how many jobs were
// newly enqueued. Call it periodically (e.g. every few minutes).
func (k *Kit) ScheduleSettlements(ctx context.Context, q *jobs.Queue) (int, error) {
	type cand struct {
		tenant, game, board, season string
		runAt                       time.Time
	}
	rows, err := k.DB.Read.QueryContext(ctx, `SELECT game_id, tenant_id FROM games WHERE status='active' ORDER BY game_id`)
	if err != nil {
		return 0, err
	}
	var games [][2]string
	for rows.Next() {
		var g [2]string
		if err := rows.Scan(&g[0], &g[1]); err != nil {
			rows.Close()
			return 0, err
		}
		games = append(games, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	now := k.Now()
	var cands []cand
	for _, g := range games {
		c, err := k.currentConfig(ctx, k.DB.Read, g[0])
		if err != nil {
			return 0, err
		}
		for id, def := range c.Doc.Boards {
			if def.Settlement == nil {
				continue
			}
			back := 24 * time.Hour
			if def.Period == "weekly" {
				back = 7 * 24 * time.Hour
			}
			season := SeasonID(def.Period, now.Add(-back))
			_, end, _ := seasonBounds(def.Period, season)
			var status string
			err := k.DB.Read.QueryRowContext(ctx, `SELECT status FROM season_settlements WHERE game_id=? AND board_id=? AND season_id=?`, g[0], id, season).Scan(&status)
			if err == nil && status == "completed" {
				continue
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, err
			}
			runAt := end.Add(settleGrace)
			if runAt.Before(now) {
				runAt = now
			}
			cands = append(cands, cand{g[1], g[0], id, season, runAt})
		}
	}
	if len(cands) == 0 {
		return 0, nil
	}
	var n int
	err = k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		n = 0
		for _, c := range cands {
			key := c.game + ":" + c.board + ":" + c.season
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE tenant_id=? AND type=? AND dedupe_key=?`, c.tenant, JobSettleSeason, key).Scan(&exists); err != nil {
				return err
			}
			if exists > 0 {
				continue
			}
			payload, _ := json.Marshal(settleJob{GameID: c.game, Board: c.board, Season: c.season})
			if _, err := q.Enqueue(ctx, tx, jobs.Job{TenantID: c.tenant, Type: JobSettleSeason, Payload: payload, RunAt: c.runAt, DedupeKey: key, MaxAttempts: 10}); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, mapWriteErr(err)
}

type settleJob struct {
	GameID string `json:"game_id"`
	Board  string `json:"board"`
	Season string `json:"season"`
}

// JobHandlers returns GameKit job handlers for the jobs.Worker.
func (k *Kit) JobHandlers() map[string]jobs.Handler {
	return map[string]jobs.Handler{
		JobSettleSeason: func(ctx context.Context, job *jobs.Claimed) error {
			var p settleJob
			if err := json.Unmarshal(job.Payload, &p); err != nil {
				k.Log.Error("bad settle job payload", "job", job.ID, "err", err)
				return nil // not retryable
			}
			r, err := k.SettleSeason(ctx, p.GameID, p.Board, p.Season)
			var he *httpx.Error
			if errors.As(err, &he) && he.Status < 500 {
				// Configuration changed (board removed, settlement disabled...): retrying cannot help.
				k.Log.Warn("season settlement skipped", "game", p.GameID, "board", p.Board, "season", p.Season, "err", err)
				return nil
			}
			if err != nil {
				return err
			}
			k.Log.Info("season settled", "game", p.GameID, "board", p.Board, "season", p.Season, "paid", r.PaidPlayers)
			return nil
		},
	}
}

func (k *Kit) adminSettle(w http.ResponseWriter, r *http.Request) error {
	g := CurrentGame(r.Context())
	res, err := k.SettleSeason(r.Context(), g.ID, r.PathValue("board"), r.PathValue("season"))
	if err != nil {
		return mapWriteErr(err)
	}
	k.Log.InfoContext(r.Context(), "season settled", "game", g.ID, "board", res.Board, "season", res.Season, "by", AdminKey(r.Context()).ID)
	httpx.WriteJSON(w, http.StatusOK, res)
	return nil
}

type payout struct {
	Rank     int64            `json:"rank"`
	PlayerID string           `json:"player_id"`
	Score    int64            `json:"score"`
	Grants   map[string]int64 `json:"grants"`
}

func (k *Kit) adminGetSettlement(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	g := CurrentGame(ctx)
	board, season := r.PathValue("board"), r.PathValue("season")
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		var err error
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 1000 {
			return httpx.BadRequest("limit must be within [1,1000]")
		}
	}
	out := struct {
		SettlementResult
		Payouts []payout `json:"payouts"`
	}{Payouts: []payout{}}
	err := k.DB.ReadTx(ctx, func(tx *sql.Tx) error {
		var err error
		if out.SettlementResult, err = loadSettlement(ctx, tx, g.ID, board, season); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT rank, player_id, score, grants FROM season_payouts WHERE game_id=? AND board_id=? AND season_id=? ORDER BY rank LIMIT ?`,
			g.ID, board, season, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p payout
			var gj string
			if err := rows.Scan(&p.Rank, &p.PlayerID, &p.Score, &gj); err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(gj), &p.Grants); err != nil {
				return err
			}
			out.Payouts = append(out.Payouts, p)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}
