package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Migration is one forward-only SQL file named NNNN_description.sql.
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
}

// MigrationStatus describes one migration's state in a database.
type MigrationStatus struct {
	Version   int    `json:"version"`
	Name      string `json:"name"`
	Applied   bool   `json:"applied"`
	AppliedAt int64  `json:"applied_at,omitempty"`
	Drift     bool   `json:"drift"` // applied checksum differs from the embedded file
}

// LoadMigrations reads *.sql files from dir in fsys.
func LoadMigrations(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []Migration
	seen := map[int]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		num, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("migration %s: name must be NNNN_description.sql", e.Name())
		}
		if seen[v] {
			return nil, fmt.Errorf("duplicate migration version %d", v)
		}
		seen[v] = true
		b, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		out = append(out, Migration{Version: v, Name: e.Name(), SQL: string(b), Checksum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

var migrationsTable = map[string]string{
	SQLite: `CREATE TABLE IF NOT EXISTS schema_migrations (
  version INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  checksum TEXT NOT NULL,
  applied_at INTEGER NOT NULL
) STRICT`,
	Postgres: `CREATE TABLE IF NOT EXISTS schema_migrations (
  version BIGINT PRIMARY KEY,
  name TEXT NOT NULL,
  checksum TEXT NOT NULL,
  applied_at BIGINT NOT NULL
)`,
}

// tableExistsSQL counts tables named ? (in the current schema on PostgreSQL).
var tableExistsSQL = map[string]string{
	SQLite:   `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`,
	Postgres: `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name=?`,
}

type querier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// TableExists reports whether table exists (in the current schema on PostgreSQL).
func TableExists(ctx context.Context, q querier, dialect, table string) (bool, error) {
	s, ok := tableExistsSQL[dialect]
	if !ok {
		return false, fmt.Errorf("store: unknown dialect %q", dialect)
	}
	var n int
	err := q.QueryRowContext(ctx, s, table).Scan(&n)
	return n > 0, err
}

// MigrationStatus compares embedded migrations with this database.
func (db *DB) MigrationStatus(ctx context.Context, ms []Migration) ([]MigrationStatus, error) {
	return MigrationStatusOf(ctx, db.Write, db.Dialect, ms)
}

// MigrationStatusOf compares embedded migrations with the database q of the
// given dialect.
func MigrationStatusOf(ctx context.Context, q querier, dialect string, ms []Migration) ([]MigrationStatus, error) {
	applied := map[int][2]string{}
	appliedAt := map[int]int64{}
	exists, err := TableExists(ctx, q, dialect, "schema_migrations")
	if err != nil {
		return nil, err
	}
	if exists {
		rows, err := q.QueryContext(ctx, `SELECT version, name, checksum, applied_at FROM schema_migrations`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var v int
			var n, c string
			var at int64
			if err := rows.Scan(&v, &n, &c, &at); err != nil {
				return nil, err
			}
			applied[v] = [2]string{n, c}
			appliedAt[v] = at
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	var out []MigrationStatus
	known := map[int]bool{}
	for _, m := range ms {
		known[m.Version] = true
		a, ok := applied[m.Version]
		out = append(out, MigrationStatus{Version: m.Version, Name: m.Name, Applied: ok, AppliedAt: appliedAt[m.Version], Drift: ok && a[1] != m.Checksum})
	}
	for v, a := range applied {
		if !known[v] {
			// Database is newer than this binary: report as drift so callers refuse to start.
			out = append(out, MigrationStatus{Version: v, Name: a[0], Applied: true, AppliedAt: appliedAt[v], Drift: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// migrateLockKey is the PostgreSQL advisory lock key held while migrating.
const migrateLockKey int64 = 0x67736d696772 // "gsmigr"

// Migrate applies pending migrations, each in its own transaction. It refuses to
// run when an applied migration's checksum changed or the database has versions
// unknown to this binary. On PostgreSQL the run holds a session advisory lock so
// instances starting together migrate one at a time.
func (db *DB) Migrate(ctx context.Context, ms []Migration) (applied []int, err error) {
	if db.Dialect != Postgres {
		return db.migrate(ctx, ms, db.Write, func(fn func(*sql.Tx) error) error { return db.WriteTx(ctx, fn) })
	}
	c, err := db.Write.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, `SELECT pg_advisory_lock(?)`, migrateLockKey); err != nil {
		return nil, err
	}
	defer c.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(?)`, migrateLockKey)
	return db.migrate(ctx, ms, c, func(fn func(*sql.Tx) error) error {
		tx, err := c.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	})
}

func (db *DB) migrate(ctx context.Context, ms []Migration, q querier, inTx func(func(*sql.Tx) error) error) (applied []int, err error) {
	ddl, ok := migrationsTable[db.Dialect]
	if !ok {
		return nil, fmt.Errorf("store: unknown dialect %q", db.Dialect)
	}
	if _, err := q.ExecContext(ctx, ddl); err != nil {
		return nil, err
	}
	st, err := MigrationStatusOf(ctx, q, db.Dialect, ms)
	if err != nil {
		return nil, err
	}
	pending := map[int]bool{}
	for _, s := range st {
		if s.Drift {
			return nil, fmt.Errorf("migration %d (%s) drift: applied checksum differs or unknown version; refusing to migrate", s.Version, s.Name)
		}
		if !s.Applied {
			pending[s.Version] = true
		}
	}
	for _, m := range ms {
		if !pending[m.Version] {
			continue
		}
		err := inTx(func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
				return fmt.Errorf("apply %s: %w", m.Name, err)
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,checksum,applied_at) VALUES(?,?,?,?)`,
				m.Version, m.Name, m.Checksum, time.Now().UnixMilli())
			return err
		})
		if err != nil {
			return applied, err
		}
		applied = append(applied, m.Version)
	}
	return applied, nil
}

// Pending reports whether any migration is unapplied or drifted.
func Pending(st []MigrationStatus) bool {
	for _, s := range st {
		if !s.Applied || s.Drift {
			return true
		}
	}
	return false
}
