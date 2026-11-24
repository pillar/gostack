package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pillar/gostack/adapters/sqlite"
	"github.com/pillar/gostack/migrations"
)

func opts(t *testing.T) sqlite.Options {
	return sqlite.Options{
		Path: filepath.Join(t.TempDir(), "app.db"), ReadConnections: 3, Synchronous: "FULL",
		BusyTimeout: 2 * time.Second, CacheSizeKiB: 4096, WALAutocheckpoint: 1000, WriteQueue: 64,
	}
}

func openMigrated(t *testing.T, o sqlite.Options) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ms, err := sqlite.LoadMigrations(migrations.SQLite, "sqlite")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(context.Background(), ms); err != nil {
		t.Fatal(err)
	}
	if err := db.OpenReaders(o); err != nil {
		t.Fatal(err)
	}
	return db
}

func pragma(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var v string
	if err := db.QueryRow("PRAGMA " + name).Scan(&v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return v
}

// A02: every connection in each pool has the expected PRAGMAs.
func TestPerConnectionPragmas(t *testing.T) {
	o := opts(t)
	db := openMigrated(t, o)
	ctx := context.Background()
	if got := pragma(t, db.Write, "journal_mode"); got != "wal" {
		t.Fatalf("journal_mode=%s", got)
	}
	if got := pragma(t, db.Write, "synchronous"); got != "2" { // FULL
		t.Fatalf("synchronous=%s", got)
	}
	if got := pragma(t, db.Write, "foreign_keys"); got != "1" {
		t.Fatalf("write foreign_keys=%s", got)
	}
	// Pin all read connections simultaneously to prove each one is initialised.
	var conns []*sql.Conn
	for i := 0; i < o.ReadConnections; i++ {
		c, err := db.Read.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	for i, c := range conns {
		for name, want := range map[string]string{"query_only": "1", "foreign_keys": "1", "busy_timeout": "2000", "cache_size": "-4096"} {
			var v string
			if err := c.QueryRowContext(ctx, "PRAGMA "+name).Scan(&v); err != nil || v != want {
				t.Fatalf("read conn %d %s=%s want %s (%v)", i, name, v, want, err)
			}
		}
		c.Close()
	}
}

func TestReadPoolRejectsWrites(t *testing.T) {
	db := openMigrated(t, opts(t))
	_, err := db.Read.Exec(`INSERT INTO games(game_id,name,status,created_at,updated_at) VALUES('g','g','active',0,0)`)
	if err == nil {
		t.Fatal("read pool accepted a write")
	}
}

func TestMigrationsIdempotentAndDriftDetected(t *testing.T) {
	o := opts(t)
	db := openMigrated(t, o)
	ctx := context.Background()
	ms, _ := sqlite.LoadMigrations(migrations.SQLite, "sqlite")
	applied, err := db.Migrate(ctx, ms)
	if err != nil || len(applied) != 0 {
		t.Fatalf("second migrate applied=%v err=%v", applied, err)
	}
	st, err := sqlite.MigrationStatusOf(ctx, db.Read, ms)
	if err != nil || sqlite.Pending(st) {
		t.Fatalf("status pending after migrate: %+v %v", st, err)
	}
	tampered := append([]sqlite.Migration(nil), ms...)
	tampered[0].Checksum = "x"
	if _, err := db.Migrate(ctx, tampered); err == nil {
		t.Fatal("drift not detected")
	}
	// Binary older than database: unknown applied version must be refused.
	if _, err := db.Migrate(ctx, ms[:1]); err == nil {
		t.Fatal("unknown applied version not detected")
	}
}

func TestSingleInstanceLock(t *testing.T) {
	o := opts(t)
	db := openMigrated(t, o)
	_ = db
	if _, err := sqlite.Open(o); !errors.Is(err, sqlite.ErrLocked) {
		t.Fatalf("second open err=%v, want ErrLocked", err)
	}
}

func TestWriteQueueBounded(t *testing.T) {
	o := opts(t)
	o.WriteQueue = 2
	db := openMigrated(t, o)
	ctx := context.Background()
	release := make(chan struct{})
	started := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = db.WriteTx(ctx, func(*sql.Tx) error { close(started); <-release; return nil })
	}()
	<-started
	wg.Add(1)
	go func() { defer wg.Done(); _ = db.WriteTx(ctx, func(*sql.Tx) error { return nil }) }()
	// Wait until the second caller is admitted, then the third must be rejected.
	deadline := time.Now().Add(2 * time.Second)
	for db.Stats().WriteWaiting < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := db.WriteTx(ctx, func(*sql.Tx) error { return nil }); !errors.Is(err, sqlite.ErrOverloaded) {
		t.Fatalf("err=%v want ErrOverloaded", err)
	}
	close(release)
	wg.Wait()
	if db.Stats().WriteRejected != 1 {
		t.Fatalf("rejected=%d", db.Stats().WriteRejected)
	}
}

func TestLedgerAppendOnly(t *testing.T) {
	db := openMigrated(t, opts(t))
	ctx := context.Background()
	err := db.WriteTx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`INSERT INTO games(game_id,name,status,created_at,updated_at) VALUES('g','g','active',0,0)`,
			`INSERT INTO players(game_id,player_id,created_at) VALUES('g','p',0)`,
			`INSERT INTO game_ledger(id,game_id,player_id,asset,amount,balance_after,kind,source_key,created_at) VALUES('l','g','p','coin',5,5,'reward','s',0)`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write.Exec(`UPDATE game_ledger SET amount=6`); err == nil {
		t.Fatal("ledger update allowed")
	}
	if _, err := db.Write.Exec(`DELETE FROM game_ledger`); err == nil {
		t.Fatal("ledger delete allowed")
	}
	if _, err := db.Write.Exec(`INSERT INTO players(game_id,player_id,created_at) VALUES('missing','p',0)`); err == nil {
		t.Fatal("foreign key not enforced")
	}
}
