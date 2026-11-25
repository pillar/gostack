package ops

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/modules/idempotency"
)

type checkinConfig struct {
	Cycle       []Grants `json:"cycle"`
	Timezone    string   `json:"timezone"`
	StreakReset bool     `json:"streak_reset"`
}

func (m *Module) adminPutCheckin(w http.ResponseWriter, r *http.Request) error {
	var c checkinConfig
	if err := decodeBody(r, 64<<10, &c); err != nil {
		return err
	}
	if len(c.Cycle) == 0 || len(c.Cycle) > 366 {
		return httpx.BadRequest("cycle must contain 1-366 days")
	}
	for _, g := range c.Cycle {
		if err := validGrants(g, true); err != nil {
			return err
		}
	}
	if _, err := loadZone(c.Timezone); err != nil {
		return err
	}
	if c.Timezone == "" {
		c.Timezone = "UTC"
	}
	g := game.CurrentGame(r.Context())
	err := m.DB.WriteTx(r.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(r.Context(), `INSERT INTO ops_checkin_config(game_id,cycle,timezone,streak_reset,updated_at) VALUES(?,?,?,?,?)
			ON CONFLICT(game_id) DO UPDATE SET cycle=excluded.cycle, timezone=excluded.timezone, streak_reset=excluded.streak_reset, updated_at=excluded.updated_at`,
			g.ID, jsonString(c.Cycle), c.Timezone, boolInt(c.StreakReset), m.now().UnixMilli())
		return err
	})
	if err != nil {
		return game.MapWriteErr(err)
	}
	httpx.WriteJSON(w, http.StatusOK, c)
	return nil
}

func loadCheckinConfig(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, gameID string) (checkinConfig, error) {
	var c checkinConfig
	var cycle string
	var reset int
	err := q.QueryRowContext(ctx, `SELECT cycle,timezone,streak_reset FROM ops_checkin_config WHERE game_id=?`, gameID).Scan(&cycle, &c.Timezone, &reset)
	if errors.Is(err, sql.ErrNoRows) {
		return c, httpx.NotFound("check-in is not configured")
	}
	if err != nil {
		return c, err
	}
	c.StreakReset = reset == 1
	return c, json.Unmarshal([]byte(cycle), &c.Cycle)
}

func (m *Module) adminGetCheckin(w http.ResponseWriter, r *http.Request) error {
	c, err := loadCheckinConfig(r.Context(), m.DB.Read, game.CurrentGame(r.Context()).ID)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, c)
	return nil
}

type checkinState struct {
	Streak, Total int64
	LastDay       string // local date of the last claim in the timezone of that time
	LastAt        int64  // epoch ms of the last claim; 0 for rows written before migration 0012
}

func loadCheckinState(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, gameID, playerID string) (checkinState, error) {
	var s checkinState
	err := q.QueryRowContext(ctx, `SELECT streak,total,last_day,last_at FROM ops_checkin_state WHERE game_id=? AND player_id=?`, gameID, playerID).Scan(&s.Streak, &s.Total, &s.LastDay, &s.LastAt)
	if errors.Is(err, sql.ErrNoRows) {
		return checkinState{}, nil
	}
	return s, err
}

const dayLayout = "2006-01-02"

// lastLocalDay is the local date of the last claim in loc (the CURRENT
// timezone): comparing it with today's date in the same zone means an admin
// timezone change cannot create a new day inside the same real day. Rows from
// before last_at existed fall back to the stored date string.
func (s checkinState) lastLocalDay(loc *time.Location) string {
	if s.LastAt > 0 {
		return time.UnixMilli(s.LastAt).In(loc).Format(dayLayout)
	}
	return s.LastDay
}

// claimedToday reports whether the last claim falls on today's local date.
func (s checkinState) claimedToday(today string, loc *time.Location) bool {
	return s.LastDay != "" && s.lastLocalDay(loc) >= today
}

