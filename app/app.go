// Package app wires configuration, storage, kits and the HTTP server into a
// runnable GoStack application, and provides the application subcommands.
package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pillar/gostack/adapters/postgres"
	"github.com/pillar/gostack/adapters/sqlite"
	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/adapters/valkey"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/core/observability"
	"github.com/pillar/gostack/kits/agent"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/kits/web"
	"github.com/pillar/gostack/migrations"
	"github.com/pillar/gostack/modules/cache"
	"github.com/pillar/gostack/modules/idempotency"
	"github.com/pillar/gostack/modules/jobs"
	"github.com/pillar/gostack/modules/ratelimit"
	"github.com/pillar/gostack/modules/webauth"
)

// Version is set at build time: -ldflags "-X github.com/pillar/gostack/app.Version=v0.1.0".
var Version = "dev"

// Hooks lets a generated application add routes and job handlers.
type Hooks struct {
	Routes func(mux *http.ServeMux, a *App)
	Jobs   map[string]jobs.Handler
	// Workflows are registered with AgentKit (kits: [agent]) before serving.
	Workflows map[string]agent.Workflow
}

// App is a running (or ready to run) application instance.
type App struct {
	// Cfg is the configuration the process started with. Restart-only keys
	// never change at runtime; read hot keys through Config().
	Cfg       config.Config
	Log       *slog.Logger
	DB        *store.DB
	Analytics *store.DB
	Jobs      *jobs.Queue
	Metrics   *observability.Registry
	Game      *game.Kit
	// Agent and Web are set when kits include agent / web.
	Agent *agent.Kit
	Web   *web.Kit
	// WebAuth is set when auth.web.enabled; protect app routes with WebAuth.Require.
	WebAuth *webauth.Module
	// Mod holds the optional modules enabled by configuration.
	Mod Modules
	// Cache is the configured cache (cache.driver memory or valkey). It must
	// only hold recomputable, briefly-stale-tolerant data, never business facts;
	// treat Get errors as misses (cache.GetOrMiss). Nothing built in uses it.
	Cache cache.Cache
	// Limits builds rate limiters per rate_limit.driver (local or global);
	// application routes should use it for their own budgets.
	Limits ratelimit.Factory
	// Valkey is the shared connection when a valkey driver is configured (else nil).
	Valkey *valkey.Client
	// LogLevel, when set (serve does), is retuned by log.level on reload.
	LogLevel *slog.LevelVar

	ready       atomic.Bool
	cfg         *config.Snapshot
	inflight    atomic.Int64
	inflightMax atomic.Int64
	reloadMu    sync.Mutex
	subs        []func(old, new config.Config)
	restartKeys atomic.Int64
}

func storageOptions(d config.DatabaseConfig, path string, lock bool) sqlite.Options {
	return sqlite.Options{
		Path: path, ReadConnections: d.ReadConnections, Synchronous: d.Synchronous, BusyTimeout: d.BusyTimeout,
		CacheSizeKiB: d.CacheSizeKiB, MmapSize: d.MmapSize, WALAutocheckpoint: d.WALAutocheckpoint,
		WriteQueue: d.WriteQueue, SkipLock: !lock,
	}
}

// OpenDB opens and migrates the main database following the PRD init order:
// instance lock → write connection → migrate → read pool (SQLite), or
// connect → migrate under an advisory lock (PostgreSQL; lock is ignored).
func OpenDB(ctx context.Context, cfg config.Config, lock bool) (*store.DB, error) {
	if cfg.Database.Driver == store.Postgres {
		return openPostgres(ctx, cfg.Database, cfg.Database.DSNRef, "", true)
	}
	o := storageOptions(cfg.Database, cfg.Database.Path, lock)
	return openSQLite(ctx, o, migrations.SQLite, "sqlite")
}

