package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pillar/gostack/adapters/pgtest"
	"github.com/pillar/gostack/kits/game"
)

func cli(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Main(args, Hooks{}, Env{Stdout: &out, Stderr: &errb, Getenv: func(string) (string, bool) { return "", false }})
	return code, out.String(), errb.String()
}

func exec(t *testing.T, path, q string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func cp(t *testing.T, src, dst string) {
	t.Helper()
	if err := copyFile(src, dst); err != nil {
		t.Fatal(err)
	}
}

// TestBackupDrill runs snapshot → verify → restore on real files while the
// application holds the database open, plus the refusal paths.
func TestBackupDrill(t *testing.T) {
	pgtest.SkipIfPostgres(t, "backup snapshot/verify/restore and this drill operate on SQLite files")
	c := testConfig(t)
	a := newTestApp(t, c)
	ctx := context.Background()
	if err := game.CreateGame(ctx, a.DB, "default", "g1", "G1", nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if err := a.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO players(game_id,player_id,created_at) VALUES('g1','p1',?)`, now); err != nil {
			return err
		}
		return game.ApplyLedger(ctx, tx, "g1", "p1", "reward", "drill-1", map[string]int64{"gold": 50}, now)
	}); err != nil {
		t.Fatal(err)
	}
	set := []string{"--set", "database.path=" + c.Database.Path, "--set", "database.analytics_path=" + c.Database.AnalyticsPath}
	dir := t.TempDir()
	snap := filepath.Join(dir, "snap.db")

	if code, _, e := cli(t, append(set, "backup", "snapshot", "--to", snap)...); code != ExitOK {
		t.Fatalf("snapshot exit %d: %s", code, e)
	}
	if code, _, e := cli(t, append(set, "backup", "snapshot", "--to", snap)...); code != ExitError || !strings.Contains(e, "refusing to overwrite") {
		t.Fatalf("snapshot over existing: exit %d %s", code, e)
	}
	before, _, _ := fileSHA256(snap)
	code, out, e := cli(t, "--json", "backup", "verify", snap)
	if code != ExitOK {
		t.Fatalf("verify exit %d: %s %s", code, out, e)
	}
	var rep VerifyReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.OK || rep.Integrity != "ok" || rep.Reconcile == nil || len(rep.Reconcile) != 0 || rep.Schema.Drift || len(rep.Schema.Pending) != 0 || rep.Schema.Applied != rep.Schema.Known {
		t.Fatalf("verify report: %+v", rep)
	}
	if after, _, _ := fileSHA256(snap); after != before {
		t.Fatal("verify modified the backup file")
	}

	// Ledger mismatch is detected.
	bad := filepath.Join(dir, "bad.db")
	cp(t, snap, bad)
	exec(t, bad, `UPDATE wallet_balances SET balance=balance+1`)
	if code, out, _ := cli(t, "backup", "verify", bad); code != ExitFailing || !strings.Contains(out, "1 mismatches") {
		t.Fatalf("tampered ledger: exit %d\n%s", code, out)
	}
	// A database from a newer binary is refused for verify and restore.
	newer := filepath.Join(dir, "newer.db")
	cp(t, snap, newer)
	exec(t, newer, `INSERT INTO schema_migrations(version,name,checksum,applied_at) VALUES(9999,'9999_future.sql','x',0)`)
	if code, out, _ := cli(t, "backup", "verify", newer); code != ExitFailing || !strings.Contains(out, "unknown=[9999]") {
		t.Fatalf("newer schema: exit %d\n%s", code, out)
	}
	if code, _, _ := cli(t, "backup", "restore", "--from", newer, "--to", filepath.Join(dir, "r-newer.db")); code != ExitFailing {
		t.Fatalf("restore of newer schema: exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "r-newer.db")); !os.IsNotExist(err) {
		t.Fatal("failed restore left a file behind")
	}
	// Garbage is rejected.
	junk := filepath.Join(dir, "junk.db")
	os.WriteFile(junk, bytes.Repeat([]byte("not sqlite "), 1000), 0o600)
	if code, _, _ := cli(t, "backup", "verify", junk); code != ExitFailing {
		t.Fatalf("junk verify exit %d", code)
	}
	// Truncated snapshot (partial upload) is rejected.
	trunc := filepath.Join(dir, "trunc.db")
	raw, _ := os.ReadFile(snap)
	os.WriteFile(trunc, raw[:len(raw)/2], 0o600)
	if code, _, _ := cli(t, "backup", "verify", trunc); code != ExitFailing {
		t.Fatalf("truncated verify exit %d", code)
	}

	// Restore to a new path, then refuse to overwrite it.
	restored := filepath.Join(dir, "restore", "app.db")
	if code, out, e := cli(t, "backup", "restore", "--from", snap, "--to", restored); code != ExitOK {
		t.Fatalf("restore exit %d: %s %s", code, out, e)
	}
	if code, _, e := cli(t, "backup", "restore", "--from", snap, "--to", restored); code != ExitError || !strings.Contains(e, "refusing to restore over existing") {
		t.Fatalf("restore over existing: exit %d %s", code, e)
	}
	// The restored database serves: open it like the server does and check data.
	rc := c
	rc.Database.Path = restored
	rc.Database.AnalyticsPath = filepath.Join(dir, "restore", "analytics.db")
	ra := newTestApp(t, rc)
	var bal int64
	if err := ra.DB.Read.QueryRow(`SELECT balance FROM wallet_balances WHERE game_id='g1' AND player_id='p1' AND asset='gold'`).Scan(&bal); err != nil || bal != 50 {
		t.Fatalf("restored balance %d err %v", bal, err)
	}
	if mm, err := game.Reconcile(ctx, ra.DB.Read, ""); err != nil || len(mm) != 0 {
		t.Fatalf("restored reconcile %v %v", mm, err)
	}
}

func TestMigrateStatusJSONReportsUnknownVersions(t *testing.T) {
	pgtest.SkipIfPostgres(t, "backup snapshot/verify/restore and this drill operate on SQLite files")
	c := testConfig(t)
	a := newTestApp(t, c)
	a.Close()
	set := []string{"--set", "database.path=" + c.Database.Path, "--set", "database.analytics_path=" + c.Database.AnalyticsPath}
	code, out, _ := cli(t, append(append([]string{"--json"}, set...), "migrate", "status")...)
	if code != ExitOK || !strings.Contains(out, "\n  \"drift\": false") {
		t.Fatalf("status exit %d\n%s", code, out)
	}
	exec(t, c.Database.Path, `INSERT INTO schema_migrations(version,name,checksum,applied_at) VALUES(9999,'9999_future.sql','x',0)`)
	code, out, _ = cli(t, append(append([]string{"--json"}, set...), "migrate", "status")...)
	if code != ExitFailing || !strings.Contains(out, "\n  \"drift\": true") || !strings.Contains(out, `"unknown_versions": [
    9999
  ]`) {
		t.Fatalf("status with newer db: exit %d\n%s", code, out)
	}
}
