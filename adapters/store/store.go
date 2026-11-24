// Package store is the driver-agnostic storage handle shared by every GoStack
// package. A *DB is produced by a dialect opener (adapters/sqlite for the Lite
// profile, adapters/postgres for Scale) and offers the same contract on both:
//
//   - WriteTx: bounded admission (ErrOverloaded instead of unbounded queueing)
//     and a bounded retry of fn on transient conflicts. fn must therefore be
//     re-runnable (it always runs in a fresh transaction) and must not perform
//     network IO.
//   - ReadTx: a read-only transaction with a consistent snapshot.
//
// Business SQL uses '?' placeholders and the portable subset of SQL accepted by
// both SQLite and PostgreSQL; the PostgreSQL opener rewrites placeholders.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Dialects.
const (
	SQLite   = "sqlite"
	Postgres = "postgres"
)

// ErrOverloaded is returned when the write admission queue is full.
var ErrOverloaded = errors.New("store: write queue full")

// ErrContention is returned (wrapping the last driver error) when a write
// transaction kept failing with retryable conflicts (SQLITE_BUSY,
// serialization_failure, deadlock_detected) after the bounded retries. It
// wraps ErrOverloaded so callers map it to 503 like a full queue.
var ErrContention = fmt.Errorf("%w: write conflicts persisted after bounded retries", ErrOverloaded)

// Config describes a DB built by a dialect opener.
type Config struct {
	Dialect string
	Write   *sql.DB
	Read    *sql.DB // may be set later (SQLite opens readers after migrations)
	// Path is the SQLite file ("" for PostgreSQL).
	Path       string
	WriteQueue int
	// WriteTxOptions / ReadTxOptions are passed to BeginTx.
	WriteTxOptions *sql.TxOptions
	ReadTxOptions  *sql.TxOptions
	// Retryable classifies errors after which fn is re-run in a new transaction.
	Retryable func(error) bool
	// Attempts bounds how many times fn runs (>= 1).
	Attempts int
	// Backoff returns the wait before attempt i+1 (i starts at 0).
	Backoff func(i int) time.Duration
	// PinConn runs each write transaction on a dedicated *sql.Conn and asks the
	// driver connection (via TakeRetryable) whether any statement of the
	// transaction failed with a retryable error, even when fn swallowed or
	// replaced that error.
	PinConn bool
	// WrapExhausted wraps the last error in ErrContention when every attempt
	// failed with a retryable error.
	WrapExhausted bool
	// Closers are closed after the pools (e.g. the SQLite instance lock).
	Closers []io.Closer
}

// DB holds a write pool and a read pool over one database.
type DB struct {
	Write   *sql.DB
	Read    *sql.DB
	Dialect string
	Path    string

	cfg      Config
	slots    chan struct{}
	waiting  atomic.Int64
	rejected atomic.Int64
	busy     atomic.Int64
}

// New builds a DB from c.
func New(c Config) *DB {
	if c.Attempts < 1 {
		c.Attempts = 1
	}
	if c.Retryable == nil {
		c.Retryable = func(error) bool { return false }
	}
	if c.Backoff == nil {
		c.Backoff = func(i int) time.Duration { return time.Duration(10*(i+1)) * time.Millisecond }
	}
	return &DB{Write: c.Write, Read: c.Read, Dialect: c.Dialect, Path: c.Path, cfg: c,
		slots: make(chan struct{}, max(c.WriteQueue, 1))}
}

// IsPostgres reports whether the DB is PostgreSQL.
func (db *DB) IsPostgres() bool { return db.Dialect == Postgres }

// Close releases pools and any adapter resources (instance lock).
func (db *DB) Close() error {
	var errs []error
	if db.Read != nil && db.Read != db.Write {
		errs = append(errs, db.Read.Close())
	}
	if db.Write != nil {
		errs = append(errs, db.Write.Close())
	}
	for _, c := range db.cfg.Closers {
		errs = append(errs, c.Close())
	}
	return errors.Join(errs...)
}