func openSQLite(ctx context.Context, o sqlite.Options, fsys fs.FS, dir string) (*store.DB, error) {
	db, err := sqlite.Open(o)
	if err != nil {
		return nil, err
	}
	ms, err := store.LoadMigrations(fsys, dir)
	if err == nil {
		_, err = db.Migrate(ctx, ms)
	}
	if err == nil {
		err = sqlite.OpenReaders(db, o)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// AnalyticsSchema is the PostgreSQL schema holding the analytics tables.
const AnalyticsSchema = "gostack_analytics"

// postgresOptions resolves the DSN secret reference and builds adapter options.
func postgresOptions(d config.DatabaseConfig, dsnRef, schema string) (postgres.Options, error) {
	dsn, err := config.ResolveSecret(dsnRef)
	if err != nil {
		return postgres.Options{}, fmt.Errorf("database dsn: %w", err)
	}
	return postgres.Options{DSN: dsn, MaxOpenConns: d.MaxOpenConnections, ReadConns: d.ReadConnections,
		WriteQueue: d.WriteQueue, Schema: schema}, nil
}

// openPostgres connects and, when migrate is set, applies the PostgreSQL
// migrations (main, or analytics when schema is AnalyticsSchema).
func openPostgres(ctx context.Context, d config.DatabaseConfig, dsnRef, schema string, migrate bool) (*store.DB, error) {
	o, err := postgresOptions(d, dsnRef, schema)
	if err != nil {
		return nil, err
	}
	if schema == AnalyticsSchema {
		o.MaxOpenConns, o.ReadConns = 2, 1
	}
	db, err := postgres.Open(ctx, o)
	if err != nil {
		return nil, err
	}
	if migrate {
		fsys, dir := migrations.Main(store.Postgres)
		if schema == AnalyticsSchema {
			fsys, dir = migrations.AnalyticsFor(store.Postgres)
		}
		ms, err := store.LoadMigrations(fsys, dir)
		if err == nil {
			_, err = db.Migrate(ctx, ms)
		}
		if err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}

func openAnalytics(ctx context.Context, cfg config.Config) (*store.DB, error) {
	d := cfg.Database
	if d.Driver == store.Postgres {
		ref := d.AnalyticsDSNRef
		if ref == "" {
			ref = d.DSNRef
		}
		return openPostgres(ctx, d, ref, AnalyticsSchema, true)
	}
	d.Synchronous = "NORMAL" // P2 data: loss of the last transactions on power failure is acceptable
	d.ReadConnections = 1
	return openSQLite(ctx, storageOptions(d, cfg.Database.AnalyticsPath, true), migrations.Analytics, "analytics")
}

// New opens storage and builds the application. Caller must Close it.
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	a := &App{Cfg: cfg, Log: log, Metrics: observability.NewRegistry(), cfg: config.NewSnapshot(cfg)}
	a.inflightMax.Store(int64(cfg.HTTP.MaxConcurrent))
	ip, err := httpx.NewClientIP(cfg.HTTP.TrustedProxies)
	if err != nil {
		return nil, err
	}
	ip.TrustCFConnectingIP = cfg.HTTP.TrustCFConnectingIP
	if a.DB, err = OpenDB(ctx, cfg, true); err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	a.Jobs = &jobs.Queue{DB: a.DB}
	if err := a.initValkey(ctx, cfg); err != nil {
		a.Close()
		return nil, err
	}
	if cfg.Auth.Web.Enabled {
		a.WebAuth = &webauth.Module{DB: a.DB, Cfg: cfg.Auth.Web, Secure: cfg.Environment == "production", ClientIP: ip, Log: log,
			Limits: a.Limits}
		a.WebAuth.Init()
		a.OnReload(func(_, n config.Config) {
			a.WebAuth.SetRates(n.Auth.Web.IPRatePerMinute, n.Auth.Web.AccountRatePerMinute)
		})
	}
	if slices.Contains(cfg.Kits, "game") {
		if a.Analytics, err = openAnalytics(ctx, cfg); err != nil {
			a.Close()
			return nil, fmt.Errorf("open analytics database: %w", err)
		}
		platforms := map[string]game.PlatformVerifier{}
		if p, ok := cfg.Game.Platforms["wechat"]; ok {
			platforms["wechat"] = &game.WeChatVerifier{Apps: p.Apps, Endpoint: p.Endpoint}
		}
		a.Game = &game.Kit{DB: a.DB, Analytics: game.NewAnalytics(a.Analytics, cfg.Game.AnalyticsBuffer, log),
			Cfg: cfg.Game, ClientIP: ip, Platforms: platforms, Metrics: a.Metrics, Log: log, Limits: a.Limits}
		a.Game.Init()
		a.OnReload(func(_, n config.Config) { a.Game.SetRates(n.Game.AuthRatePerMinute, n.Game.ScoreRatePerMinute) })
	}
	if err := a.initModules(cfg); err != nil {
		a.Close()
		return nil, err
	}
	a.initKits(cfg)
	a.registerMetrics()
	return a, nil
}

func (a *App) registerMetrics() {
	a.Metrics.RegisterRuntime()
	a.Metrics.Gauge("db_write_waiting", func() float64 { return float64(a.DB.Stats().WriteWaiting) })
	a.Metrics.Gauge("db_write_rejected_total", func() float64 { return float64(a.DB.Stats().WriteRejected) })
	a.Metrics.Gauge("db_busy_retries_total", func() float64 { return float64(a.DB.Stats().Busy) })
	a.Metrics.Gauge("db_wal_bytes", func() float64 { return float64(a.DB.Stats().WALBytes) })
	dataDir := filepath.Dir(a.Cfg.Database.Path)
	a.Metrics.Gauge("disk_free_bytes", func() float64 {
		var st syscall.Statfs_t
		if err := syscall.Statfs(dataDir, &st); err != nil {
			return -1
		}
		return float64(int64(st.Bavail) * int64(st.Bsize))
	})
	a.Metrics.Gauge("http_in_flight", func() float64 { return float64(a.inflight.Load()) })
	a.Metrics.Gauge("http_max_concurrent", func() float64 { return float64(a.inflightMax.Load()) })
	a.Metrics.Gauge("config_restart_pending_keys", func() float64 { return float64(a.restartKeys.Load()) })
	a.Metrics.Counter("config_reloads_total", `result="applied"`)
	a.Metrics.Counter("config_reloads_total", `result="rejected"`)
	var mu sync.Mutex
	var last jobs.Stats
	var at time.Time
	jobStats := func() jobs.Stats {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(at) > 5*time.Second {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			if s, err := a.Jobs.Stats(ctx); err == nil {
				last, at = s, time.Now()
			}
			cancel()
		}
		return last
	}
	a.Metrics.Gauge("jobs_runnable", func() float64 { return float64(jobStats().Runnable) })
	a.Metrics.Gauge("jobs_dead", func() float64 { return float64(jobStats().Dead) })
	a.Metrics.Gauge("jobs_oldest_age_seconds", func() float64 { return jobStats().OldestAgeSec })
	if a.Game != nil {
		an := a.Game.Analytics
		a.Metrics.Gauge("analytics_accepted_total", func() float64 { n, _, _, _ := an.Stats(); return float64(n) })
		a.Metrics.Gauge("analytics_dropped_total", func() float64 { _, n, _, _ := an.Stats(); return float64(n) })
		a.Metrics.Gauge("analytics_buffer_depth", func() float64 { _, _, _, n := an.Stats(); return float64(n) })
	}
}

// Close releases storage.
func (a *App) Close() error {
	var errs []error
	if a.Analytics != nil {
		errs = append(errs, a.Analytics.Close())
	}
	if a.DB != nil {
		errs = append(errs, a.DB.Close())
	}
	if a.Valkey != nil {
		errs = append(errs, a.Valkey.Close())
	}
	return errors.Join(errs...)
}

// Handler builds the public HTTP handler.
func (a *App) Handler(h Hooks) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("GET /readyz", httpx.HandlerFunc(a.readyz))
	if a.Game != nil {
		a.Game.Routes(mux)
	}
	if a.WebAuth != nil {
		a.WebAuth.Routes(mux)
	}
	a.moduleRoutes(mux)
	if err := a.kitRoutes(mux, h); err != nil {
		a.Log.Error("kit routes", "err", err)
	}
	if h.Routes != nil {
		h.Routes(mux, a)
	}
	mux.Handle("/", httpx.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		return httpx.NotFound("route not found")
	}))
	c := a.Cfg.HTTP
	main := httpx.Chain(observability.CaptureRoute(mux),
		httpx.WithRequestID(),
		observability.AccessLog(a.Metrics, a.Log),
		httpx.WithRecover(),
		a.concurrencyGate,
		// The in-flight cap is enforced by concurrencyGate (hot-reloadable);
		// WithLimits gets the static validation ceiling.
		httpx.WithLimits(c.MaxBodyBytes, c.RequestTimeout, config.MaxConcurrentCeiling),
	)
	prefix, sh := a.storageRoute()
	if sh == nil {
		return main
	}
	// Object transfers are bounded by storage.max_object_bytes / storage.timeout
	// instead of http.max_body_bytes / http.request_timeout; they keep request
	// ids, access logs, panic recovery and the in-flight cap.
	smux := http.NewServeMux()
	smux.Handle(prefix, sh)
	storage := httpx.Chain(observability.CaptureRoute(smux),
		httpx.WithRequestID(),
		observability.AccessLog(a.Metrics, a.Log),
		httpx.WithRecover(),
		a.concurrencyGate,
		a.storageDeadlines,
	)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, prefix) {
			storage.ServeHTTP(w, r)
			return
		}
		main.ServeHTTP(w, r)
	})
}

