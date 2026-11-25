// Package postgres is the Scale profile storage adapter: a pgx (database/sql)
// write pool and a read-only pool over one PostgreSQL database, returned as the
// driver-agnostic *store.DB.
//
// Write transactions run at SERIALIZABLE isolation and are retried a bounded
// number of times on serialization_failure (40001) and deadlock_detected
// (40P01). Business code was written for SQLite's single serialized writer
// (read-then-write patterns); SERIALIZABLE gives the same outcome as some
// serial order without auditing every query. Reads use READ COMMITTED for
// single statements and REPEATABLE READ READ ONLY for ReadTx snapshots.
//
// Business SQL keeps SQLite-style '?' placeholders; connections rewrite them
// (see Rebind).
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pillar/gostack/adapters/store"
)

// Options configures Open.
type Options struct {
	// DSN is a libpq-style URL or key/value string.
	DSN string
	// MaxOpenConns bounds the write pool (default 10).
	MaxOpenConns int
	// ReadConns bounds the read-only pool (default 4).
	ReadConns int
	// WriteQueue bounds callers admitted to WriteTx at once (per process).
	WriteQueue int
	// Schema, when set, is created if missing and used as the search_path
	// (the analytics database lives in its own schema).
	Schema string
	// Attempts bounds WriteTx retries on serialization failures (default 12).
	Attempts int
}

// ErrCollation is returned when the database does not sort text bytewise.
var ErrCollation = errors.New("postgres: database collation must be C (bytewise, like SQLite BINARY); create it with LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0")

// Open connects both pools, verifies the server and returns the DB. Callers run
// migrations with db.Migrate.
func Open(ctx context.Context, o Options) (*store.DB, error) {
	if o.DSN == "" {
		return nil, errors.New("postgres: dsn required")
	}
	base, err := pgx.ParseConfig(o.DSN)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse dsn: %w", err) // pgx redacts the password
	}
	if base.RuntimeParams == nil {
		base.RuntimeParams = map[string]string{}
	}
	if _, ok := base.RuntimeParams["application_name"]; !ok {
		base.RuntimeParams["application_name"] = "gostack"
	}
	if o.Schema != "" {
		base.RuntimeParams["search_path"] = pgx.Identifier{o.Schema}.Sanitize()
	}
	wcfg := base.Copy()
	rcfg := base.Copy()
	rcfg.RuntimeParams["default_transaction_read_only"] = "on"

	w := sql.OpenDB(connector{stdlib.GetConnector(*wcfg)})
	w.SetMaxOpenConns(defInt(o.MaxOpenConns, 10))
	w.SetMaxIdleConns(defInt(o.MaxOpenConns, 10))
	w.SetConnMaxIdleTime(5 * time.Minute)
	r := sql.OpenDB(connector{stdlib.GetConnector(*rcfg)})
	r.SetMaxOpenConns(defInt(o.ReadConns, 4))
	r.SetMaxIdleConns(defInt(o.ReadConns, 4))
	r.SetConnMaxIdleTime(5 * time.Minute)
	fail := func(err error) (*store.DB, error) {
		w.Close()
		r.Close()
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := w.PingContext(pctx); err != nil {
		return fail(fmt.Errorf("postgres: connect: %w", err))
	}
	var bytewise bool
	if err := w.QueryRowContext(pctx, `SELECT ('B' < 'a') AND ('Z' < '_') AND ('_' < 'a')`).Scan(&bytewise); err != nil {
		return fail(fmt.Errorf("postgres: check collation: %w", err))
	}
	if !bytewise {
		return fail(ErrCollation)
	}
	if o.Schema != "" {
		if _, err := w.ExecContext(pctx, `CREATE SCHEMA IF NOT EXISTS `+pgx.Identifier{o.Schema}.Sanitize()); err != nil && !store.IsUniqueViolation(err) && store.SQLState(err) != "42P06" {
			return fail(fmt.Errorf("postgres: create schema: %w", err))
		}
	}
	if err := r.PingContext(pctx); err != nil {
		return fail(fmt.Errorf("postgres: connect read pool: %w", err))
	}
	return store.New(store.Config{
		Dialect: store.Postgres, Write: w, Read: r, WriteQueue: o.WriteQueue,
		WriteTxOptions: &sql.TxOptions{Isolation: sql.LevelSerializable},
		ReadTxOptions:  &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true},
		Retryable:      IsRetryable, Attempts: defInt(o.Attempts, 12), Backoff: backoff,
		PinConn: true, WrapExhausted: true,
	}), nil
}

// backoff is full jitter over an exponentially growing cap (2ms .. 250ms).
func backoff(i int) time.Duration {
	c := min(2*time.Millisecond<<min(i, 10), 250*time.Millisecond)
	return time.Duration(rand.Int64N(int64(c))) + time.Millisecond
}

func defInt(v, d int) int {
	if v <= 0 {
		return d
	}
	return v
}
