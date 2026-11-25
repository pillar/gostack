package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pillar/gostack/adapters/pgtest"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/kits/game"
	"github.com/pillar/gostack/modules/billing"
)

func TestPGDSNKeepsPasswordOutOfArgs(t *testing.T) {
	for _, in := range []string{
		"postgres://app:s3cr%40t@db.example:5433/game?sslmode=require&application_name=x",
		"postgres://app@db.example:5433/game?password=s3cr%40t&sslmode=require",
		"host=db.example port=5433 user=app password='s3cr@t \\' x' dbname=game sslmode=require",
	} {
		d, err := parsePGDSN(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if p := d.password(); !strings.HasPrefix(p, "s3cr@t") {
			t.Fatalf("password %q", p)
		}
		bare := d.render(false)
		if strings.Contains(bare, "s3cr") {
			t.Fatalf("password left in %q", bare)
		}
		orig, _ := pgx.ParseConfig(in)
		for _, s := range []string{bare, d.render(true)} {
			c, err := pgx.ParseConfig(s)
			if err != nil {
				t.Fatalf("reparse %q: %v", s, err)
			}
			if c.Host != orig.Host || c.Port != orig.Port || c.User != orig.User || c.Database != orig.Database || (c.TLSConfig == nil) != (orig.TLSConfig == nil) {
				t.Fatalf("render %q lost fields: %+v", s, c)
			}
		}
		if full, _ := pgx.ParseConfig(d.render(true)); full.Password != orig.Password {
			t.Fatalf("password not preserved: %q vs %q", full.Password, orig.Password)
		}
		if w := d.withDB("gs_verify_x"); w.dbName() != "gs_verify_x" || w.password() != d.password() {
			t.Fatalf("withDB: %q", w.render(false))
		}
		env := strings.Join(d.toolEnv(), "\n")
		if !strings.Contains(env, "PGPASSWORD="+d.password()) {
			t.Fatal("PGPASSWORD not set for tools")
		}
	}
	if _, err := parsePGDSN("postgres://u:pw@[::1"); err == nil || strings.Contains(err.Error(), "pw") {
		t.Fatalf("bad dsn error %v", err)
	}
}

// pgBackupEnv enables the PostgreSQL backup tests: GOSTACK_TEST_POSTGRES_DSN
// and the client tools in PATH.
func pgBackupEnv(t *testing.T) config.Config {
	t.Helper()
	pgtest.RequirePostgres(t)
	for _, b := range []string{"pg_dump", "pg_restore"} {
		if _, err := osexec.LookPath(b); err != nil {
			t.Skip(b + " not in PATH")
		}
	}
	return testConfig(t)
}

func pgSet(c config.Config, args ...string) []string {
	set := []string{"--set", "profile=scale", "--set", "database.driver=postgres",
		"--set", "database.dsn_ref=" + c.Database.DSNRef, "--set", "queue.driver=postgres",
		"--set", "rate_limit.accept_per_instance=true"}
	if c.Database.AnalyticsDSNRef != "" {
		set = append(set, "--set", "database.analytics_dsn_ref="+c.Database.AnalyticsDSNRef)
	}
	return append(set, args...)
}

func seedPG(t *testing.T, a *App) {
	t.Helper()
	ctx := context.Background()
	if err := game.CreateGame(ctx, a.DB, "default", "g1", "G1", nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for i, p := range []string{"p1", "p2"} {
		if err := a.DB.WriteTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.Exec(`INSERT INTO players(game_id,player_id,created_at) VALUES('g1',?,?) ON CONFLICT DO NOTHING`, p, now); err != nil {
				return err
			}
			return game.ApplyLedger(ctx, tx, "g1", p, "reward", "drill-"+p, map[string]int64{"gold": int64(50 * (i + 1))}, now)
		}); err != nil {
			t.Fatal(err)
		}
	}
	b := &billing.Service{DB: a.DB}
	if _, err := b.CreateAccount(ctx, "t1", "k1", "acc1", "USD"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Credit(ctx, "t1", "k2", "acc1", 700, "seed"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Reserve(ctx, "t1", "k3", "acc1", 200, time.Hour, "hold"); err != nil {
		t.Fatal(err)
	}
}

func execDSNRef(t *testing.T, ref, q string) {
	t.Helper()
	d, err := resolvePGDSN(ref)
	if err != nil {
		t.Fatal(err)
	}
	db, err := openPlain(d)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q); err != nil {
		t.Fatal(err)
	}
}

func verifyDBsLeft(t *testing.T) int {
	t.Helper()
	d, _ := parsePGDSN(pgtest.DSN())
	db, err := openPlain(d)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pg_database WHERE datname LIKE 'gs\_verify\_%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestPostgresBackupDrill runs snapshot → verify → restore against a real
// PostgreSQL server with pg_dump/pg_restore, plus the refusal and tamper paths.
func TestPostgresBackupDrill(t *testing.T) {
	c := pgBackupEnv(t)
	a := newTestApp(t, c)
	seedPG(t, a)
	ctx := context.Background()
	dir := t.TempDir()
	snap := filepath.Join(dir, "snap.dump")
	leftBefore := verifyDBsLeft(t)

	code, out, e := cli(t, pgSet(c, "--json", "backup", "snapshot", "--to", snap)...)
	if code != ExitOK {
		t.Fatalf("snapshot exit %d: %s %s", code, out, e)
	}
	var info PGSnapshotInfo
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatal(err)
	}
	m := info.Info.Main
	if !IsPGDump(snap) || m.Tables["public.players"] != 2 || m.Tables["public.billing_accounts"] != 1 ||
		m.Ledger["public.game_ledger.gold"] != 150 || m.Ledger["public.billing_ledger_entries.delta_available"] != 500 ||
		m.SchemaVersion["public"] == 0 || m.SchemaVersion[AnalyticsSchema] == 0 || info.Info.Analytics != nil {
		t.Fatalf("manifest: %+v", info.Info)
	}
	if st, err := os.Stat(snap + ".json"); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("manifest file: %v", err)
	}
	if code, _, e := cli(t, pgSet(c, "backup", "snapshot", "--to", snap)...); code != ExitError || !strings.Contains(e, "refusing to overwrite") {
		t.Fatalf("snapshot over existing: exit %d %s", code, e)
	}
	// Only the manifest exists: still refused, nothing written.
	only := filepath.Join(dir, "only.dump")
	os.WriteFile(only+".json", []byte("{}"), 0o600)
	if code, _, _ := cli(t, pgSet(c, "backup", "snapshot", "--to", only)...); code != ExitError {
		t.Fatalf("snapshot over existing manifest: exit %d", code)
	}
	if _, err := os.Stat(only); !os.IsNotExist(err) {
		t.Fatal("refused snapshot left an archive")
	}

	code, out, e = cli(t, pgSet(c, "--json", "backup", "verify", snap)...)
	if code != ExitOK {
		t.Fatalf("verify exit %d: %s %s", code, out, e)
	}
	var rep PGVerifyReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.OK || rep.Restore != "ok" || rep.Reconcile == nil || len(rep.Reconcile) != 0 || rep.BillingReconcile == nil ||
		len(rep.BillingReconcile) != 0 || len(rep.ManifestDiffs) != 0 || rep.Schema.Drift || rep.Schema.Applied != rep.Schema.Known ||
		rep.AnalyticsSchema == nil || rep.AnalyticsSchema.Applied != rep.AnalyticsSchema.Known || !strings.HasPrefix(rep.Database, "gs_verify_") {
		t.Fatalf("verify report: %+v", rep)
	}
	if n := verifyDBsLeft(t); n != leftBefore {
		t.Fatalf("temporary verify databases left behind: %d", n-leftBefore)
	}
	// --dsn-ref works without a PostgreSQL configuration (file detected by magic).
	if code, out, e := cli(t, "backup", "verify", "--dsn-ref", c.Database.DSNRef, snap); code != ExitOK || !strings.Contains(out, "result:      OK") {
		t.Fatalf("verify --dsn-ref exit %d: %s %s", code, out, e)
	}
	if code, _, e := cli(t, "backup", "verify", snap); code != ExitError || !strings.Contains(e, "--dsn-ref") {
		t.Fatalf("verify PG archive without server: exit %d %s", code, e)
	}

	cpWith := func(name string, edit func(m *PGManifest), cut bool) string {
		p := filepath.Join(dir, name)
		raw, _ := os.ReadFile(snap)
		if cut {
			raw = raw[:len(raw)*2/3]
		}
		os.WriteFile(p, raw, 0o600)
		var mm PGManifest
		b, _ := os.ReadFile(snap + ".json")
		json.Unmarshal(b, &mm)
		mm.Main.SHA256, mm.Main.Bytes, _ = fileSHA256(p)
		if edit != nil {
			edit(&mm)
		}
		b, _ = json.Marshal(mm)
		os.WriteFile(p+".json", b, 0o600)
		return p
	}
	// Manifest disagrees with the data (lost rows) → exit 3 with the difference.
	lost := cpWith("lost.dump", func(m *PGManifest) { m.Main.Tables["public.players"] = 3 }, false)
	if code, out, _ := cli(t, pgSet(c, "backup", "verify", lost)...); code != ExitFailing || !strings.Contains(out, "public.players: manifest 3, restored 2") {
		t.Fatalf("manifest mismatch: exit %d\n%s", code, out)
	}
	// Truncated archive with a matching checksum → pg_restore fails → exit 3.
	trunc := cpWith("trunc.dump", nil, true)
	if code, out, _ := cli(t, pgSet(c, "backup", "verify", trunc)...); code != ExitFailing || !strings.Contains(out, "restore:     failed") {
		t.Fatalf("truncated: exit %d\n%s", code, out)
	}
	// Truncated archive against the original manifest → checksum refusal.
	raw, _ := os.ReadFile(snap)
	bad := filepath.Join(dir, "bad.dump")
	os.WriteFile(bad, raw[:len(raw)-10], 0o600)
	mb, _ := os.ReadFile(snap + ".json")
	os.WriteFile(bad+".json", mb, 0o600)
	if code, out, _ := cli(t, pgSet(c, "backup", "verify", bad)...); code != ExitFailing || !strings.Contains(out, "checksum differs") {
		t.Fatalf("checksum: exit %d\n%s", code, out)
	}
	// Missing manifest and non-archive files.
	nom := filepath.Join(dir, "nomanifest.dump")
	os.WriteFile(nom, raw, 0o600)
	if code, out, _ := cli(t, pgSet(c, "backup", "verify", nom)...); code != ExitFailing || !strings.Contains(out, "manifest") {
		t.Fatalf("no manifest: exit %d\n%s", code, out)
	}
	junk := filepath.Join(dir, "junk.dump")
	os.WriteFile(junk, []byte(strings.Repeat("junk", 100)), 0o600)
	if code, _, _ := cli(t, pgSet(c, "backup", "verify", junk)...); code != ExitFailing {
		t.Fatalf("junk: exit %d", code)
	}
	if n := verifyDBsLeft(t); n != leftBefore {
		t.Fatalf("temporary verify databases left behind after failures: %d", n-leftBefore)
	}

	// Restore refuses a non-empty target (the live database itself) and literal DSNs.
	if code, _, e := cli(t, pgSet(c, "backup", "restore", "--from", snap, "--to-dsn-ref", c.Database.DSNRef)...); code != ExitError || !strings.Contains(e, "not empty") {
		t.Fatalf("restore over non-empty: exit %d %s", code, e)
	}
	if code, _, _ := cli(t, pgSet(c, "backup", "restore", "--from", snap, "--to-dsn-ref", "postgres://x@h/db")...); code != ExitUsage {
		t.Fatalf("literal dsn: exit %d", code)
	}
	// A failed (truncated) restore leaves the target empty.
	target := pgtest.DSNRef(t, pgtest.NewDatabase(t))
	if code, _, _ := cli(t, pgSet(c, "backup", "restore", "--from", trunc, "--to-dsn-ref", target)...); code != ExitFailing {
		t.Fatalf("truncated restore: exit %d", code)
	}
	td, _ := resolvePGDSN(target)
	if err := checkEmptyTarget(ctx, td, false); err != nil {
		t.Fatalf("target not empty after failed restore: %v", err)
	}
	code, out, e = cli(t, pgSet(c, "--json", "backup", "restore", "--from", snap, "--to-dsn-ref", target)...)
	if code != ExitOK {
		t.Fatalf("restore exit %d: %s %s", code, out, e)
	}
	// The restored database serves.
	rc := c
	rc.Database.DSNRef = target
	ra := newTestApp(t, rc)
	var bal int64
	if err := ra.DB.Read.QueryRow(`SELECT balance FROM wallet_balances WHERE game_id='g1' AND player_id='p2' AND asset='gold'`).Scan(&bal); err != nil || bal != 100 {
		t.Fatalf("restored balance %d err %v", bal, err)
	}
	acc, err := (&billing.Service{DB: ra.DB}).GetAccount(ctx, "t1", "acc1")
	if err != nil || acc.Available != 500 || acc.Reserved != 200 {
		t.Fatalf("restored account %+v %v", acc, err)
	}
	// Identity/sequence state survives: a new ledger entry gets a fresh id.
	if _, err := (&billing.Service{DB: ra.DB}).Credit(ctx, "t1", "k9", "acc1", 1, "after restore"); err != nil {
		t.Fatalf("write after restore: %v", err)
	}
	if code, _, e := cli(t, pgSet(c, "backup", "restore", "--from", snap, "--to-dsn-ref", target)...); code != ExitError || !strings.Contains(e, "not empty") {
		t.Fatalf("second restore: exit %d %s", code, e)
	}

	// Ledger tampering and a schema from a newer binary are detected.
	execDSNRef(t, c.Database.DSNRef, `UPDATE wallet_balances SET balance=balance+1 WHERE player_id='p1'`)
	execDSNRef(t, c.Database.DSNRef, `INSERT INTO schema_migrations(version,name,checksum,applied_at) VALUES(9999,'9999_future.sql','x',0)`)
	snap2 := filepath.Join(dir, "snap2.dump")
	if code, _, e := cli(t, pgSet(c, "backup", "snapshot", "--to", snap2)...); code != ExitOK {
		t.Fatalf("snapshot2: %d %s", code, e)
	}
	code, out, _ = cli(t, pgSet(c, "backup", "verify", snap2)...)
	if code != ExitFailing || !strings.Contains(out, "reconcile:   1 mismatches") || !strings.Contains(out, "unknown=[9999]") {
		t.Fatalf("tampered: exit %d\n%s", code, out)
	}
}

