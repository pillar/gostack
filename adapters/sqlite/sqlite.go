// Package sqlite is the Lite profile storage adapter: one write connection, a
// read-only pool, per-connection PRAGMAs, an exclusive process lock and a bounded
// write admission queue. It returns the driver-agnostic *store.DB.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pillar/gostack/adapters/store"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrOverloaded is returned when the write admission queue is full (alias of
// store.ErrOverloaded).
var ErrOverloaded = store.ErrOverloaded

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

// Open acquires the instance lock and opens the write pool. Call OpenReaders
// after migrations, matching the PRD initialization order.
func Open(o Options) (*store.DB, error) {
	if o.Path == "" {
		return nil, errors.New("sqlite: path required")
	}
	if err := os.MkdirAll(filepath.Dir(o.Path), 0o750); err != nil {
		return nil, fmt.Errorf("sqlite: create dir: %w", err)
	}
	c := store.Config{Dialect: store.SQLite, Path: o.Path, WriteQueue: o.WriteQueue, Retryable: IsBusy, Attempts: 3}
	var lock *os.File
	if !o.SkipLock {
		f, err := os.OpenFile(o.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("sqlite: open lock: %w", err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			return nil, fmt.Errorf("%w: %s", ErrLocked, o.Path)
		}
		lock = f
		c.Closers = []io.Closer{f}
	}
	fail := func(w *sql.DB, err error) (*store.DB, error) {
		if w != nil {
			w.Close()
		}
		if lock != nil {
			lock.Close()
		}
		return nil, err
	}
	w, err := sql.Open("sqlite", dsn(o, false))
	if err != nil {
		return fail(nil, err)
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	if err := w.Ping(); err != nil {
		return fail(w, fmt.Errorf("sqlite: open write: %w", err))
	}
	var mode string
	if err := w.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		return fail(w, fmt.Errorf("sqlite: journal_mode is %q, want wal (%v)", mode, err))
	}
	c.Write = w
	return store.New(c), nil
}

// OpenReaders opens the read-only pool (mode=ro, query_only=1) of db.
func OpenReaders(db *store.DB, o Options) error {
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

// IsBusy reports SQLITE_BUSY / SQLITE_LOCKED.
func IsBusy(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		c := se.Code() & 0xff
		return c == sqlite3.SQLITE_BUSY || c == sqlite3.SQLITE_LOCKED
	}
	return false
}

// IntegrityCheck runs PRAGMA integrity_check on db's write connection and
// returns nil when the result is "ok".
func IntegrityCheck(ctx context.Context, db *store.DB) error {
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