// Stats reports write-queue pressure for metrics and readiness.
type Stats struct {
	WriteWaiting  int64
	WriteRejected int64
	// Busy counts retried write attempts (SQLITE_BUSY or PostgreSQL
	// serialization/deadlock failures).
	Busy     int64
	WALBytes int64 // SQLite only
}

func (db *DB) Stats() Stats {
	s := Stats{WriteWaiting: db.waiting.Load(), WriteRejected: db.rejected.Load(), Busy: db.busy.Load()}
	if db.Path != "" {
		if fi, err := os.Stat(db.Path + "-wal"); err == nil {
			s.WALBytes = fi.Size()
		}
	}
	return s
}

// Overloaded reports whether the write queue is at least 90% full.
func (db *DB) Overloaded() bool {
	return db.waiting.Load()*10 >= int64(cap(db.slots))*9
}

// WriteTx runs fn in a write transaction (SQLite: BEGIN IMMEDIATE on the single
// writer; PostgreSQL: SERIALIZABLE). Admission is bounded: when WriteQueue
// callers are already inside WriteTx it fails fast with ErrOverloaded. Retryable
// conflicts re-run fn in a fresh transaction a bounded number of times; fn must
// be safe to re-run and must not perform network IO.
func (db *DB) WriteTx(ctx context.Context, fn func(*sql.Tx) error) error {
	select {
	case db.slots <- struct{}{}:
	default:
		db.rejected.Add(1)
		return ErrOverloaded
	}
	db.waiting.Add(1)
	defer func() { db.waiting.Add(-1); <-db.slots }()
	var err error
	for i := 0; i < db.cfg.Attempts; i++ {
		var retry bool
		err, retry = db.writeOnce(ctx, fn)
		if !retry {
			return err
		}
		db.busy.Add(1)
		if i == db.cfg.Attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(db.cfg.Backoff(i)):
		}
	}
	if db.cfg.WrapExhausted {
		return fmt.Errorf("%w: %w", ErrContention, err)
	}
	return err
}

// takeRetryable is implemented by driver connections that remember whether a
// statement failed with a retryable error since the last call.
type takeRetryable interface{ TakeRetryable() bool }

func (db *DB) writeOnce(ctx context.Context, fn func(*sql.Tx) error) (err error, retry bool) {
	if !db.cfg.PinConn {
		tx, err := db.Write.BeginTx(ctx, db.cfg.WriteTxOptions)
		if err != nil {
			return err, db.cfg.Retryable(err)
		}
		if err := fn(tx); err != nil {
			_ = tx.Rollback()
			return err, db.cfg.Retryable(err)
		}
		err = tx.Commit()
		return err, err != nil && db.cfg.Retryable(err)
	}
	c, err := db.Write.Conn(ctx)
	if err != nil {
		return err, false
	}
	defer c.Close()
	flagged := func() (f bool) {
		_ = c.Raw(func(dc any) error {
			if t, ok := dc.(takeRetryable); ok {
				f = t.TakeRetryable()
			}
			return nil
		})
		return f
	}
	flagged() // reset
	tx, err := c.BeginTx(ctx, db.cfg.WriteTxOptions)
	if err != nil {
		return err, db.cfg.Retryable(err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err, db.cfg.Retryable(err) || flagged()
	}
	err = tx.Commit()
	return err, err != nil && (db.cfg.Retryable(err) || flagged())
}

// ReadTx runs fn in a read-only transaction giving a consistent snapshot.
func (db *DB) ReadTx(ctx context.Context, fn func(*sql.Tx) error) error {
	o := db.cfg.ReadTxOptions
	if o == nil {
		o = &sql.TxOptions{ReadOnly: true}
	}
	tx, err := db.Read.BeginTx(ctx, o)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(tx)
}

// IsUniqueViolation reports a UNIQUE or PRIMARY KEY constraint failure on
// either dialect (SQLITE_CONSTRAINT_UNIQUE/PRIMARYKEY or SQLSTATE 23505).
func IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code == "23505"
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || se.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
	}
	return false
}

// SQLState returns the PostgreSQL SQLSTATE of err ("" when err is not a
// PostgreSQL error).
func SQLState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}