// TestPostgresBackupSeparateAnalytics covers database.analytics_dsn_ref: the
// analytics schema is dumped from its own database and restored separately.
func TestPostgresBackupSeparateAnalytics(t *testing.T) {
	c := pgBackupEnv(t)
	c.Database.AnalyticsDSNRef = pgtest.DSNRef(t, pgtest.NewDatabase(t))
	a := newTestApp(t, c)
	seedPG(t, a)
	dir := t.TempDir()
	snap := filepath.Join(dir, "s.dump")
	code, out, e := cli(t, pgSet(c, "--json", "backup", "snapshot", "--to", snap)...)
	if code != ExitOK {
		t.Fatalf("snapshot: %d %s %s", code, out, e)
	}
	var info PGSnapshotInfo
	json.Unmarshal([]byte(out), &info)
	if info.Info.Analytics == nil || info.Analytics != snap+".analytics" || !IsPGDump(snap+".analytics") ||
		len(info.Info.Analytics.Schemas) != 1 || info.Info.Analytics.SchemaVersion[AnalyticsSchema] == 0 {
		t.Fatalf("analytics part: %+v", info)
	}
	if _, ok := info.Info.Main.SchemaVersion[AnalyticsSchema]; ok {
		t.Fatalf("main dump unexpectedly has analytics schema: %+v", info.Info.Main)
	}
	if code, out, e := cli(t, pgSet(c, "backup", "verify", snap)...); code != ExitOK {
		t.Fatalf("verify: %d %s %s", code, out, e)
	}
	target := pgtest.DSNRef(t, pgtest.NewDatabase(t))
	atarget := pgtest.DSNRef(t, pgtest.NewDatabase(t))
	if code, out, e := cli(t, pgSet(c, "backup", "restore", "--from", snap, "--to-dsn-ref", target, "--analytics-to-dsn-ref", atarget)...); code != ExitOK {
		t.Fatalf("restore: %d %s %s", code, out, e)
	}
	rc := c
	rc.Database.DSNRef, rc.Database.AnalyticsDSNRef = target, atarget
	newTestApp(t, rc) // opens and migrates both without error
	// Missing analytics archive is detected.
	os.Remove(snap + ".analytics")
	if code, out, _ := cli(t, pgSet(c, "backup", "verify", snap)...); code != ExitFailing || !strings.Contains(out, "analytics archive") {
		t.Fatalf("missing analytics: %d %s", code, out)
	}
}
