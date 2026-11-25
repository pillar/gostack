package ops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/modules/idempotency"
)

type taskReq struct {
	Title    string `json:"title"`
	Metric   string `json:"metric"`
	Target   int64  `json:"target"`
	Period   string `json:"period"`
	Grants   Grants `json:"grants"`
	Timezone string `json:"timezone"`
	EventID  string `json:"event_id"`
}

type taskRow struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Metric   string `json:"metric"`
	Target   int64  `json:"target"`
	Period   string `json:"period"`
	Grants   Grants `json:"grants"`
	Timezone string `json:"timezone"`
	EventID  string `json:"event_id"`
}

func (m *Module) adminPutTask(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var req taskReq
	if err := decodeBody(r, 16<<10, &req); err != nil {
		return err
	}
	if !game.ValidID(req.Metric) {
		return httpx.BadRequest("metric must match [a-z0-9][a-z0-9_-]{0,31}")
	}
	if req.Target <= 0 || req.Target > 1_000_000_000_000 {
		return httpx.BadRequest("target must be a positive integer")
	}
	if req.Period != "once" && req.Period != "daily" && req.Period != "weekly" {
		return httpx.BadRequest("period must be once, daily or weekly")
	}
	if err := validGrants(req.Grants, true); err != nil {
		return err
	}
	if _, err := loadZone(req.Timezone); err != nil {
		return err
	}
	if req.Timezone == "" {
		req.Timezone = "UTC"
	}
	if req.EventID != "" && !game.ValidID(req.EventID) || len(req.Title) > 200 {
		return httpx.BadRequest("invalid event_id or title")
	}
	g := game.CurrentGame(r.Context())
	now := m.now().UnixMilli()
	err = m.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.Context(), `INSERT INTO ops_tasks(game_id,id,title,metric,target,period,grants,timezone,event_id,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(game_id,id) DO UPDATE SET title=excluded.title, metric=excluded.metric, target=excluded.target, period=excluded.period,
			  grants=excluded.grants, timezone=excluded.timezone, event_id=excluded.event_id, updated_at=excluded.updated_at`,
			g.ID, id, req.Title, req.Metric, req.Target, req.Period, jsonString(req.Grants), req.Timezone, req.EventID, now, now)
		return err
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, taskRow{id, req.Title, req.Metric, req.Target, req.Period, req.Grants, req.Timezone, req.EventID})
	return nil
}

func scanTask(sc interface{ Scan(...any) error }) (taskRow, error) {
	var t taskRow
	var grants string
	err := sc.Scan(&t.ID, &t.Title, &t.Metric, &t.Target, &t.Period, &grants, &t.Timezone, &t.EventID)
	t.Grants = parseGrants(grants)
	return t, err
}

const taskCols = `id,title,metric,target,period,grants,timezone,event_id`

func queryTasks(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, where string, args ...any) ([]taskRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+taskCols+` FROM ops_tasks WHERE `+where+` ORDER BY id LIMIT 200`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []taskRow{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (m *Module) adminListTasks(w http.ResponseWriter, r *http.Request) error {
	out, err := queryTasks(r.Context(), m.DB.Read, `game_id=?`, game.CurrentGame(r.Context()).ID)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"tasks": out})
	return nil
}

func (m *Module) adminDeleteTask(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	g := game.CurrentGame(r.Context())
	var n int64
	err = m.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM ops_task_progress WHERE game_id=? AND task_id=?`, g.ID, id); err != nil {
			return err
		}
		res, err := tx.ExecContext(r.Context(), `DELETE FROM ops_tasks WHERE game_id=? AND id=?`, g.ID, id)
		if err == nil {
			n, err = res.RowsAffected()
		}
		return err
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	if n == 0 {
		return httpx.NotFound("task not found")
	}
	return writeNoContent(w)
}

// periodKey identifies the current period of a task in its timezone.
func periodKey(period, tz string, now time.Time) string {
	t := now.In(zoneOrUTC(tz))
	switch period {
	case "daily":
		return t.Format(dayLayout)
	case "weekly":
		y, w := t.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	}
	return "once"
}

// Progress adds delta to every task counting metric for the player, inside the
// caller's transaction (so game code can advance progress atomically with its own
// state change). It is the only way progress is written: clients cannot post
// progress. Tasks gated by an inactive event are skipped. fn must be re-runnable,
// like any WriteTx body; Progress itself holds no state.
func (m *Module) Progress(ctx context.Context, tx *sql.Tx, gameID, playerID, metric string, delta int64) error {
	_, err := m.progress(ctx, tx, gameID, playerID, metric, delta)
	return err
}

func (m *Module) progress(ctx context.Context, tx *sql.Tx, gameID, playerID, metric string, delta int64) (int, error) {
	if delta <= 0 || delta > 1_000_000_000 {
		return 0, errors.New("ops: progress delta must be between 1 and 1e9")
	}
	tasks, err := queryTasks(ctx, tx, `game_id=? AND metric=?`, gameID, metric)
	if err != nil {
		return 0, err
	}
	now := m.now()
	n := 0
	for _, t := range tasks {
		if t.EventID != "" {
			ok, err := eventActive(ctx, tx, gameID, t.EventID, now.UnixMilli())
			if err != nil {
				return n, err
			}
			if !ok {
				continue
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ops_task_progress(game_id,player_id,task_id,period_key,progress) VALUES(?,?,?,?,?)
			ON CONFLICT(game_id,player_id,task_id,period_key) DO UPDATE SET progress=CASE WHEN ops_task_progress.progress+excluded.progress > 1000000000000000 THEN 1000000000000000
				ELSE ops_task_progress.progress+excluded.progress END`,
			gameID, playerID, t.ID, periodKey(t.Period, t.Timezone, now), delta); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// adminProgress lets tooling advance progress for a player (game servers use Module.Progress).