// effectiveStreak is the streak a claim today would extend: with streak_reset a
// gap of more than one local day (both dates in the current timezone) starts over.
func effectiveStreak(s checkinState, today string, loc *time.Location, reset bool) int64 {
	if s.LastDay == "" {
		return 0
	}
	if reset {
		a, err1 := time.Parse(dayLayout, s.lastLocalDay(loc))
		b, err2 := time.Parse(dayLayout, today)
		if err1 == nil && err2 == nil && b.Sub(a) > 24*time.Hour {
			return 0
		}
	}
	return s.Streak
}

func (m *Module) getCheckin(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	s := game.CurrentSession(ctx)
	c, err := loadCheckinConfig(ctx, m.DB.Read, s.GameID)
	if err != nil {
		return err
	}
	st, err := loadCheckinState(ctx, m.DB.Read, s.GameID, s.PlayerID)
	if err != nil {
		return err
	}
	loc := zoneOrUTC(c.Timezone)
	now := m.now().In(loc)
	today := now.Format(dayLayout)
	done := st.claimedToday(today, loc)
	streak := effectiveStreak(st, today, loc, c.StreakReset)
	idx := int(streak % int64(len(c.Cycle)))
	next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, loc)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"timezone": c.Timezone, "today": today, "checked_in_today": done, "streak": streak, "total": st.Total,
		"cycle_length": len(c.Cycle), "next_index": idx, "next_grants": c.Cycle[idx], "next_available_at": next.UnixMilli(),
	})
	return nil
}

// postCheckin claims the reward of the configured timezone's current local day,
// once per day; the day is part of the ledger source key. "Already claimed" is
// decided from the instant of the last claim, re-read in the current timezone.
func (m *Module) postCheckin(w http.ResponseWriter, r *http.Request) error {
	s := game.CurrentSession(r.Context())
	return m.idem(w, r, "game:"+s.GameID+":"+s.PlayerID, "checkin", nil, func(tx *sql.Tx) (idempotency.Result, error) {
		ctx := r.Context()
		c, err := loadCheckinConfig(ctx, tx, s.GameID)
		if err != nil {
			return idempotency.Result{}, err
		}
		now := m.now()
		loc := zoneOrUTC(c.Timezone)
		today := now.In(loc).Format(dayLayout)
		st, err := loadCheckinState(ctx, tx, s.GameID, s.PlayerID)
		if err != nil {
			return idempotency.Result{}, err
		}
		if st.claimedToday(today, loc) {
			return idempotency.Result{}, httpx.Conflict("already_checked_in", "already checked in today")
		}
		source := "checkin:" + today + ":" + s.PlayerID
		// After a timezone change toward the west the same local date can come
		// round again; its ledger key is already taken, so it counts as claimed.
		var used int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM game_ledger WHERE game_id=? AND source_key=?`, s.GameID, source).Scan(&used); err != nil {
			return idempotency.Result{}, err
		}
		if used > 0 {
			return idempotency.Result{}, httpx.Conflict("already_checked_in", "already checked in today")
		}
		streak := effectiveStreak(st, today, loc, c.StreakReset) + 1
		idx := int((streak - 1) % int64(len(c.Cycle)))
		if _, err := tx.ExecContext(ctx, `INSERT INTO ops_checkin_state(game_id,player_id,streak,total,last_day,last_at) VALUES(?,?,?,1,?,?)
			ON CONFLICT(game_id,player_id) DO UPDATE SET streak=excluded.streak, total=ops_checkin_state.total+1, last_day=excluded.last_day, last_at=excluded.last_at`,
			s.GameID, s.PlayerID, streak, today, now.UnixMilli()); err != nil {
			return idempotency.Result{}, err
		}
		if err := m.Kit.Grant(ctx, tx, s.GameID, s.PlayerID, "reward", source, c.Cycle[idx], now.UnixMilli()); err != nil {
			return idempotency.Result{}, err
		}
		return idempotency.Result{Body: map[string]any{"day": today, "streak": streak, "index": idx, "grants": c.Cycle[idx]}}, nil
	})
}
