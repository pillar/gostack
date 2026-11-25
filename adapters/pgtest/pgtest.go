// Package pgtest runs storage-backed tests against PostgreSQL when the
// environment variable GOSTACK_TEST_POSTGRES_DSN points at a server where the
// connecting role may CREATE DATABASE, e.g.
//
//	GOSTACK_TEST_POSTGRES_DSN=postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable
//
// Every test gets a fresh database gs_test_<random> (C collation) that is
// dropped in t.Cleanup. Without the variable the helpers report "not enabled"
// and tests keep using SQLite.
package pgtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pillar/gostack/adapters/postgres"
	"github.com/pillar/gostack/adapters/sqlite"
	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/migrations"
)

// EnvDSN is the variable that enables PostgreSQL test runs.
const EnvDSN = "GOSTACK_TEST_POSTGRES_DSN"

// DSN returns the admin DSN ("" when PostgreSQL tests are disabled).
func DSN() string { return os.Getenv(EnvDSN) }

// Enabled reports whether tests should run against PostgreSQL.
func Enabled() bool { return DSN() != "" }

// SkipIfPostgres skips t when the suite runs against PostgreSQL.
func SkipIfPostgres(t testing.TB, reason string) {
	t.Helper()
	if Enabled() {
		t.Skip("SQLite-only under " + EnvDSN + ": " + reason)
	}
}

// RequirePostgres skips t unless PostgreSQL tests are enabled.
func RequirePostgres(t testing.TB) string {
	t.Helper()
	if !Enabled() {
		t.Skip(EnvDSN + " not set")
	}
	return DSN()
}

// NewDatabase creates an empty C-collated database and returns its DSN. The
// database is dropped (FORCE) when the test ends.
func NewDatabase(t testing.TB) string {
	t.Helper()
	dsn, err := create(t, `LC_COLLATE 'C' LC_CTYPE 'C'`)
	if err != nil {
		t.Fatalf("pgtest: %v", err)
	}
	return dsn
}

// NewDatabaseWith creates a database with the given locale clause; ok is false
// when the server rejects it (e.g. the locale is not installed).
func NewDatabaseWith(t testing.TB, locale string) (dsn string, ok bool) {
	t.Helper()
	dsn, err := create(t, locale)
	return dsn, err == nil
}

func create(t testing.TB, locale string) (string, error) {
	admin := RequirePostgres(t)
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "gs_test_" + hex.EncodeToString(b[:])
	db, err := openAdmin(admin)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = db.ExecContext(ctx, `CREATE DATABASE `+name+` TEMPLATE template0 ENCODING 'UTF8' `+locale)
	db.Close()
	if err != nil {
		return "", fmt.Errorf("create database: %w", err)
	}
	t.Cleanup(func() {
		db, err := openAdmin(admin)
		if err != nil {
			t.Errorf("pgtest: drop %s: %v", name, err)
			return
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := db.ExecContext(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Errorf("pgtest: drop %s: %v", name, err)
		}
	})
	return withDatabase(t, admin, name), nil
}

// DSNRef writes dsn to a private file and returns a file: secret reference.
func DSNRef(t testing.TB, dsn string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "dsn")
	if err := os.WriteFile(p, []byte(dsn), 0o600); err != nil {
		t.Fatal(err)
	}
	return "file:" + p
}

func openAdmin(dsn string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(1)
	return db, nil
}

// withDatabase returns admin with its database name replaced.
func withDatabase(t testing.TB, admin, name string) string {
	t.Helper()
	if u, err := url.Parse(admin); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		u.Path = "/" + name
		return u.String()
	}
	// key=value form
	var parts []string
	for _, f := range strings.Fields(admin) {
		if !strings.HasPrefix(f, "dbname=") {
			parts = append(parts, f)
		}
	}
	return strings.Join(append(parts, "dbname="+name), " ")
}

// UseScale switches cfg to profile scale on a fresh PostgreSQL database when
// PostgreSQL tests are enabled, and reports whether it did. Analytics uses the
// same database (schema gostack_analytics).
func UseScale(t testing.TB, cfg *config.Config) bool {
	t.Helper()
	if !Enabled() {
		return false
	}
	cfg.Profile = "scale"
	cfg.Database.Driver = "postgres"
	cfg.Database.DSNRef = DSNRef(t, NewDatabase(t))
	cfg.Queue.Driver = "postgres"
	cfg.RateLimit.AcceptPerInstance = true
	cfg.Backup.Enabled = false
	cfg.Database.MaxOpenConnections = 4 // keep the suite within a default max_connections=100
	return true
}

// OpenStore opens a migrated main database on a fresh PostgreSQL database
// (closed in t.Cleanup). writeQueue <= 0 means 64.
func OpenStore(t testing.TB, writeQueue int) *store.DB {
	t.Helper()
	return OpenDSN(t, NewDatabase(t), writeQueue)
}

// OpenDSN opens and migrates the main schema of an existing database.
func OpenDSN(t testing.TB, dsn string, writeQueue int) *store.DB {
	t.Helper()
	if writeQueue <= 0 {
		writeQueue = 64
	}
	ctx := context.Background()
	db, err := postgres.Open(ctx, postgres.Options{DSN: dsn, WriteQueue: writeQueue, MaxOpenConns: 4, ReadConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	fsys, dir := migrations.Main(store.Postgres)
	ms, err := store.LoadMigrations(fsys, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(ctx, ms); err != nil {
		t.Fatal(err)
	}
	return db
}

// Open returns a migrated main database: a fresh PostgreSQL database when
// PostgreSQL tests are enabled (with o.WriteQueue), otherwise SQLite at o.Path
// (instance lock, migrations, read pool). It is closed in t.Cleanup.
func Open(t testing.TB, o sqlite.Options) *store.DB {
	t.Helper()
	if Enabled() {
		return OpenStore(t, o.WriteQueue)
	}
	db, err := sqlite.Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ms, err := store.LoadMigrations(migrations.SQLite, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(context.Background(), ms); err != nil {
		t.Fatal(err)
	}
	if err := sqlite.OpenReaders(db, o); err != nil {
		t.Fatal(err)
	}
	return db
}
