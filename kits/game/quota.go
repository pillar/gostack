package game

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/ratelimit"
)

// Per-game resource quotas (G-A09, docs/api/quotas.md). One noisy game of a
// multi-game deployment must not exhaust the shared players table, save
// storage, request budget, analytics pipeline or write queue.
//
//   - max_players and save_bytes_total are enforced exactly: the counters in
//     game_usage change in the same write transaction as the player insert or
//     the save put/restore/delete, with a conditional UPDATE.
//   - requests_per_minute uses one rate limiter per game from the kit's
//     limiter factory (global under rate_limit.driver valkey).
//   - analytics_events_per_day and write_concurrency are per instance.
//
// Limits are read from game_quotas (NULL = configured default, 0 = unlimited)
// and cached per game for quotaTTL, so an admin change applies on this
// instance immediately and on other instances within quotaTTL.

// quotaTTL bounds how long an instance uses cached limits.
const quotaTTL = 5 * time.Second

// maxQuotaValue bounds every quota value (keeps arithmetic far from overflow).
const maxQuotaValue int64 = 1 << 50

// Quotas are a game's effective limits; 0 means unlimited.
type Quotas struct {
	MaxPlayers            int64 `json:"max_players"`
	RequestsPerMinute     int64 `json:"requests_per_minute"`
	SaveBytesTotal        int64 `json:"save_bytes_total"`
	AnalyticsEventsPerDay int64 `json:"analytics_events_per_day"`
	WriteConcurrency      int64 `json:"write_concurrency"`
}

// QuotaOverrides are the admin-set values of one game; nil inherits the
// configured default (game.quota_defaults).
type QuotaOverrides struct {
	MaxPlayers            *int64 `json:"max_players"`
	RequestsPerMinute     *int64 `json:"requests_per_minute"`
	SaveBytesTotal        *int64 `json:"save_bytes_total"`
	AnalyticsEventsPerDay *int64 `json:"analytics_events_per_day"`
	WriteConcurrency      *int64 `json:"write_concurrency"`
}

// QuotaStatus is a game's quota configuration and stored usage.
type QuotaStatus struct {
	GameID    string         `json:"game_id"`
	Limits    Quotas         `json:"limits"`
	Overrides QuotaOverrides `json:"overrides"`
	Defaults  Quotas         `json:"defaults"`
	Usage     QuotaUsage     `json:"usage"`
}

// QuotaUsage: players and save_bytes come from game_usage (exact, all
// instances); the other two are this instance's counters (omitted by the CLI).
type QuotaUsage struct {
	Players              int64  `json:"players"`
	SaveBytes            int64  `json:"save_bytes"`
	AnalyticsEventsToday *int64 `json:"analytics_events_today,omitempty"`
	WritesInFlight       *int64 `json:"writes_in_flight,omitempty"`
}

func defaultQuotas(d config.GameQuotaDefaults) Quotas {
	return Quotas{MaxPlayers: d.MaxPlayers, RequestsPerMinute: d.RequestsPerMinute, SaveBytesTotal: d.SaveBytesTotal,
		AnalyticsEventsPerDay: d.AnalyticsEventsPerDay, WriteConcurrency: d.WriteConcurrency}
}

func (o QuotaOverrides) apply(d Quotas) Quotas {
	pick := func(p *int64, def int64) int64 {
		if p != nil {
			return *p
		}
		return def
	}
	return Quotas{MaxPlayers: pick(o.MaxPlayers, d.MaxPlayers), RequestsPerMinute: pick(o.RequestsPerMinute, d.RequestsPerMinute),
		SaveBytesTotal: pick(o.SaveBytesTotal, d.SaveBytesTotal), AnalyticsEventsPerDay: pick(o.AnalyticsEventsPerDay, d.AnalyticsEventsPerDay),
		WriteConcurrency: pick(o.WriteConcurrency, d.WriteConcurrency)}
}

