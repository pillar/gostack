// Package sqlite is the Lite profile storage adapter: one write connection, a
// read-only pool, per-connection PRAGMAs, an exclusive process lock and a bounded
// write admission queue.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrOverloaded is returned when the write admission queue is full.
var ErrOverloaded = errors.New("sqlite: write queue full")

// ErrLocked is returned when another process holds the database instance lock.
var ErrLocked = errors.New("sqlite: database is locked by another instance")

type Options struct {
	Path              string
	ReadConnections   int
	Synchronous       string // FULL | NORMAL
	BusyTimeout       time.Duration
	CacheSizeKiB      int
	MmapSize          int64
	WALAutocheckpoint int
	WriteQueue        int
	// SkipLock is for read-only tooling (doctor, migrate status) that must not
	// take the single-instance lock.
	SkipLock bool
}

// DB holds separate write and read pools over one SQLite file.
type DB struct {
	Write *sql.DB
	Read  *sql.DB
	path  string
	lock  *os.File
	slots chan struct{}

	waiting  atomic.Int64
	rejected atomic.Int64
	busy     atomic.Int64
}

// Open acquires the instance lock and opens the write pool. Call OpenReaders
// after migrations, matching the PRD initialization order.
func Open(o Options) (*DB, error) {
	if o.Path == "" {
		return nil, errors.New("sqlite: path required")
	}
	if err := os.MkdirAll(filepath.Dir(o.Path), 0o750); err != nil {
		return nil, fmt.Errorf("sqlite: create dir: %w", err)
	}
	db := &DB{path: o.Path, slots: make(chan struct{}, max(o.WriteQueue, 1))}
	if !o.SkipLock {
		f, err := os.OpenFile(o.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("sqlite: open lock: %w", err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			return nil, fmt.Errorf("%w: %s", ErrLocked, o.Path)
		}
		db.lock = f
	}
	w, err := sql.Open("sqlite", dsn(o, false))
	if err != nil {
		db.Close()
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	db.Write = w
	if err := w.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: open write: %w", err)
	}
	var mode string
	if err := w.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		db.Close()
		return nil, fmt.Errorf("sqlite: journal_mode is %q, want wal (%v)", mode, err)
	}
	return db, nil
}

// OpenReaders opens the read-only pool (mode=ro, query_only=1).
func (db *DB) OpenReaders(o Options) error {
	r, err := sql.Open("sqlite", dsn(o, true))
	if err != nil {
		return err
	}
	n := max(o.ReadConnections, 1)
	r.SetMaxOpenConns(n)
	r.SetMaxIdleConns(n)
	if err := r.Ping(); err != nil {
		r.Close()
		return fmt.Errorf("sqlite: open read: %w", err)
	}
	db.Read = r
	return nil
}

func dsn(o Options, readOnly bool) string {
	q := url.Values{}
	add := func(p string) { q.Add("_pragma", p) }
	add(fmt.Sprintf("busy_timeout(%d)", o.BusyTimeout.Milliseconds()))
	add("foreign_keys(1)")
	if o.CacheSizeKiB > 0 {
		add(fmt.Sprintf("cache_size(-%d)", o.CacheSizeKiB))
	}
	add(fmt.Sprintf("mmap_size(%d)", o.MmapSize))
	if readOnly {
		q.Set("mode", "ro")
		add("query_only(1)")
	} else {
		add("journal_mode(WAL)")
		add(fmt.Sprintf("synchronous(%s)", strings.ToUpper(defaultStr(o.Synchronous, "FULL"))))
		add(fmt.Sprintf("wal_autocheckpoint(%d)", o.WALAutocheckpoint))
		q.Set("_txlock", "immediate")
	}
	return "file:" + o.Path + "?" + q.Encode()
}

func defaultStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Close releases pools and the instance lock.
func (db *DB) Close() error {
	var errs []error
	if db.Read != nil {
		errs = append(errs, db.Read.Close())
	}
	if db.Write != nil {
		errs = append(errs, db.Write.Close())
	}
	if db.lock != nil {
		errs = append(errs, db.lock.Close())
	}
	return errors.Join(errs...)
}

func (db *DB) Path() string { return db.path }

// Stats reports write-queue pressure for metrics and readiness.
type Stats struct {
	WriteWaiting  int64
	WriteRejected int64
	Busy          int64
	WALBytes      int64
}

func (db *DB) Stats() Stats {
	s := Stats{WriteWaiting: db.waiting.Load(), WriteRejected: db.rejected.Load(), Busy: db.busy.Load()}
	if fi, err := os.Stat(db.path + "-wal"); err == nil {
		s.WALBytes = fi.Size()
	}
	return s
}

// Overloaded reports whether the write queue is at least 90% full.
func (db *DB) Overloaded() bool {
	return db.waiting.Load()*10 >= int64(cap(db.slots))*9
}

// WriteTx runs fn in a BEGIN IMMEDIATE transaction on the single write
// connection. Admission is bounded: when WriteQueue callers are already waiting
// it fails fast with ErrOverloaded instead of queueing without limit. fn must be
// short and must not perform network IO. SQLITE_BUSY is retried a bounded number
// of times; fn must therefore be safe to re-run (it runs in a fresh transaction).
func (db *DB) WriteTx(ctx context.Context, fn func(*sql.Tx) error) error {
	select {
	case db.slots <- struct{}{}:
	default:
		db.rejected.Add(1)
		return ErrOverloaded
	}
	db.waiting.Add(1)
	defer func() { db.waiting.Add(-1); <-db.slots }()

	const attempts = 3
	var err error
	for i := 0; i < attempts; i++ {
		err = db.writeOnce(ctx, fn)
		if !IsBusy(err) {
			return err
		}
		db.busy.Add(1)
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(time.Duration(10*(i+1)) * time.Millisecond):
		}
	}
	return err
}

func (db *DB) writeOnce(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := db.Write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ReadTx runs fn in a read-only transaction giving a consistent snapshot.
func (db *DB) ReadTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := db.Read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(tx)
}

// IsBusy reports SQLITE_BUSY / SQLITE_LOCKED.
func IsBusy(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		c := se.Code() & 0xff
		return c == sqlite3.SQLITE_BUSY || c == sqlite3.SQLITE_LOCKED
	}
	return false
}

// IsUniqueViolation reports a UNIQUE or PRIMARY KEY constraint failure.
func IsUniqueViolation(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || se.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
	}
	return false
}

// IntegrityCheck runs PRAGMA integrity_check and returns nil when the result is "ok".
func (db *DB) IntegrityCheck(ctx context.Context) error {
	rows, err := db.Write.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	var msgs []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return err
		}
		msgs = append(msgs, m)
	}
	if len(msgs) == 1 && msgs[0] == "ok" {
		return nil
	}
	return fmt.Errorf("integrity_check: %s", strings.Join(msgs, "; "))
}
