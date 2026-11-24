package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pillar/gostack/adapters/pgtest"
	"github.com/pillar/gostack/adapters/postgres"
	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/migrations"
)

func TestPoolsReadOnlyAndUniqueViolation(t *testing.T) {
	db := pgtest.OpenStore(t, 8)
	ctx := context.Background()
	if db.Dialect != store.Postgres || !db.IsPostgres() {
		t.Fatalf("dialect %q", db.Dialect)
	}
	if _, err := db.Read.Exec(`INSERT INTO games(game_id,name,status,created_at,updated_at) VALUES('g','g','active',0,0)`); err == nil {
		t.Fatal("read pool accepted a write")
	}
	ins := func() error {
		return db.WriteTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.Exec(`INSERT INTO games(game_id,name,status,created_at,updated_at) VALUES(?,?,'active',?,?)`, "g", "g", 0, 0)
			return err
		})
	}
	if err := ins(); err != nil {
		t.Fatal(err)
	}
	if err := ins(); !store.IsUniqueViolation(err) {
		t.Fatalf("second insert err=%v, want unique violation", err)
	}
	// Go bools are stored as 0/1 like SQLite.
	if _, err := db.Write.Exec(`INSERT INTO ops_redeem_batches(game_id,batch_id,shared,max_redemptions,grants,created_at) VALUES('g','b',?,1,'{}',0)`, true); err != nil {
		t.Fatal(err)
	}
	var shared bool
	var n int64
	if err := db.ReadTx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT shared, shared FROM ops_redeem_batches WHERE batch_id=?`, "b").Scan(&shared, &n)
	}); err != nil || !shared || n != 1 {
		t.Fatalf("bool round trip %v %d %v", shared, n, err)
	}
}

func TestMigrationsIdempotentAndDriftDetected(t *testing.T) {
	db := pgtest.OpenStore(t, 8)
	ctx := context.Background()
	fsys, dir := migrations.Main(store.Postgres)
	ms, err := store.LoadMigrations(fsys, dir)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := db.Migrate(ctx, ms)
	if err != nil || len(applied) != 0 {
		t.Fatalf("second migrate applied=%v err=%v", applied, err)
	}
	st, err := store.MigrationStatusOf(ctx, db.Read, store.Postgres, ms)
	if err != nil || store.Pending(st) {
		t.Fatalf("pending after migrate: %+v %v", st, err)
	}
	tampered := append([]store.Migration(nil), ms...)
	tampered[0].Checksum = "x"
	if _, err := db.Migrate(ctx, tampered); err == nil {
		t.Fatal("drift not detected")
	}
	if _, err := db.Migrate(ctx, ms[:1]); err == nil {
		t.Fatal("unknown applied version not detected")
	}
}

// Concurrent instances migrating one fresh database serialize on the advisory lock.
func TestConcurrentMigrate(t *testing.T) {
	dsn := pgtest.NewDatabase(t)
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pgtest.OpenDSN(t, dsn, 8) // fails the test on a migration error
		}()
	}
	wg.Wait()
}

func TestLedgersAppendOnly(t *testing.T) {
	db := pgtest.OpenStore(t, 8)
	for _, q := range []string{
		`INSERT INTO games(game_id,name,status,created_at,updated_at) VALUES('g','g','active',0,0)`,
		`INSERT INTO players(game_id,player_id,created_at) VALUES('g','p',0)`,
		`INSERT INTO game_ledger(id,game_id,player_id,asset,amount,balance_after,kind,source_key,created_at) VALUES('l','g','p','coin',5,5,'reward','s',0)`,
		`INSERT INTO billing_accounts(tenant_id,id,currency,created_at,updated_at) VALUES('t','a','USD',0,0)`,
		`INSERT INTO billing_ledger_entries(id,tenant_id,account_id,type,amount,delta_available,delta_reserved,idempotency_key,created_at,seq) VALUES('e','t','a','credit',1,1,0,'k',0,1)`,
	} {
		if _, err := db.Write.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`UPDATE game_ledger SET amount=6`, `DELETE FROM game_ledger`, `TRUNCATE game_ledger CASCADE`,
		`UPDATE billing_ledger_entries SET amount=6`, `DELETE FROM billing_ledger_entries`, `TRUNCATE billing_ledger_entries`,
	} {
		if _, err := db.Write.Exec(q); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("%s: err=%v, want append-only", q, err)
		}
	}
	if _, err := db.Write.Exec(`INSERT INTO players(game_id,player_id,created_at) VALUES('missing','p',0)`); err == nil {
		t.Fatal("foreign key not enforced")
	}
}

// SERIALIZABLE + bounded retry turns read-then-write code (written for SQLite's
// single writer) into serial behaviour even across two connection pools.
func TestSerializableRetryPreservesReadModifyWrite(t *testing.T) {
	dsn := pgtest.NewDatabase(t)
	dbs := []*store.DB{pgtest.OpenDSN(t, dsn, 64), pgtest.OpenDSN(t, dsn, 64)}
	ctx := context.Background()
	if _, err := dbs[0].Write.Exec(`INSERT INTO games(game_id,name,status,created_at,updated_at) VALUES('g','g','active',0,0)`); err != nil {
		t.Fatal(err)
	}
	const workers, each = 8, 25
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []error
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db := dbs[w%2]
			for range each {
				for {
					err := db.WriteTx(ctx, func(tx *sql.Tx) error {
						var v int64
						if err := tx.QueryRow(`SELECT updated_at FROM games WHERE game_id='g'`).Scan(&v); err != nil {
							return err
						}
						_, err := tx.Exec(`UPDATE games SET updated_at=? WHERE game_id='g'`, v+1)
						return err
					})
					if errors.Is(err, store.ErrContention) {
						continue // retries exhausted: the caller may retry (HTTP maps it to 503)
					}
					if err != nil {
						mu.Lock()
						failures = append(failures, err)
						mu.Unlock()
					}
					break
				}
			}
		}()
	}
	wg.Wait()
	if len(failures) > 0 {
		t.Fatal(failures[0])
	}
	var v int64
	if err := dbs[0].Read.QueryRow(`SELECT updated_at FROM games WHERE game_id='g'`).Scan(&v); err != nil || v != workers*each {
		t.Fatalf("counter=%d want %d (lost updates) %v", v, workers*each, err)
	}
	if dbs[0].Stats().Busy+dbs[1].Stats().Busy == 0 {
		t.Log("no serialization conflict observed (scheduling); retries not exercised in this run")
	}
}

// A retryable failure is retried even when fn swallows it and returns another
// error (the connection remembers the SQLSTATE).
func TestRetryWhenFnReplacesError(t *testing.T) {
	db := pgtest.OpenStore(t, 8)
	ctx := context.Background()
	runs := 0
	err := db.WriteTx(ctx, func(tx *sql.Tx) error {
		runs++
		if runs == 1 {
			_, err := tx.Exec(`DO $$ BEGIN RAISE EXCEPTION 'synthetic' USING ERRCODE = '40001'; END $$`)
			if err == nil {
				t.Fatal("expected synthetic serialization failure")
			}
			return errors.New("business error hiding the cause")
		}
		return nil
	})
	if err != nil || runs != 2 {
		t.Fatalf("runs=%d err=%v, want a retry and success", runs, err)
	}
	if db.Stats().Busy != 1 {
		t.Fatalf("busy=%d", db.Stats().Busy)
	}
	// Non-retryable errors are returned at once.
	runs = 0
	want := errors.New("plain")
	if err := db.WriteTx(ctx, func(*sql.Tx) error { runs++; return want }); !errors.Is(err, want) || runs != 1 {
		t.Fatalf("runs=%d err=%v", runs, err)
	}
}

func TestWriteQueueBounded(t *testing.T) {
	db := pgtest.OpenStore(t, 2)
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
	go func() { defer wg.Done(); _ = db.WriteTx(ctx, func(*sql.Tx) error { <-release; return nil }) }()
	deadline := time.Now().Add(2 * time.Second)
	for db.Stats().WriteWaiting < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := db.WriteTx(ctx, func(*sql.Tx) error { return nil }); !errors.Is(err, store.ErrOverloaded) {
		t.Fatalf("err=%v want ErrOverloaded", err)
	}
	close(release)
	wg.Wait()
	if db.Stats().WriteRejected != 1 {
		t.Fatalf("rejected=%d", db.Stats().WriteRejected)
	}
}

// Databases that do not sort bytewise are refused (SQLite BINARY semantics).
func TestNonCCollationRefused(t *testing.T) {
	dsn, ok := pgtest.NewDatabaseWith(t, `LC_COLLATE 'en_US.UTF-8' LC_CTYPE 'en_US.UTF-8'`)
	if !ok {
		t.Skip("server has no en_US.UTF-8 locale")
	}
	_, err := postgres.Open(context.Background(), postgres.Options{DSN: dsn})
	if !errors.Is(err, postgres.ErrCollation) {
		t.Fatalf("err=%v want ErrCollation", err)
	}
}

func TestAnalyticsSchema(t *testing.T) {
	dsn := pgtest.NewDatabase(t)
	main := pgtest.OpenDSN(t, dsn, 8)
	ctx := context.Background()
	an, err := postgres.Open(ctx, postgres.Options{DSN: dsn, Schema: "gostack_analytics"})
	if err != nil {
		t.Fatal(err)
	}
	defer an.Close()
	fsys, dir := migrations.AnalyticsFor(store.Postgres)
	ms, err := store.LoadMigrations(fsys, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := an.Migrate(ctx, ms); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.TableExists(ctx, an.Read, store.Postgres, "analytics_events"); err != nil || !ok {
		t.Fatalf("analytics table missing in its schema: %v", err)
	}
	if ok, err := store.TableExists(ctx, main.Read, store.Postgres, "analytics_events"); err != nil || ok {
		t.Fatalf("analytics table leaked into the main schema: %v", err)
	}
	// Separate schema_migrations per schema: both report no pending migrations.
	st, err := an.MigrationStatus(ctx, ms)
	if err != nil || store.Pending(st) {
		t.Fatalf("analytics status %+v %v", st, err)
	}
}