func nullable(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

// LoadQuotaStatus reads a game's overrides and stored usage (404 when the game
// does not exist). Usage of a game without a game_usage row is counted.
func LoadQuotaStatus(ctx context.Context, q queryer, defaults config.GameQuotaDefaults, gameID string) (QuotaStatus, error) {
	st := QuotaStatus{GameID: gameID, Defaults: defaultQuotas(defaults)}
	var o [5]sql.NullInt64
	var players, bytes sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT q.max_players, q.requests_per_minute, q.save_bytes_total, q.analytics_events_per_day, q.write_concurrency,
		u.players, u.save_bytes
		FROM games g LEFT JOIN game_quotas q ON q.game_id=g.game_id LEFT JOIN game_usage u ON u.game_id=g.game_id
		WHERE g.game_id=?`, gameID).Scan(&o[0], &o[1], &o[2], &o[3], &o[4], &players, &bytes)
	if errors.Is(err, sql.ErrNoRows) {
		return st, httpx.NotFound("game not found")
	}
	if err != nil {
		return st, err
	}
	st.Overrides = QuotaOverrides{MaxPlayers: nullable(o[0]), RequestsPerMinute: nullable(o[1]), SaveBytesTotal: nullable(o[2]),
		AnalyticsEventsPerDay: nullable(o[3]), WriteConcurrency: nullable(o[4])}
	st.Limits = st.Overrides.apply(st.Defaults)
	if players.Valid {
		st.Usage.Players, st.Usage.SaveBytes = players.Int64, bytes.Int64
	} else if st.Usage.Players, st.Usage.SaveBytes, err = countUsage(ctx, q, gameID); err != nil {
		return st, err
	}
	return st, nil
}

func countUsage(ctx context.Context, q queryer, gameID string) (players, saveBytes int64, err error) {
	err = q.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM players WHERE game_id=?),
		(SELECT coalesce(sum(length(payload)),0) FROM save_slots WHERE game_id=?)`, gameID, gameID).Scan(&players, &saveBytes)
	return players, saveBytes, err
}

