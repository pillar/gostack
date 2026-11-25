// Package driver compares SQLite drivers for ADR-0001: modernc.org/sqlite (pure
// Go, used by GoStack) and github.com/mattn/go-sqlite3 (CGO). The CGO driver is
// a dependency of this benchmarks module only; it never enters the main go.mod.
//
//	cd benchmarks && go test ./driver -run TestFeatures -v
//	cd benchmarks && go test ./driver -run '^$' -bench . -benchmem -count 5 -cpu 1,4
//
// Both drivers use the same settings as adapters/sqlite: WAL, synchronous=FULL,
// busy_timeout, BEGIN IMMEDIATE for writes, read-only (mode=ro, query_only)
// readers. The FULLFSYNC variants additionally set PRAGMA fullfsync=1, which on
// macOS makes SQLite use F_FULLFSYNC (a real flush of the drive cache, the
// closest equivalent of Linux fsync durability); it has no effect on Linux.
package driver

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "modernc.org/sqlite"
)

type drv struct {
	name, driver string
	dsn          func(path string, ro bool, fullfsync bool) string
}

var drivers = []drv{
	{"modernc", "sqlite", func(p string, ro, ff bool) string {
		q := url.Values{}
		q.Add("_pragma", "busy_timeout(5000)")
		q.Add("_pragma", "foreign_keys(1)")
		if ro {
			q.Set("mode", "ro")
			q.Add("_pragma", "query_only(1)")
		} else {
			q.Add("_pragma", "journal_mode(WAL)")
			q.Add("_pragma", "synchronous(FULL)")
			q.Set("_txlock", "immediate")
		}
		if ff {
			q.Add("_pragma", "fullfsync(1)")
		}
		return "file:" + p + "?" + q.Encode()
	}},
	{"mattn", "sqlite3", func(p string, ro, ff bool) string {
		q := url.Values{}
		q.Set("_busy_timeout", "5000")
		q.Set("_foreign_keys", "1")
		if ro {
			q.Set("mode", "ro")
			q.Set("_query_only", "1")
		} else {
			q.Set("_journal_mode", "WAL")
			q.Set("_synchronous", "FULL")
			q.Set("_txlock", "immediate")
		}
		s := "file:" + p + "?" + q.Encode()
		return s
	}},
}

func open(tb testing.TB, d drv, path string, ro, ff bool, conns int) *sql.DB {
	db, err := sql.Open(d.driver, d.dsn(path, ro, ff))
	if err != nil {
		tb.Fatal(err)
	}
	db.SetMaxOpenConns(conns)
	db.SetMaxIdleConns(conns)
	tb.Cleanup(func() { db.Close() })
	if ff && d.name == "mattn" {
		// mattn has no DSN key for fullfsync; with one connection the PRAGMA sticks.
		if _, err := db.Exec("PRAGMA fullfsync=1"); err != nil {
			tb.Fatal(err)
		}
	}
	var mode string
	if !ro {
		if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
			tb.Fatalf("%s journal_mode=%q err=%v", d.name, mode, err)
		}
		var sync int
		if err := db.QueryRow("PRAGMA synchronous").Scan(&sync); err != nil || sync != 2 {
			tb.Fatalf("%s synchronous=%d (want 2=FULL) err=%v", d.name, sync, err)
		}
	}
	return db
}

const schema = `CREATE TABLE kv(id INTEGER PRIMARY KEY, k TEXT NOT NULL, v BLOB NOT NULL, at INTEGER NOT NULL);
CREATE INDEX kv_k ON kv(k);`

func payload(n int) []byte { return []byte(strings.Repeat("x", n)) }