func (m *Module) adminProgress(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		PlayerID string `json:"player_id"`
		Metric   string `json:"metric"`
		Delta    int64  `json:"delta"`
	}
	if err := decodeBody(r, 4<<10, &req); err != nil {
		return err
	}
	if !game.ValidID(req.Metric) || req.PlayerID == "" || req.Delta <= 0 || req.Delta > 1_000_000_000 {
		return httpx.BadRequest("player_id, metric and a delta between 1 and 1e9 are required")
	}
	g := game.CurrentGame(r.Context())
	var n int
	err := m.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		if err := game.EnsurePlayer(r.Context(), tx, g.ID, req.PlayerID); err != nil {
			return err
		}
		var err error
		n, err = m.progress(r.Context(), tx, g.ID, req.PlayerID, req.Metric, req.Delta)
		return err
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"tasks_updated": n})
	return nil
}

func (m *Module) listTasks(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := game.CurrentSession(ctx)
	now := m.now()
	var out []map[string]any
	err := m.DB.ReadTx(ctx, func(tx *sql.Tx) error {
		tasks, err := queryTasks(ctx, tx, `game_id=?`, s.GameID)
		if err != nil {
			return err
		}
		out = []map[string]any{}
		for _, t := range tasks {
			if t.EventID != "" {
				ok, err := eventActive(ctx, tx, s.GameID, t.EventID, now.UnixMilli())
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
			}
			key := periodKey(t.Period, t.Timezone, now)
			var progress, claimed int64
			err := tx.QueryRowContext(ctx, `SELECT progress,claimed_at FROM ops_task_progress WHERE game_id=? AND player_id=? AND task_id=? AND period_key=?`,
				s.GameID, s.PlayerID, t.ID, key).Scan(&progress, &claimed)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			out = append(out, map[string]any{"id": t.ID, "title": t.Title, "metric": t.Metric, "target": t.Target, "period": t.Period,
				"grants": t.Grants, "period_key": key, "progress": min(progress, t.Target), "completed": progress >= t.Target,
				"claimed": claimed > 0, "claimable": progress >= t.Target && claimed == 0})
		}
		return nil
	})
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"tasks": out})
	return nil
}

// claimTask pays a completed task once per period (source key task:<id>:<period>:<player>).
func (m *Module) claimTask(w http.ResponseWriter, r *http.Request) error {
	s := game.CurrentSession(r.Context())
	id, err := pathID(r)
	if err != nil {
		return err
	}
	return m.idem(w, r, "game:"+s.GameID+":"+s.PlayerID, "task.claim:"+id, nil, func(tx *sql.Tx) (idempotency.Result, error) {
		ctx := r.Context()
		tasks, err := queryTasks(ctx, tx, `game_id=? AND id=?`, s.GameID, id)
		if err != nil {
			return idempotency.Result{}, err
		}
		if len(tasks) == 0 {
			return idempotency.Result{}, httpx.NotFound("task not found")
		}
		t := tasks[0]
		now := m.now()
		if t.EventID != "" {
			ok, err := eventActive(ctx, tx, s.GameID, t.EventID, now.UnixMilli())
			if err != nil {
				return idempotency.Result{}, err
			}
			if !ok {
				return idempotency.Result{}, httpx.Forbidden("task is not available now")
			}
		}
		key := periodKey(t.Period, t.Timezone, now)
		var progress, claimed int64
		err = tx.QueryRowContext(ctx, `SELECT progress,claimed_at FROM ops_task_progress WHERE game_id=? AND player_id=? AND task_id=? AND period_key=?`,
			s.GameID, s.PlayerID, id, key).Scan(&progress, &claimed)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return idempotency.Result{}, err
		}
		if progress < t.Target {
			return idempotency.Result{}, httpx.Conflict("not_completed", "task is not completed")
		}
		if claimed > 0 {
			return idempotency.Result{}, httpx.Conflict("already_claimed", "task already claimed for this period")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE ops_task_progress SET claimed_at=? WHERE game_id=? AND player_id=? AND task_id=? AND period_key=?`,
			now.UnixMilli(), s.GameID, s.PlayerID, id, key); err != nil {
			return idempotency.Result{}, err
		}
		if err := m.Kit.Grant(ctx, tx, s.GameID, s.PlayerID, "reward", "task:"+id+":"+key+":"+s.PlayerID, t.Grants, now.UnixMilli()); err != nil {
			return idempotency.Result{}, err
		}
		return idempotency.Result{Body: map[string]any{"task_id": id, "period_key": key, "grants": t.Grants}}, nil
	})
}
