// Package app wires configuration, storage, kits and the HTTP server into a
// runnable GoStack application, and provides the application subcommands.
package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pillar/gostack/adapters/sqlite"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/core/observability"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/migrations"
	"github.com/pillar/gostack/modules/idempotency"
	"github.com/pillar/gostack/modules/jobs"
)

// Version is set at build time: -ldflags "-X github.com/pillar/gostack/app.Version=v0.1.0".
var Version = "dev"

// Hooks lets a generated application add routes and job handlers.
type Hooks struct {
	Routes func(mux *http.ServeMux, a *App)
	Jobs   map[string]jobs.Handler
}

// App is a running (or ready to run) application instance.
type App struct {
	Cfg       config.Config
	Log       *slog.Logger
	DB        *sqlite.DB
	Analytics *sqlite.DB
	Jobs      *jobs.Queue
	Metrics   *observability.Registry
	Game      *game.Kit

	ready atomic.Bool
}

func storageOptions(d config.DatabaseConfig, path string, lock bool) sqlite.Options {
	return sqlite.Options{
		Path: path, ReadConnections: d.ReadConnections, Synchronous: d.Synchronous, BusyTimeout: d.BusyTimeout,
		CacheSizeKiB: d.CacheSizeKiB, MmapSize: d.MmapSize, WALAutocheckpoint: d.WALAutocheckpoint,
		WriteQueue: d.WriteQueue, SkipLock: !lock,
	}
}

// OpenDB opens and migrates the main database following the PRD init order:
// instance lock → write connection → migrate → read pool.
func OpenDB(ctx context.Context, cfg config.Config, lock bool) (*sqlite.DB, error) {
	o := storageOptions(cfg.Database, cfg.Database.Path, lock)
	db, err := sqlite.Open(o)
	if err != nil {
		return nil, err
	}
	ms, err := sqlite.LoadMigrations(migrations.SQLite, "sqlite")
	if err == nil {
		_, err = db.Migrate(ctx, ms)
	}
	if err == nil {
		err = db.OpenReaders(o)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func openAnalytics(ctx context.Context, cfg config.Config) (*sqlite.DB, error) {
	d := cfg.Database
	d.Synchronous = "NORMAL" // P2 data: loss of the last transactions on power failure is acceptable
	d.ReadConnections = 1
	o := storageOptions(d, cfg.Database.AnalyticsPath, true)
	db, err := sqlite.Open(o)
	if err != nil {
		return nil, err
	}
	ms, err := sqlite.LoadMigrations(migrations.Analytics, "analytics")
	if err == nil {
		_, err = db.Migrate(ctx, ms)
	}
	if err == nil {
		err = db.OpenReaders(o)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// New opens storage and builds the application. Caller must Close it.
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	a := &App{Cfg: cfg, Log: log, Metrics: observability.NewRegistry()}
	var err error
	if a.DB, err = OpenDB(ctx, cfg, true); err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	a.Jobs = &jobs.Queue{DB: a.DB}
	if slices.Contains(cfg.Kits, "game") {
		if a.Analytics, err = openAnalytics(ctx, cfg); err != nil {
			a.Close()
			return nil, fmt.Errorf("open analytics database: %w", err)
		}
		ip, err := httpx.NewClientIP(cfg.HTTP.TrustedProxies)
		if err != nil {
			a.Close()
			return nil, err
		}
		platforms := map[string]game.PlatformVerifier{}
		if p, ok := cfg.Game.Platforms["wechat"]; ok {
			platforms["wechat"] = &game.WeChatVerifier{Apps: p.Apps, Endpoint: p.Endpoint}
		}
		a.Game = &game.Kit{DB: a.DB, Analytics: game.NewAnalytics(a.Analytics, cfg.Game.AnalyticsBuffer, log),
			Cfg: cfg.Game, ClientIP: ip, Platforms: platforms, Metrics: a.Metrics, Log: log}
		a.Game.Init()
	}
	a.registerMetrics()
	return a, nil
}

func (a *App) registerMetrics() {
	a.Metrics.RegisterRuntime()
	a.Metrics.Gauge("db_write_waiting", func() float64 { return float64(a.DB.Stats().WriteWaiting) })
	a.Metrics.Gauge("db_write_rejected_total", func() float64 { return float64(a.DB.Stats().WriteRejected) })
	a.Metrics.Gauge("db_busy_retries_total", func() float64 { return float64(a.DB.Stats().Busy) })
	a.Metrics.Gauge("db_wal_bytes", func() float64 { return float64(a.DB.Stats().WALBytes) })
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
	if h.Routes != nil {
		h.Routes(mux, a)
	}
	mux.Handle("/", httpx.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		return httpx.NotFound("route not found")
	}))
	c := a.Cfg.HTTP
	return httpx.Chain(observability.CaptureRoute(mux),
		httpx.WithRequestID(),
		observability.AccessLog(a.Metrics, a.Log),
		httpx.WithRecover(),
		httpx.WithLimits(c.MaxBodyBytes, c.RequestTimeout, c.MaxConcurrent),
	)
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
func (a *App) Serve(ctx context.Context, h Hooks) error {
	c := a.Cfg.HTTP
	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           a.Handler(h),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       c.RequestTimeout + 5*time.Second,
		WriteTimeout:      c.RequestTimeout + 5*time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    c.MaxHeaderBytes,
	}
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
	for k, v := range h.Jobs {
		handlers[k] = v
	}
	worker := &jobs.Worker{Queue: a.Jobs, Handlers: handlers, Workers: a.Cfg.Queue.Workers, Poll: a.Cfg.Queue.PollInterval, Lease: a.Cfg.Queue.Lease, Log: a.Log}
	wg.Add(2)
	go func() { defer wg.Done(); worker.Run(bg) }()
	go func() { defer wg.Done(); a.maintenance(bg) }()
	if a.Game != nil {
		go a.Game.Analytics.Run(bg, 5*time.Second)
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
		if a.Game != nil {
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
