package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pillar/gostack/adapters/sqlite"
	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/migrations"
	"github.com/pillar/gostack/modules/jobs"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitError   = 1
	ExitUsage   = 2
	ExitFailing = 3 // a check ran and reported problems
)

// UsageError marks command-line misuse.
type UsageError struct{ Msg string }

func (e UsageError) Error() string { return e.Msg }

// CheckFailed marks a check (doctor, validate, reconcile) that ran and failed.
type CheckFailed struct{ Msg string }

func (e CheckFailed) Error() string { return e.Msg }

const appUsage = `Application commands:
  serve                                  run the HTTP server (SIGHUP reloads hot config keys)
  migrate status|up                      show or apply schema migrations
  migrate to-postgres --from APP.DB --to-dsn-ref env:NAME [--analytics-from ANALYTICS.DB]
                                         copy a stopped SQLite deployment into an empty PostgreSQL database (verified)
  config validate                        validate configuration and list hot-reloadable keys
  config diff <other.yaml>               compare running sources with another file (hot vs restart)
  backup snapshot --to FILE              consistent online copy (SQLite: VACUUM INTO; PostgreSQL: pg_dump -Fc + FILE.json manifest)
  backup verify [--dsn-ref REF] FILE     integrity, schema compatibility and ledger reconcile (PostgreSQL: restore into a temporary database)
  backup restore --from FILE --to PATH   verify, then restore to a new path (never overwrites)
  backup restore --from FILE --to-dsn-ref env:NAME [--analytics-to-dsn-ref REF]
                                         restore a PostgreSQL backup into an empty database, then verify it
  doctor                                check environment, storage and configuration
  version                                print version
  game create <game_id> [--name N] [--origin URL]...
  game config publish <game_id> <file.json>
  game config rollback <game_id> <revision>
  game reconcile [game_id]               compare wallet balances with the ledger
  apikey create --name N [--scope admin] [--tenant T] [--ttl 720h]
  apikey revoke <key_id>
  jobs replay <job_id>                   move a dead job back to pending

Global flags (before the command): --config FILE, --json, --set key=value (repeatable)
`

// Env carries the process streams; tests substitute buffers.
type Env struct {
	Stdout, Stderr io.Writer
	Getenv         func(string) (string, bool)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// Main runs application subcommands and returns the process exit code.
func Main(args []string, hooks Hooks, env Env) int {
	if env.Stdout == nil {
		env.Stdout, env.Stderr = os.Stdout, os.Stderr
	}
	err := run(args, hooks, env)
	switch e := err.(type) {
	case nil:
		return ExitOK
	case UsageError:
		fmt.Fprintf(env.Stderr, "error: %s\n\n%s", e.Msg, appUsage)
		return ExitUsage
	case CheckFailed:
		fmt.Fprintf(env.Stderr, "check failed: %s\n", e.Msg)
		return ExitFailing
	default:
		fmt.Fprintf(env.Stderr, "error: %v\n", err)
		return ExitError
	}
}

type globals struct {
	configPath string
	json       bool
	sets       multiFlag
}

func parseGlobals(args []string) (globals, []string, error) {
	var g globals
	fs := flag.NewFlagSet("gostack", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&g.configPath, "config", "", "config file")
	fs.BoolVar(&g.json, "json", false, "JSON output")
	fs.Var(&g.sets, "set", "override key=value")
	if err := fs.Parse(args); err != nil {
		return g, nil, UsageError{err.Error()}
	}
	return g, fs.Args(), nil
}

func (g globals) load(env Env) (config.Config, error) {
	ov := map[string]string{}
	for _, s := range g.sets {
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			return config.Config{}, UsageError{"--set expects key=value"}
		}
		ov[k] = v
	}
	return config.Load(g.configPath, env.Getenv, ov)
}