// BenchmarkWriteFULL: one short BEGIN IMMEDIATE transaction inserting one ~1 KiB
// row per op on the single write connection (the GameKit write path shape).
func BenchmarkWriteFULL(b *testing.B) {
	for _, ff := range []bool{false, true} {
		for _, d := range drivers {
			name := d.name
			if ff {
				name += "/fullfsync"
			}
			b.Run(name, func(b *testing.B) {
				db := open(b, d, filepath.Join(b.TempDir(), "w.db"), false, ff, 1)
				if _, err := db.Exec(schema); err != nil {
					b.Fatal(err)
				}
				v := payload(1024)
				ctx := context.Background()
				b.ResetTimer()
				for i := range b.N {
					tx, err := db.BeginTx(ctx, nil)
					if err != nil {
						b.Fatal(err)
					}
					if _, err := tx.Exec(`INSERT INTO kv(k,v,at) VALUES(?,?,?)`, fmt.Sprintf("k%09d", i), v, i); err != nil {
						b.Fatal(err)
					}
					if err := tx.Commit(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkBatchInsert1000: 1000 prepared inserts (~1 KiB rows) per transaction; one op = one batch.
func BenchmarkBatchInsert1000(b *testing.B) {
	for _, d := range drivers {
		b.Run(d.name, func(b *testing.B) {
			db := open(b, d, filepath.Join(b.TempDir(), "b.db"), false, false, 1)
			if _, err := db.Exec(schema); err != nil {
				b.Fatal(err)
			}
			v := payload(1024)
			b.ResetTimer()
			n := 0
			for range b.N {
				tx, err := db.Begin()
				if err != nil {
					b.Fatal(err)
				}
				st, err := tx.Prepare(`INSERT INTO kv(k,v,at) VALUES(?,?,?)`)
				if err != nil {
					b.Fatal(err)
				}
				for range 1000 {
					n++
					if _, err := st.Exec(fmt.Sprintf("k%09d", n), v, n); err != nil {
						b.Fatal(err)
					}
				}
				st.Close()
				if err := tx.Commit(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

const readRows = 100_000

func seedRead(b *testing.B, d drv, path string) {
	w := open(b, d, path, false, false, 1)
	if _, err := w.Exec(schema); err != nil {
		b.Fatal(err)
	}
	tx, _ := w.Begin()
	st, _ := tx.Prepare(`INSERT INTO kv(id,k,v,at) VALUES(?,?,?,?)`)
	v := payload(1024)
	for i := 1; i <= readRows; i++ {
		if _, err := st.Exec(i, fmt.Sprintf("k%09d", i), v, i); err != nil {
			b.Fatal(err)
		}
	}
	st.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	if _, err := w.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkPointRead4: 4 goroutines over a 4-connection read-only pool doing
// primary-key lookups of ~1 KiB rows in a 100k-row table; one op = one read.
func BenchmarkPointRead4(b *testing.B) {
	for _, d := range drivers {
		b.Run(d.name, func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "r.db")
			seedRead(b, d, path)
			r := open(b, d, path, true, false, 4)
			st, err := r.Prepare(`SELECT k, v, at FROM kv WHERE id=?`)
			if err != nil {
				b.Fatal(err)
			}
			defer st.Close()
			b.ResetTimer()
			var wg sync.WaitGroup
			per := (b.N + 3) / 4
			for g := range 4 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					rng := rand.New(rand.NewPCG(uint64(g), 7))
					var k string
					var v []byte
					var at int64
					for range per {
						if err := st.QueryRow(rng.IntN(readRows)+1).Scan(&k, &v, &at); err != nil {
							b.Error(err)
							return
						}
					}
				}()
			}
			wg.Wait()
		})
	}
}

// TestFeatures records the SQLite version and checks FTS5 and JSON support of each driver.
func TestFeatures(t *testing.T) {
	for _, d := range drivers {
		db := open(t, d, filepath.Join(t.TempDir(), "f.db"), false, false, 1)
		var ver string
		if err := db.QueryRow(`SELECT sqlite_version()`).Scan(&ver); err != nil {
			t.Fatal(err)
		}
		_, ftsErr := db.Exec(`CREATE VIRTUAL TABLE docs USING fts5(body)`)
		fts := ftsErr == nil
		if fts {
			if _, err := db.Exec(`INSERT INTO docs(body) VALUES('hello benchmark world')`); err != nil {
				t.Fatal(err)
			}
			var n int
			if err := db.QueryRow(`SELECT count(*) FROM docs WHERE docs MATCH 'benchmark'`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("%s fts5 match n=%d err=%v", d.name, n, err)
			}
		}
		var js string
		jsErr := db.QueryRow(`SELECT json_extract('{"a":1}','$.a')`).Scan(&js)
		rows, err := db.Query(`PRAGMA compile_options`)
		if err != nil {
			t.Fatal(err)
		}
		var opts []string
		for rows.Next() {
			var o string
			_ = rows.Scan(&o)
			if strings.Contains(o, "FTS") || strings.Contains(o, "THREADSAFE") || strings.Contains(o, "DEFAULT_SYNC") || strings.Contains(o, "WAL") {
				opts = append(opts, o)
			}
		}
		rows.Close()
		t.Logf("FEATURES driver=%s sqlite=%s fts5=%v fts5_err=%v json=%v compile_options=%v", d.name, ver, fts, ftsErr, jsErr == nil, opts)
	}
}