// concurrencyGate rejects requests beyond http.max_concurrent with 503 instead
// of queueing. The limit is read per request so a reload applies immediately.
func (a *App) concurrencyGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := a.inflight.Add(1)
		defer a.inflight.Add(-1)
		if n > a.inflightMax.Load() {
			httpx.WriteError(w, r, httpx.Overloaded("server busy"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Config returns the current configuration snapshot (startup values with hot
// keys from the latest successful reload).
func (a *App) Config() config.Config { return a.cfg.Load() }

// OnReload registers f to run after a successful reload with the previous and
// the new snapshot. Only hot keys differ between them. Register before Serve;
// f runs on the reload goroutine and must not block.
func (a *App) OnReload(f func(old, new config.Config)) {
	a.reloadMu.Lock()
	a.subs = append(a.subs, f)
	a.reloadMu.Unlock()
}

// ReloadResult reports what a reload changed.
type ReloadResult struct {
	Applied []config.Change `json:"applied"`
	// RestartRequired lists changed keys that were NOT applied.
	RestartRequired []config.Change `json:"restart_required"`
}

// Reload validates next completely and, if valid, atomically swaps in a
// snapshot with next's hot keys; restart-only changes are reported, not
// applied. An invalid next leaves the running configuration untouched.
func (a *App) Reload(next config.Config) (ReloadResult, error) {
	var res ReloadResult
	if err := next.Validate(); err != nil {
		a.Metrics.Counter("config_reloads_total", `result="rejected"`).Add(1)
		return res, err
	}
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	old := a.cfg.Load()
	for _, c := range config.Diff(old, next) {
		if c.Hot {
			res.Applied = append(res.Applied, c)
		} else {
			res.RestartRequired = append(res.RestartRequired, c)
		}
	}
	cur := config.WithHot(old, next)
	a.cfg.Store(cur)
	a.inflightMax.Store(int64(cur.HTTP.MaxConcurrent))
	if a.LogLevel != nil {
		_ = a.LogLevel.UnmarshalText([]byte(cur.Log.Level))
	}
	a.restartKeys.Store(int64(len(res.RestartRequired)))
	for _, f := range a.subs {
		f(old, cur)
	}
	a.Metrics.Counter("config_reloads_total", `result="applied"`).Add(1)
	return res, nil
}

// reloadFrom loads configuration with load and applies it, logging the outcome.
func (a *App) reloadFrom(load func() (config.Config, error)) {
	next, err := load()
	if err == nil {
		var res ReloadResult
		if res, err = a.Reload(next); err == nil {
			a.Log.Info("config reloaded", "applied", changeKeys(res.Applied))
			for _, c := range res.RestartRequired {
				a.Log.Warn("config change requires restart; not applied", "key", c.Key, "running", c.Old, "configured", c.New)
			}
			return
		}
	} else {
		a.Metrics.Counter("config_reloads_total", `result="rejected"`).Add(1)
	}
	a.Log.Error("config reload rejected; keeping current configuration", "err", err)
}

func changeKeys(cs []config.Change) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Key)
	}
	return out
}

// NewLogger builds the process logger with a level that can change at runtime.
func NewLogger(w io.Writer, level, format string) (*slog.Logger, *slog.LevelVar) {
	lv := new(slog.LevelVar)
	_ = lv.UnmarshalText([]byte(level))
	base := observability.NewLogger(w, "debug", format) // keeps redaction; filtering happens below
	return slog.New(levelFilter{base.Handler(), lv}), lv
}

type levelFilter struct {
	slog.Handler
	lv *slog.LevelVar
}

func (h levelFilter) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.lv.Level() && h.Handler.Enabled(ctx, l)
}
func (h levelFilter) WithAttrs(as []slog.Attr) slog.Handler {
	return levelFilter{h.Handler.WithAttrs(as), h.lv}
}
func (h levelFilter) WithGroup(n string) slog.Handler {
	return levelFilter{h.Handler.WithGroup(n), h.lv}
}