func run(args []string, hooks Hooks, env Env) error {
	g, rest, err := parseGlobals(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return UsageError{"command required"}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd, rest := rest[0], rest[1:]
	if cmd == "help" {
		fmt.Fprint(env.Stdout, appUsage)
		return nil
	}
	if cmd == "version" {
		if g.json {
			PrintJSON(env.Stdout, map[string]string{"version": Version, "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH})
		} else {
			fmt.Fprintf(env.Stdout, "gostack %s (%s %s/%s)\n", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		}
		return nil
	}
	cfg, cfgErr := g.load(env)
	switch cmd {
	case "config":
		if len(rest) == 2 && rest[0] == "diff" {
			return configDiff(g, cfg, cfgErr, rest[1], env)
		}
		if len(rest) != 1 || rest[0] != "validate" {
			return UsageError{"usage: config validate | config diff <other.yaml>"}
		}
		if g.json {
			out := map[string]any{"valid": cfgErr == nil, "hot_keys": config.HotKeys()}
			if cfgErr != nil {
				out["errors"] = strings.Split(cfgErr.Error(), "\n")
			} else {
				out["config"] = cfg
			}
			PrintJSON(env.Stdout, out)
		}
		if cfgErr != nil {
			return CheckFailed{cfgErr.Error()}
		}
		if !g.json {
			fmt.Fprintln(env.Stdout, "config valid")
			fmt.Fprintf(env.Stdout, "hot-reloadable on SIGHUP: %s\nall other keys require a restart\n", strings.Join(config.HotKeys(), ", "))
		}
		return nil
	case "backup":
		return backupCmd(ctx, cfg, cfgErr, rest, g.json, env)
	case "doctor":
		return doctor(ctx, cfg, cfgErr, g.json, env)
	}
	if cfgErr != nil {
		return fmt.Errorf("invalid configuration:\n%w", cfgErr)
	}
	switch cmd {
	case "serve":
		return serve(ctx, g, cfg, hooks, env)
	case "migrate":
		return migrate(ctx, cfg, rest, g.json, env)
	case "game":
		return gameCmd(ctx, cfg, rest, g.json, env)
	case "apikey":
		return apikeyCmd(ctx, cfg, rest, g.json, env)
	case "jobs":
		if len(rest) != 2 || rest[0] != "replay" {
			return UsageError{"usage: jobs replay <job_id>"}
		}
		db, err := openAdmin(ctx, cfg)
		if err != nil {
			return err
		}
		defer db.Close()
		return (&jobs.Queue{DB: db}).Replay(ctx, rest[1])
	}
	return UsageError{fmt.Sprintf("unknown command %q", cmd)}
}

func serve(ctx context.Context, g globals, cfg config.Config, hooks Hooks, env Env) error {
	// Registered before startup so an early SIGHUP does not kill the process.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	log, lv := NewLogger(env.Stderr, cfg.Log.Level, cfg.Log.Format)
	a, err := New(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer a.Close()
	a.LogLevel = lv
	// Reload re-reads the same sources: the --config file, the environment of
	// this process and the original --set flags.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				a.reloadFrom(func() (config.Config, error) { return g.load(env) })
			}
		}
	}()
	return a.Serve(ctx, hooks)
}

func configDiff(g globals, cur config.Config, curErr error, other string, env Env) error {
	if curErr != nil {
		return CheckFailed{"current configuration invalid: " + curErr.Error()}
	}
	g.configPath = other
	next, err := g.load(env)
	if err != nil {
		return CheckFailed{"configuration " + other + " invalid: " + err.Error()}
	}
	changes := config.Diff(cur, next)
	if g.json {
		PrintJSON(env.Stdout, map[string]any{"changes": changes, "hot_keys": config.HotKeys()})
		return nil
	}
	if len(changes) == 0 {
		fmt.Fprintln(env.Stdout, "no changes")
	}
	for _, c := range changes {
		mode := "restart"
		if c.Hot {
			mode = "hot"
		}
		fmt.Fprintf(env.Stdout, "%-8s %s: %s -> %s\n", mode, c.Key, c.Old, c.New)
	}
	return nil
}

// openAdmin opens the database for short administrative writes while a server
// may be running. It skips the instance lock (SQLite file locking still
// serialises writers) and refuses to run against a schema it does not match.
func openAdmin(ctx context.Context, cfg config.Config) (*store.DB, error) {
	var db *store.DB
	var o sqlite.Options
	var err error
	if cfg.Database.Driver == store.Postgres {
		if db, err = openPostgres(ctx, cfg.Database, cfg.Database.DSNRef, "", false); err != nil {
			return nil, err
		}
	} else {
		o = storageOptions(cfg.Database, cfg.Database.Path, false)
		if _, err := os.Stat(o.Path); err != nil {
			return nil, fmt.Errorf("database %s: %w (run `migrate up` or `serve` first)", o.Path, err)
		}
		if db, err = sqlite.Open(o); err != nil {
			return nil, err
		}
	}
	fsys, dir := migrations.Main(db.Dialect)
	ms, err := store.LoadMigrations(fsys, dir)
	if err != nil {
		db.Close()
		return nil, err
	}
	st, err := db.MigrationStatus(ctx, ms)
	if err == nil && store.Pending(st) {
		err = errors.New("database schema does not match this binary; run `migrate status`")
	}
	if err == nil && db.Dialect == store.SQLite {
		err = sqlite.OpenReaders(db, o)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func migrate(ctx context.Context, cfg config.Config, args []string, asJSON bool, env Env) error {
	if len(args) >= 1 && args[0] == "to-postgres" {
		return migrateToPostgres(ctx, args[1:], env)
	}
	if len(args) != 1 || (args[0] != "status" && args[0] != "up") {
		return UsageError{"usage: migrate status|up | migrate to-postgres --from APP.DB --to-dsn-ref env:NAME [--analytics-from ANALYTICS.DB]"}
	}
	fsys, dir := migrations.Main(cfg.Database.Driver)
	ms, err := store.LoadMigrations(fsys, dir)
	if err != nil {
		return err
	}
	var db *store.DB
	if cfg.Database.Driver == store.Postgres {
		db, err = openPostgres(ctx, cfg.Database, cfg.Database.DSNRef, "", false)
	} else {
		db, err = sqlite.Open(storageOptions(cfg.Database, cfg.Database.Path, args[0] == "up"))
	}
	if err != nil {
		if errors.Is(err, sqlite.ErrLocked) {
			return fmt.Errorf("%w; stop the running server before `migrate up`", err)
		}
		return err
	}
	defer db.Close()
	var applied []int
	if args[0] == "up" {
		if applied, err = db.Migrate(ctx, ms); err != nil {
			return err
		}
	}
	st, err := db.MigrationStatus(ctx, ms)
	if err != nil {
		return err
	}
	if asJSON {
		ss := summarize(st, ms)
		// Top-level "drift"/"unknown_versions" are what deploy/rollback.sh checks:
		// a database with versions this binary does not know must not be served by it.
		PrintJSON(env.Stdout, map[string]any{"applied_now": applied, "migrations": st, "pending": store.Pending(st),
			"drift": ss.Drift, "unknown_versions": ss.Unknown, "pending_versions": ss.Pending})
	} else {
		for _, s := range st {
			state := "pending"
			if s.Applied {
				state = "applied"
			}
			if s.Drift {
				state = "DRIFT"
			}
			fmt.Fprintf(env.Stdout, "%04d %-8s %s\n", s.Version, state, s.Name)
		}
	}
	if args[0] == "status" && store.Pending(st) {
		return CheckFailed{"migrations pending or drifted"}
	}
	return nil
}

func gameCmd(ctx context.Context, cfg config.Config, args []string, asJSON bool, env Env) error {
	if len(args) == 0 {
		return UsageError{"usage: game create|config|reconcile"}
	}
	db, err := openAdmin(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("game create", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		name := fs.String("name", "", "display name")
		tenant := fs.String("tenant", "default", "tenant id")
		var origins multiFlag
		fs.Var(&origins, "origin", "allowed CORS origin (repeatable)")
		if len(args) < 2 {
			return UsageError{"usage: game create <game_id> [--name N] [--origin URL]"}
		}
		if err := fs.Parse(args[2:]); err != nil {
			return UsageError{err.Error()}
		}
		if err := game.CreateGame(ctx, db, *tenant, args[1], *name, origins); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "game %s created\n", args[1])
		return nil
	case "config":
		if len(args) != 4 {
			return UsageError{"usage: game config publish <game_id> <file> | rollback <game_id> <revision>"}
		}
		var rev int64
		switch args[1] {
		case "publish":
			b, err := os.ReadFile(args[3])
			if err != nil {
				return err
			}
			rev, err = game.PublishConfig(ctx, db, args[2], b, "cli", filepath.Base(args[3]))
			if err != nil {
				return err
			}
		case "rollback":
			to, err := strconv.ParseInt(args[3], 10, 64)
			if err != nil {
				return UsageError{"revision must be an integer"}
			}
			if rev, err = game.RollbackConfig(ctx, db, args[2], to, "cli"); err != nil {
				return err
			}
		default:
			return UsageError{"usage: game config publish|rollback"}
		}
		fmt.Fprintf(env.Stdout, "game %s config revision %d published\n", args[2], rev)
		return nil
	case "reconcile":
		id := ""
		if len(args) > 1 {
			id = args[1]
		}
		ms, err := game.Reconcile(ctx, db.Read, id)
		if err != nil {
			return err
		}
		if asJSON {
			PrintJSON(env.Stdout, map[string]any{"mismatches": ms})
		} else {
			fmt.Fprintf(env.Stdout, "%d mismatches\n", len(ms))
			for _, m := range ms {
				fmt.Fprintf(env.Stdout, "%s %s %s balance=%d ledger=%d\n", m.GameID, m.PlayerID, m.Asset, m.Balance, m.Ledger)
			}
		}
		if len(ms) > 0 {
			return CheckFailed{fmt.Sprintf("%d wallet balances disagree with the ledger", len(ms))}
		}
		return nil
	}
	return UsageError{"unknown game subcommand " + strconv.Quote(args[0])}
}

func apikeyCmd(ctx context.Context, cfg config.Config, args []string, asJSON bool, env Env) error {
	if len(args) == 0 {
		return UsageError{"usage: apikey create|revoke"}
	}
	db, err := openAdmin(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	store := auth.APIKeys{DB: db}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("apikey create", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		name := fs.String("name", "", "key name")
		tenant := fs.String("tenant", "default", "tenant id")
		ttl := fs.Duration("ttl", 0, "expiry (0 = none)")
		var scopes multiFlag
		fs.Var(&scopes, "scope", "scope (repeatable)")
		if err := fs.Parse(args[1:]); err != nil {
			return UsageError{err.Error()}
		}
		if strings.TrimSpace(*tenant) == "" {
			// A key without a tenant would be scoped to nothing (billing
			// would read it as "all tenants"), so it is never issued.
			return UsageError{"apikey create: --tenant must not be empty"}
		}
		if len(scopes) == 0 {
			scopes = multiFlag{"admin"}
		}
		k, secret, err := store.Create(ctx, *tenant, *name, scopes, *ttl)
		if err != nil {
			return err
		}
		if asJSON {
			PrintJSON(env.Stdout, map[string]any{"key": k, "secret": secret})
		} else {
			fmt.Fprintf(env.Stdout, "id: %s\nsecret (shown once): %s\n", k.ID, secret)
		}
		return nil
	case "revoke":
		if len(args) != 2 {
			return UsageError{"usage: apikey revoke <key_id>"}
		}
		return store.Revoke(ctx, args[1])
	}
	return UsageError{"unknown apikey subcommand"}
}

type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func doctor(ctx context.Context, cfg config.Config, cfgErr error, asJSON bool, env Env) error {
	var checks []check
	add := func(name string, ok bool, detail string) { checks = append(checks, check{name, ok, detail}) }
	add("go_runtime", true, runtime.Version()+" "+runtime.GOOS+"/"+runtime.GOARCH)
	if cfgErr != nil {
		add("config", false, cfgErr.Error())
	} else {
		add("config", true, "profile="+cfg.Profile+" environment="+cfg.Environment)
		if cfg.Database.Driver == store.Postgres {
			doctorPostgres(ctx, cfg, add)
		} else {
			doctorSQLite(ctx, cfg, add)
		}
	}
	failed := 0
	for _, c := range checks {
		if !c.OK {
			failed++
		}
	}
	if asJSON {
		PrintJSON(env.Stdout, map[string]any{"ok": failed == 0, "checks": checks})
	} else {
		for _, c := range checks {
			mark := "ok  "
			if !c.OK {
				mark = "FAIL"
			}
			fmt.Fprintf(env.Stdout, "[%s] %-16s %s\n", mark, c.Name, c.Detail)
		}
	}
	if failed > 0 {
		return CheckFailed{fmt.Sprintf("%d doctor checks failed", failed)}
	}
	return nil
}

func errString(err error, ok string) string {
	if err != nil {
		return err.Error()
	}
	return ok
}

func doctorSQLite(ctx context.Context, cfg config.Config, add func(string, bool, string)) {
	dir := filepath.Dir(cfg.Database.Path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		add("data_dir", false, err.Error())
	} else if f, err := os.CreateTemp(dir, ".doctor-*"); err != nil {
		add("data_dir", false, "not writable: "+err.Error())
	} else {
		f.Close()
		os.Remove(f.Name())
		add("data_dir", true, dir)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err == nil {
		free := int64(st.Bavail) * int64(st.Bsize)
		add("disk_free", free > 1<<30, fmt.Sprintf("%d MiB free (warn below 1024 MiB)", free>>20))
	}
	o := storageOptions(cfg.Database, cfg.Database.Path, false)
	if _, err := os.Stat(o.Path); err != nil {
		add("database", true, "not created yet (created by migrate up / serve)")
	} else if db, err := sqlite.Open(o); err != nil {
		add("database", false, err.Error())
	} else {
		var ver string
		_ = db.Write.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&ver)
		var fts int
		_ = db.Write.QueryRowContext(ctx, "SELECT sqlite_compileoption_used('ENABLE_FTS5')").Scan(&fts)
		add("sqlite", true, fmt.Sprintf("version %s, fts5=%v", ver, fts == 1))
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := sqlite.IntegrityCheck(c, db)
		cancel()
		add("integrity_check", err == nil, errString(err, "ok"))
		ms, _ := store.LoadMigrations(migrations.SQLite, "sqlite")
		s, err := db.MigrationStatus(ctx, ms)
		// Pending migrations are applied by `migrate up` / `serve`, so they are
		// reported, not failed (an upgrade preflight runs doctor before them).
		// Drift or versions unknown to this binary fail.
		ss := summarize(s, ms)
		add("migrations", err == nil && !ss.Drift, errString(err, fmt.Sprintf("%d known, %d applied, pending=%v unknown=%v drifted=%v",
			ss.Known, ss.Applied, ss.Pending, ss.Unknown, ss.Drifted)))
		add("wal_bytes", true, strconv.FormatInt(db.Stats().WALBytes, 10))
		db.Close()
	}
}

func doctorPostgres(ctx context.Context, cfg config.Config, add func(string, bool, string)) {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	db, err := openPostgres(c, cfg.Database, cfg.Database.DSNRef, "", false)
	if err != nil {
		add("database", false, err.Error())
		return
	}
	defer db.Close()
	var ver string
	_ = db.Write.QueryRowContext(c, "SHOW server_version").Scan(&ver)
	add("postgres", true, "server "+ver+", collation bytewise (C)")
	fsys, dir := migrations.Main(store.Postgres)
	ms, _ := store.LoadMigrations(fsys, dir)
	s, err := db.MigrationStatus(c, ms)
	ss := summarize(s, ms)
	add("migrations", err == nil && !ss.Drift, errString(err, fmt.Sprintf("%d known, %d applied, pending=%v unknown=%v drifted=%v",
		ss.Known, ss.Applied, ss.Pending, ss.Unknown, ss.Drifted)))
}