// SetQuotaOverrides replaces a game's overrides (nil fields inherit defaults).
func SetQuotaOverrides(ctx context.Context, db *store.DB, gameID string, o QuotaOverrides, by string) error {
	for _, v := range []*int64{o.MaxPlayers, o.RequestsPerMinute, o.SaveBytesTotal, o.AnalyticsEventsPerDay, o.WriteConcurrency} {
		if v != nil && (*v < 0 || *v > maxQuotaValue) {
			return httpx.BadRequest(fmt.Sprintf("quota values must be null (inherit the default) or within [0, %d] (0 = unlimited)", maxQuotaValue))
		}
	}
	if o.RequestsPerMinute != nil && *o.RequestsPerMinute > 1e9 {
		return httpx.BadRequest("requests_per_minute must be <= 1000000000")
	}
	return db.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO game_quotas(game_id,max_players,requests_per_minute,save_bytes_total,analytics_events_per_day,write_concurrency,updated_by,updated_at)
			VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(game_id) DO UPDATE SET max_players=excluded.max_players, requests_per_minute=excluded.requests_per_minute,
			save_bytes_total=excluded.save_bytes_total, analytics_events_per_day=excluded.analytics_events_per_day,
			write_concurrency=excluded.write_concurrency, updated_by=excluded.updated_by, updated_at=excluded.updated_at`,
			gameID, o.MaxPlayers, o.RequestsPerMinute, o.SaveBytesTotal, o.AnalyticsEventsPerDay, o.WriteConcurrency, by, time.Now().UnixMilli())
		return err
	})
}

// RecountUsage recomputes a game's game_usage row from players and save_slots
// (repair after rows were written outside GameKit, e.g. bulk imports).
func RecountUsage(ctx context.Context, db *store.DB, gameID string) (QuotaUsage, error) {
	var u QuotaUsage
	err := db.WriteTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM games WHERE game_id=?`, gameID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return httpx.NotFound("game not found")
		}
		var err error
		if u.Players, u.SaveBytes, err = countUsage(ctx, tx, gameID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO game_usage(game_id,players,save_bytes) VALUES(?,?,?)
			ON CONFLICT(game_id) DO UPDATE SET players=excluded.players, save_bytes=excluded.save_bytes`, gameID, u.Players, u.SaveBytes)
		return err
	})
	return u, err
}

func quotaExceeded(quota string, limit, used, requested int64) error {
	e := httpx.NewError(http.StatusForbidden, "quota_exceeded", "game quota exceeded: "+quota)
	e.Details = map[string]any{"quota": quota, "limit": limit, "used": used, "requested": requested}
	return e
}

// addUsage adds delta to game_usage.<column> inside tx. A positive delta under
// a limit (> 0) applies only if the result stays within it; otherwise 403
// quota_exceeded. Shrinking always applies. A missing row (game created
// outside CreateGame) is created from a recount that already includes this
// transaction's change. column is a fixed identifier, never user input.
func addUsage(ctx context.Context, tx *sql.Tx, gameID, column, quota string, delta, limit int64) error {
	if delta == 0 {
		return nil
	}
	for range 2 {
		q, args := `UPDATE game_usage SET `+column+`=`+column+`+? WHERE game_id=?`, []any{delta, gameID}
		if delta > 0 && limit > 0 {
			q, args = q+` AND `+column+`+? <= ?`, append(args, delta, limit)
		}
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return nil
		}
		var used int64
		err = tx.QueryRowContext(ctx, `SELECT `+column+` FROM game_usage WHERE game_id=?`, gameID).Scan(&used)
		if err == nil {
			return quotaExceeded(quota, limit, used, delta)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		players, saveBytes, err := countUsage(ctx, tx, gameID)
		if err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, `INSERT INTO game_usage(game_id,players,save_bytes) VALUES(?,?,?) ON CONFLICT(game_id) DO NOTHING`,
			gameID, players, saveBytes)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			used = players
			if column == "save_bytes" {
				used = saveBytes
			}
			if delta > 0 && limit > 0 && used > limit {
				return quotaExceeded(quota, limit, used-delta, delta)
			}
			return nil
		}
		// A concurrent transaction created the row (without this change): update it.
	}
	return errors.New("game_usage row for " + gameID + " missing after insert")
}

// gameQuota is one game's cached limits and per-instance quota state.
type gameQuota struct {
	id string

	mu       sync.Mutex
	loaded   bool
	loading  bool
	loadedAt time.Time
	limits   Quotas
	rpm      ratelimit.Allower

	players, saveBytes atomic.Int64 // stored usage as of the last refresh (gauges)
	writes             atomic.Int64 // mutating requests in flight on this instance
	gaugeLimits        atomic.Pointer[Quotas]
	gaugeEvents        atomic.Int64 // mirror of evCount for the gauge

	evMu     sync.Mutex
	evDay    int64 // UTC day number of evCount
	evSeeded bool
	evCount  int64 // events admitted today (seeded from the analytics store)
}

type quotaCache struct {
	mu    sync.Mutex
	games map[string]*gameQuota // bounded by the admin-created games
}

func newQuotaCache() *quotaCache { return &quotaCache{games: map[string]*gameQuota{}} }

func (k *Kit) quotaEntry(id string) *gameQuota {
	c := k.quotas
	c.mu.Lock()
	defer c.mu.Unlock()
	gq := c.games[id]
	if gq == nil {
		gq = &gameQuota{id: id}
		c.games[id] = gq
		k.registerQuotaGauges(gq)
	}
	return gq
}

// registerQuotaGauges exports limit and usage gauges labelled with the game id.
// Cardinality: games are created by admins (bounded set), 5 limit + 4 usage
// series per game, registered only for games this instance has served.
// Gauge functions run under the registry lock, so they read atomics only, and
// no code calls the registry while holding gq.mu or gq.evMu.
func (k *Kit) registerQuotaGauges(gq *gameQuota) {
	if k.Metrics == nil {
		return
	}
	lim := func(f func(Quotas) int64) func() float64 {
		return func() float64 {
			if l := gq.gaugeLimits.Load(); l != nil {
				return float64(f(*l))
			}
			return 0
		}
	}
	l := func(q string) string { return `{game="` + gq.id + `",quota="` + q + `"}` }
	k.Metrics.Gauge("game_quota_limit"+l("max_players"), lim(func(q Quotas) int64 { return q.MaxPlayers }))
	k.Metrics.Gauge("game_quota_limit"+l("requests_per_minute"), lim(func(q Quotas) int64 { return q.RequestsPerMinute }))
	k.Metrics.Gauge("game_quota_limit"+l("save_bytes_total"), lim(func(q Quotas) int64 { return q.SaveBytesTotal }))
	k.Metrics.Gauge("game_quota_limit"+l("analytics_events_per_day"), lim(func(q Quotas) int64 { return q.AnalyticsEventsPerDay }))
	k.Metrics.Gauge("game_quota_limit"+l("write_concurrency"), lim(func(q Quotas) int64 { return q.WriteConcurrency }))
	k.Metrics.Gauge("game_quota_usage"+l("max_players"), func() float64 { return float64(gq.players.Load()) })
	k.Metrics.Gauge("game_quota_usage"+l("save_bytes_total"), func() float64 { return float64(gq.saveBytes.Load()) })
	k.Metrics.Gauge("game_quota_usage"+l("write_concurrency"), func() float64 { return float64(gq.writes.Load()) })
	k.Metrics.Gauge("game_quota_usage"+l("analytics_events_per_day"), func() float64 { return float64(gq.gaugeEvents.Load()) })
}

func (k *Kit) countQuotaRejection(gameID, quota string) {
	if k.Metrics != nil {
		k.Metrics.Counter("game_quota_rejected_total", `game="`+gameID+`",quota="`+quota+`"`).Add(1)
	}
}

// gameQuota returns the game's entry with limits no older than quotaTTL. While
// one request refreshes, others use the previous limits; a failed refresh keeps
// the previous limits (logged) and only fails when nothing was loaded yet.
func (k *Kit) gameQuota(ctx context.Context, id string) (*gameQuota, error) {
	gq := k.quotaEntry(id)
	gq.mu.Lock()
	if gq.loaded && (gq.loading || time.Since(gq.loadedAt) < quotaTTL) {
		gq.mu.Unlock()
		return gq, nil
	}
	gq.loading = true
	gq.mu.Unlock()
	st, err := LoadQuotaStatus(ctx, k.DB.Read, k.Cfg.QuotaDefaults, id)
	gq.mu.Lock()
	gq.loading = false
	if err != nil {
		loaded := gq.loaded
		gq.mu.Unlock()
		if loaded {
			k.Log.WarnContext(ctx, "quota refresh failed; using previous limits", "game", id, "err", err)
			return gq, nil
		}
		return nil, err
	}
	rpm := gq.rpm
	gq.mu.Unlock()
	if n := st.Limits.RequestsPerMinute; n > 0 {
		if rpm == nil {
			// Built outside gq.mu: the factory registers metrics. Backend
			// failure degrades to a per-instance bucket of the same size.
			rpm = k.NewLimiter("game_quota_rpm_"+id, int(n), ratelimit.FailLocal)
		} else {
			rpm.SetPerMinute(int(n))
		}
	}
	gq.mu.Lock()
	if gq.rpm == nil {
		gq.rpm = rpm
	}
	gq.limits, gq.loaded, gq.loadedAt = st.Limits, true, time.Now()
	gq.mu.Unlock()
	limits := st.Limits
	gq.gaugeLimits.Store(&limits)
	gq.players.Store(st.Usage.Players)
	gq.saveBytes.Store(st.Usage.SaveBytes)
	return gq, nil
}

func (gq *gameQuota) current() (Quotas, ratelimit.Allower) {
	gq.mu.Lock()
	defer gq.mu.Unlock()
	return gq.limits, gq.rpm
}

// invalidateQuota makes the next request reload the game's limits.
func (k *Kit) invalidateQuota(id string) {
	gq := k.quotaEntry(id)
	gq.mu.Lock()
	gq.loadedAt = time.Time{}
	gq.mu.Unlock()
}

// quotaLimits returns the limits to enforce inside a write transaction: the
// cached ones (withGame refreshed them for this request) or, when this
// instance has none, the stored ones read through q.
func (k *Kit) quotaLimits(ctx context.Context, q queryer, id string) (Quotas, error) {
	k.quotas.mu.Lock()
	gq := k.quotas.games[id]
	k.quotas.mu.Unlock()
	if gq != nil {
		gq.mu.Lock()
		l, ok := gq.limits, gq.loaded
		gq.mu.Unlock()
		if ok {
			return l, nil
		}
	}
	st, err := LoadQuotaStatus(ctx, q, k.Cfg.QuotaDefaults, id)
	return st.Limits, err
}

// enforceRequestQuotas applies requests_per_minute to every request of the
// game and write_concurrency to its mutating requests. release must be called
// when the request ends.
func (k *Kit) enforceRequestQuotas(r *http.Request, gameID string) (release func(), err error) {
	gq, err := k.gameQuota(r.Context(), gameID)
	if err != nil {
		return nil, err
	}
	limits, rpm := gq.current()
	if limits.RequestsPerMinute > 0 && rpm != nil {
		if ok, retry := rpm.Allow("requests"); !ok {
			k.countQuotaRejection(gameID, "requests_per_minute")
			e := httpx.RateLimited(retry)
			e.Details = map[string]any{"quota": "requests_per_minute", "limit": limits.RequestsPerMinute}
			return nil, e
		}
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return func() {}, nil
	}
	if n := gq.writes.Add(1); limits.WriteConcurrency > 0 && n > limits.WriteConcurrency {
		gq.writes.Add(-1)
		k.countQuotaRejection(gameID, "write_concurrency")
		e := httpx.Overloaded("game write concurrency quota reached")
		e.Details = map[string]any{"quota": "write_concurrency", "limit": limits.WriteConcurrency}
		return nil, e
	}
	return func() { gq.writes.Add(-1) }, nil
}

// admitEvents returns how many of n analytics events the game's daily quota
// still admits on this instance. The count is per instance and per UTC day,
// seeded from the analytics store on the first event of the day, so with N
// instances at most N × quota events are accepted per day.
func (k *Kit) admitEvents(ctx context.Context, gameID string, n int) int {
	gq := k.quotaEntry(gameID)
	limits, _ := gq.current()
	if limits.AnalyticsEventsPerDay <= 0 {
		return n
	}
	now := k.Now().UTC()
	day := now.Unix() / 86400
	gq.evMu.Lock()
	if gq.evDay != day {
		gq.evDay, gq.evCount, gq.evSeeded = day, 0, false
	}
	if !gq.evSeeded {
		gq.evSeeded = true
		var stored int64
		err := k.Analytics.DB.Read.QueryRowContext(ctx, `SELECT count(*) FROM analytics_events WHERE game_id=? AND server_time>=?`,
			gameID, day*86400*1000).Scan(&stored)
		if err != nil {
			k.Log.WarnContext(ctx, "analytics quota seed failed; counting from zero", "game", gameID, "err", err)
		}
		gq.evCount += stored
	}
	allowed := int64(n)
	if left := limits.AnalyticsEventsPerDay - gq.evCount; left < allowed {
		allowed = max(left, 0)
	}
	gq.evCount += allowed
	gq.gaugeEvents.Store(gq.evCount)
	gq.evMu.Unlock()
	if dropped := int64(n) - allowed; dropped > 0 && k.Metrics != nil {
		k.Metrics.Counter("game_quota_events_dropped_total", `game="`+gameID+`"`).Add(dropped)
	}
	return int(allowed)
}

func (k *Kit) instanceUsage(st *QuotaStatus) {
	gq := k.quotaEntry(st.GameID)
	w := gq.writes.Load()
	gq.evMu.Lock()
	ev := int64(0)
	if gq.evDay == k.Now().UTC().Unix()/86400 {
		ev = gq.evCount
	}
	gq.evMu.Unlock()
	st.Usage.WritesInFlight, st.Usage.AnalyticsEventsToday = &w, &ev
}

func (k *Kit) adminGetQuotas(w http.ResponseWriter, r *http.Request) error {
	g := gameFrom(r.Context())
	st, err := LoadQuotaStatus(r.Context(), k.DB.Read, k.Cfg.QuotaDefaults, g.ID)
	if err != nil {
		return err
	}
	k.instanceUsage(&st)
	httpx.WriteJSON(w, http.StatusOK, st)
	return nil
}

func (k *Kit) adminPutQuotas(w http.ResponseWriter, r *http.Request) error {
	g := gameFrom(r.Context())
	body, err := readBody(r, 4096)
	if err != nil {
		return err
	}
	var o QuotaOverrides
	if err := decodeStrict(body, &o); err != nil {
		return err
	}
	key := AdminKey(r.Context())
	if err := SetQuotaOverrides(r.Context(), k.DB, g.ID, o, key.ID); err != nil {
		return mapWriteErr(err)
	}
	k.invalidateQuota(g.ID)
	k.Log.InfoContext(r.Context(), "game quotas updated", "game", g.ID, "by", key.ID)
	st, err := LoadQuotaStatus(r.Context(), k.DB.Read, k.Cfg.QuotaDefaults, g.ID)
	if err != nil {
		return err
	}
	k.instanceUsage(&st)
	httpx.WriteJSON(w, http.StatusOK, st)
	return nil
}
