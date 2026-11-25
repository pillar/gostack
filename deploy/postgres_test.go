package deploy_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPostgresPITRScripts exercises the archive/restore helpers and
// pitr-restore.sh on files (no server). The full PostgreSQL flow is drilled by
// scripts/drill/pg_pitr.sh.
func TestPostgresPITRScripts(t *testing.T) {
	pgdir, _ := filepath.Abs("postgres")
	run := func(script string, args ...string) (int, string) {
		t.Helper()
		c := exec.Command("bash", append([]string{filepath.Join(pgdir, script)}, args...)...)
		out, err := c.CombinedOutput()
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), string(out)
		} else if err != nil {
			t.Fatal(err)
		}
		return 0, string(out)
	}
	for _, s := range []string{"wal-archive.sh", "wal-restore.sh", "base-backup.sh", "pitr-restore.sh"} {
		if out, err := exec.Command("bash", "-n", filepath.Join(pgdir, s)).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", s, err, out)
		}
	}
	dir := t.TempDir()
	arch := filepath.Join(dir, "archive")
	os.Mkdir(arch, 0o700)
	seg := filepath.Join(dir, "000000010000000000000003")
	os.WriteFile(seg, []byte("wal-one"), 0o600)

	if code, out := run("wal-archive.sh", seg, "000000010000000000000003", arch); code != 0 {
		t.Fatalf("archive: %d %s", code, out)
	}
	if b, _ := os.ReadFile(filepath.Join(arch, "000000010000000000000003")); string(b) != "wal-one" {
		t.Fatalf("archived content %q", b)
	}
	// Identical retry succeeds; different content is refused and not replaced.
	if code, _ := run("wal-archive.sh", seg, "000000010000000000000003", arch); code != 0 {
		t.Fatal("identical re-archive must succeed")
	}
	os.WriteFile(seg, []byte("wal-two"), 0o600)
	if code, out := run("wal-archive.sh", seg, "000000010000000000000003", arch); code == 0 || !strings.Contains(out, "refusing to overwrite") {
		t.Fatalf("different re-archive: %d %s", code, out)
	}
	if b, _ := os.ReadFile(filepath.Join(arch, "000000010000000000000003")); string(b) != "wal-one" {
		t.Fatal("archive file was replaced")
	}
	if code, _ := run("wal-archive.sh", seg, "../x", arch); code == 0 {
		t.Fatal("path in file name accepted")
	}
	if code, _ := run("wal-archive.sh", seg, "000000010000000000000004", filepath.Join(dir, "missing")); code == 0 {
		t.Fatal("missing archive dir accepted")
	}
	if ents, _ := os.ReadDir(arch); len(ents) != 1 {
		t.Fatalf("archive has leftovers: %v", ents)
	}
	// Restore helper.
	got := filepath.Join(dir, "RECOVERYXLOG")
	if code, _ := run("wal-restore.sh", "000000010000000000000003", got, arch); code != 0 {
		t.Fatal("restore failed")
	}
	if b, _ := os.ReadFile(got); string(b) != "wal-one" {
		t.Fatalf("restored %q", b)
	}
	if code, _ := run("wal-restore.sh", "00000002.history", got, arch); code == 0 {
		t.Fatal("missing file must exit non-zero (end of archive)")
	}

	// pitr-restore.sh prepares a new data directory and never reuses one.
	base := filepath.Join(dir, "base")
	os.Mkdir(base, 0o700)
	os.WriteFile(filepath.Join(base, "backup_label"), []byte("START WAL LOCATION: 0/3000028 (file 000000010000000000000003)\n"), 0o600)
	os.WriteFile(filepath.Join(base, "postgresql.auto.conf"), []byte("# auto\n"), 0o600)
	to := filepath.Join(dir, "rec")
	if code, out := run("pitr-restore.sh", "--base", base, "--archive", arch, "--to", to, "--target-time", "2026-11-25 10:00:00.123456+00"); code != 0 {
		t.Fatalf("pitr-restore: %d %s", code, out)
	}
	conf, _ := os.ReadFile(filepath.Join(to, "postgresql.auto.conf"))
	for _, want := range []string{"restore_command = '" + filepath.Join(pgdir, "wal-restore.sh") + " %f %p " + arch + "'",
		"recovery_target_time = '2026-11-25 10:00:00.123456+00'", "archive_mode = off", "recovery_target_action = 'promote'"} {
		if !strings.Contains(string(conf), want) {
			t.Fatalf("postgresql.auto.conf lacks %q:\n%s", want, conf)
		}
	}
	if _, err := os.Stat(filepath.Join(to, "recovery.signal")); err != nil {
		t.Fatal("recovery.signal missing")
	}
	if code, out := run("pitr-restore.sh", "--base", base, "--archive", arch, "--to", to); code == 0 || !strings.Contains(out, "exists") {
		t.Fatalf("existing target: %d %s", code, out)
	}
	to2 := filepath.Join(dir, "rec2")
	if code, _ := run("pitr-restore.sh", "--base", base, "--archive", arch, "--to", to2, "--target-time", "x'; DROP"); code == 0 {
		t.Fatal("quoted target accepted")
	}
	if code, _ := run("pitr-restore.sh", "--base", dir, "--archive", arch, "--to", to2); code == 0 {
		t.Fatal("non-backup base accepted")
	}
	if _, err := os.Stat(to2); !os.IsNotExist(err) {
		t.Fatal("refused restore left a directory")
	}
	// Latest (no target): no recovery_target_time line.
	to3 := filepath.Join(dir, "rec3")
	if code, out := run("pitr-restore.sh", "--base", base, "--archive", arch, "--to", to3); code != 0 {
		t.Fatalf("latest: %d %s", code, out)
	}
	if conf, _ := os.ReadFile(filepath.Join(to3, "postgresql.auto.conf")); strings.Contains(string(conf), "recovery_target_time") {
		t.Fatal("latest restore must not set a target time")
	}
}