func (a *App) readyz(w http.ResponseWriter, r *http.Request) error {
	switch {
	case !a.ready.Load():
		return httpx.Overloaded("not ready")
	case a.DB.Overloaded():
		return httpx.Overloaded("write queue saturated")
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	var one int
	if err := a.DB.Read.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return httpx.Overloaded("database unavailable")
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready", "version": Version})
	return nil
}

// Serve runs the HTTP server, workers and maintenance until ctx is cancelled,
// then shuts down: stop accepting → drain in-flight requests → stop workers →
// flush analytics → close databases (Close is the caller's job).
// httpServer is the public listener's server. Its read/write timeouts bound
// ordinary requests; the local storage route extends them per request.
func (a *App) httpServer(h Hooks) *http.Server {
	c := a.Cfg.HTTP
	return &http.Server{
		Handler:           a.Handler(h),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       c.RequestTimeout + 5*time.Second,
		WriteTimeout:      c.RequestTimeout + 5*time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    c.MaxHeaderBytes,
	}
}

func (a *App) Serve(ctx context.Context, h Hooks) error {
	c := a.Cfg.HTTP
	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	srv := a.httpServer(h)
	var internal *http.Server
	if c.InternalListen != "" {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", a.Metrics.Handler())
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		internal = &http.Server{Addr: c.InternalListen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := internal.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				a.Log.Error("internal listener failed", "err", err)
			}
		}()
	}

	bg, stopBG := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	handlers := map[string]jobs.Handler{}
	if a.Game != nil {
		for k, v := range a.Game.JobHandlers() {
			handlers[k] = v
		}
	}
	for k, v := range a.moduleJobHandlers() {
		handlers[k] = v
	}
	for k, v := range a.kitJobHandlers() {
		handlers[k] = v
	}
	for k, v := range h.Jobs {
		handlers[k] = v
	}
	worker := &jobs.Worker{Queue: a.Jobs, Handlers: handlers, Workers: a.Cfg.Queue.Workers, Poll: a.Cfg.Queue.PollInterval, Lease: a.Cfg.Queue.Lease, Log: a.Log}
	wg.Add(2)
	go func() { defer wg.Done(); worker.Run(bg) }()
	go func() { defer wg.Done(); a.maintenance(bg) }()
	if a.Game != nil {
		go a.Game.Analytics.Run(bg, 5*time.Second)
		if iv := a.Cfg.Game.Settlement.Interval; iv > 0 {
			wg.Add(1)
			go func() { defer wg.Done(); a.scheduleSettlements(bg, iv) }()
		}
	}
	if a.Mod.Match != nil {
		wg.Add(1)
		go func() { defer wg.Done(); a.Mod.Match.Run(bg) }()
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	a.ready.Store(true)
	a.Log.Info("serving", "listen", ln.Addr().String(), "version", Version, "profile", a.Cfg.Profile)

	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	a.ready.Store(false)
	sctx, cancel := context.WithTimeout(context.Background(), c.ShutdownTimeout)
	defer cancel()
	if serr := srv.Shutdown(sctx); serr != nil {
		// Deadline passed: drop remaining connections. Handlers still running may
		// fail against closed pools; their transactions roll back (no partial commits).
		a.Log.Error("http shutdown incomplete; forcing close", "err", serr)
		_ = srv.Close()
	}
	if internal != nil {
		_ = internal.Shutdown(sctx)
	}
	// Upgraded WebSocket connections are not tracked by http.Server: close them
	// (1001 going away) before background work and databases stop.
	if a.Mod.Realtime != nil {
		if herr := a.Mod.Realtime.Hub().Close(sctx); herr != nil {
			a.Log.Warn("realtime shutdown incomplete", "err", herr)
		}
	}
	stopBG()
	wg.Wait()
	if a.Game != nil {
		<-a.Game.Analytics.Done()
	}
	a.Log.Info("stopped")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// scheduleSettlements enqueues settlement jobs for ended seasons; the jobs
// carry dedupe keys, so repeated passes and multiple processes are harmless.
func (a *App) scheduleSettlements(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		if n, err := a.Game.ScheduleSettlements(c, a.Jobs); err != nil && ctx.Err() == nil {
			a.Log.Warn("settlement scheduling failed", "err", err)
		} else if n > 0 {
			a.Log.Info("settlement jobs enqueued", "count", n)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// maintenance runs idempotent cleanup periodically.
func (a *App) maintenance(ctx context.Context) {
	run := func() {
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		now := time.Now().UnixMilli()
		if err := a.DB.WriteTx(c, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(c, `DELETE FROM sessions WHERE expires_at<=?`, now)
			return err
		}); err != nil {
			a.Log.Warn("session cleanup failed", "err", err)
		}
		if _, err := idempotency.PurgeExpired(c, a.DB); err != nil {
			a.Log.Warn("idempotency cleanup failed", "err", err)
		}
		if _, err := a.Jobs.PurgeSucceeded(c, 7*24*time.Hour); err != nil {
			a.Log.Warn("job cleanup failed", "err", err)
		}
		if a.WebAuth != nil {
			if err := a.WebAuth.Maintain(c); err != nil {
				a.Log.Warn("web session cleanup failed", "err", err)
			}
		}
		for name, m := range a.maintainers() {
			if err := m.Maintain(c); err != nil {
				a.Log.Warn("module maintenance failed", "module", name, "err", err)
			}
		}
		if a.Game != nil {
			if _, err := a.Game.PurgeRuns(c); err != nil {
				a.Log.Warn("score run cleanup failed", "err", err)
			}
			if _, err := a.Game.Analytics.PurgeOlderThan(c, time.Duration(a.Cfg.Game.AnalyticsRetainDays)*24*time.Hour); err != nil {
				a.Log.Warn("analytics retention failed", "err", err)
			}
		}
	}
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// PrintJSON writes v as indented JSON (CLI --json output).
func PrintJSON(w interface{ Write([]byte) (int, error) }, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	w.Write(append(b, '\n'))
}
