// Package deploy_test runs the deploy shell scripts for real inside a temporary
// DEPLOY_ROOT with systemctl, useradd, chown, curl and litestream stubbed on
// PATH. The GoStack binary, sqlite files, symlink switches, sha256 checks,
// snapshots and migration-compatibility checks are all real. What this cannot
// cover: real systemd/useradd/runuser behaviour and running as root on Linux.
package deploy_test

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

const name = "demo"

type harness struct {
	t                  *testing.T
	root, stubs, work  string
	bin, sum, unit, ls string
}

func stub(t *testing.T, dir, file, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte("#!/usr/bin/env bash\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) *harness {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("bash scripts")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	h := &harness{t: t, root: t.TempDir(), stubs: t.TempDir(), work: t.TempDir()}
	h.bin = filepath.Join(h.work, name)
	build := exec.Command("go", "build", "-o", h.bin, "../cmd/gostack")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	b, _ := os.ReadFile(h.bin)
	s := sha256.Sum256(b)
	h.sum = hex.EncodeToString(s[:])
	h.ls = filepath.Join(h.work, "calls.log")
	// systemctl: record calls; is-active succeeds. Emulates systemd's start-rate
	// limit like a real host: restarting a crash-looping (unhealthy) release
	// trips it, after which every restart is refused until reset-failed.
	stub(t, h.stubs, "systemctl", `echo "systemctl $*" >>"$STUB_LOG"
case "$1" in
reset-failed) rm -f "$STUB_WORK/start_limited" ;;
restart)
  [ -e "$STUB_WORK/start_limited" ] && { echo "Job failed: Start request repeated too quickly." >&2; exit 1; }
  bad="$(cat "$STUB_WORK/unhealthy" 2>/dev/null || true)"
  cur="$(readlink "$DEPLOY_ROOT/opt/`+name+`/current")"
  case " $bad " in *" ${cur#releases/} "*) touch "$STUB_WORK/start_limited" ;; esac ;;
esac
exit 0
`)
	stub(t, h.stubs, "useradd", `echo "useradd $*" >>"$STUB_LOG"`+"\n")
	stub(t, h.stubs, "chown", `echo "chown $*" >>"$STUB_LOG"`+"\n")
	// curl: healthy unless the current release is the one named in $STUB_WORK/unhealthy.
	stub(t, h.stubs, "curl", `echo "curl $*" >>"$STUB_LOG"
bad="$(cat "$STUB_WORK/unhealthy" 2>/dev/null || true)"
cur="$(readlink "$DEPLOY_ROOT/opt/`+name+`/current")"
case " $bad " in *" ${cur#releases/} "*) exit 22 ;; esac
exit 0
`)
	// litestream restore -config C -o OUT [-timestamp T] DB: copies $STUB_WORK/replica.db.
	stub(t, h.stubs, "litestream", `echo "litestream $*" >>"$STUB_LOG"
[ -e "$STUB_WORK/litestream_fail" ] && { echo partial >"$5"; exit 1; }
out=""; while [ $# -gt 0 ]; do [ "$1" = "-o" ] && out="$2"; shift; done
cp "$STUB_WORK/replica.db" "$out"
`)
	h.unit = filepath.Join(h.work, name+".service")
	os.WriteFile(h.unit, []byte("[Service]\nExecStart=/opt/demo/current/demo serve\n"), 0o644)
	os.WriteFile(filepath.Join(h.work, "gostack.yaml"), []byte("http:\n  listen: 127.0.0.1:18080\nkits: [game]\n"), 0o644)
	return h
}

func (h *harness) run(script string, args ...string) (int, string) {
	h.t.Helper()
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), "DEPLOY_ROOT="+h.root, "STUB_LOG="+h.ls, "STUB_WORK="+h.work,
		"PATH="+h.stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		h.t.Fatal(err)
	}
	return code, string(out)
}

func (h *harness) current(link string) string {
	t, _ := os.Readlink(filepath.Join(h.root, "opt", name, link))
	return t
}

func (h *harness) envVars() []string {
	f, err := os.Open(filepath.Join(h.root, "etc", name, "gostack.env"))
	if err != nil {
		h.t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := sc.Text(); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// app runs the installed binary like the service would.
func (h *harness) app(args ...string) (int, string) {
	h.t.Helper()
	cmd := exec.Command(filepath.Join(h.root, "opt", name, "current", name),
		append([]string{"--config", filepath.Join(h.root, "etc", name, "gostack.yaml")}, args...)...)
	cmd.Env = append(os.Environ(), h.envVars()...)
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return 0, string(out)
}

func (h *harness) dbExec(q string) {
	h.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(h.root, "var", "lib", name, "app.db"))
	if err != nil {
		h.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q); err != nil {
		h.t.Fatal(err)
	}
}

func TestScriptsSyntax(t *testing.T) {
	for _, s := range []string{"lib.sh", "install.sh", "upgrade.sh", "rollback.sh", "litestream/restore.sh", "../scripts/release.sh"} {
		if out, err := exec.Command("bash", "-n", s).CombinedOutput(); err != nil {
			t.Errorf("%s: %v\n%s", s, err, out)
		}
	}
}

func TestInstallUpgradeRollback(t *testing.T) {
	h := setup(t)
	common := []string{"--name", name, "--binary", h.bin, "--health-url", "http://127.0.0.1:18080/readyz", "--timeout", "2"}
	install := append(append([]string{}, common...), "--unit", h.unit, "--config", filepath.Join(h.work, "gostack.yaml"))

	// Wrong checksum: refused before anything is staged.
	if code, out := h.run("install.sh", append(install, "--version", "v1", "--sha256", strings.Repeat("0", 64))...); code == 0 || !strings.Contains(out, "sha256 mismatch") {
		t.Fatalf("bad sha accepted: %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(h.root, "opt", name, "releases", "v1")); !os.IsNotExist(err) {
		t.Fatal("release staged despite bad checksum")
	}
	// Missing checksum is refused too.
	if code, _ := h.run("install.sh", append(install, "--version", "v1")...); code == 0 {
		t.Fatal("unverified binary accepted")
	}
	sums := filepath.Join(h.work, "SHA256SUMS")
	os.WriteFile(sums, []byte(h.sum+"  "+name+"\n"), 0o644)
	if code, out := h.run("install.sh", append(install, "--version", "v1", "--sums", sums)...); code != 0 {
		t.Fatalf("install: %d\n%s", code, out)
	}
	if h.current("current") != "releases/v1" {
		t.Fatalf("current -> %q", h.current("current"))
	}
	if _, err := os.Stat(filepath.Join(h.root, "etc", "systemd", "system", name+".service")); err != nil {
		t.Fatal("unit not installed")
	}
	calls, _ := os.ReadFile(h.ls)
	for _, want := range []string{"useradd --system", "systemctl daemon-reload", "systemctl enable " + name, "systemctl restart " + name, "curl -fsS"} {
		if !strings.Contains(string(calls), want) {
			t.Fatalf("missing call %q in\n%s", want, calls)
		}
	}
	if code, out := h.run("install.sh", append(install, "--version", "v1", "--sums", sums)...); code == 0 || !strings.Contains(out, "already installed") {
		t.Fatalf("second install not refused: %d\n%s", code, out)
	}

	// The service (stubbed) would create and migrate the DB on start; do it for real.
	if code, out := h.app("migrate", "up"); code != 0 {
		t.Fatalf("migrate up: %s", out)
	}

	upgrade := func(v string) (int, string) {
		return h.run("upgrade.sh", append(append([]string{}, common...), "--version", v, "--sha256", h.sum)...)
	}
	if code, out := upgrade("v2"); code != 0 {
		t.Fatalf("upgrade v2: %d\n%s", code, out)
	}
	if h.current("current") != "releases/v2" || h.current("previous") != "releases/v1" {
		t.Fatalf("after upgrade current=%s previous=%s", h.current("current"), h.current("previous"))
	}
	snaps, _ := filepath.Glob(filepath.Join(h.root, "var", "lib", name, "backups", "pre-upgrade-v1-to-v2-*.db"))
	if len(snaps) != 1 {
		t.Fatalf("pre-upgrade snapshot missing: %v", snaps)
	}
	if code, out := h.app("backup", "verify", snaps[0]); code != 0 {
		t.Fatalf("snapshot does not verify: %s", out)
	}

	// Unhealthy release: automatic switch back to v2, previous restored to v1.
	os.WriteFile(filepath.Join(h.work, "unhealthy"), []byte("v3"), 0o644)
	if code, out := upgrade("v3"); code != 1 || !strings.Contains(out, "rolled back to v2 (healthy)") {
		t.Fatalf("failed upgrade: %d\n%s", code, out)
	}
	if h.current("current") != "releases/v2" || h.current("previous") != "releases/v1" {
		t.Fatalf("after failed upgrade current=%s previous=%s", h.current("current"), h.current("previous"))
	}
	os.Remove(filepath.Join(h.work, "unhealthy"))

	// Rollback to previous.
	if code, out := h.run("rollback.sh", "--name", name, "--timeout", "2"); code != 0 {
		t.Fatalf("rollback: %d\n%s", code, out)
	}
	if h.current("current") != "releases/v1" || h.current("previous") != "releases/v2" {
		t.Fatalf("after rollback current=%s previous=%s", h.current("current"), h.current("previous"))
	}

	// Upgrade that applies a migration and then fails, with the old release
	// unhealthy too: exit 3 and the pre-upgrade snapshot is named. (Simulated
	// older schema: migration 4 removed from the live database.)
	h.dbExec(`DROP TABLE web_sessions; DROP TABLE web_users; DELETE FROM schema_migrations WHERE version=4`)
	os.WriteFile(filepath.Join(h.work, "unhealthy"), []byte("v5 v1"), 0o644)
	code, out := upgrade("v5")
	if code != 3 || !strings.Contains(out, "applies migrations [4]") || !strings.Contains(out, "backups/pre-upgrade-v1-to-v5-") {
		t.Fatalf("failed migrating upgrade: %d\n%s", code, out)
	}
	if h.current("current") != "releases/v1" {
		t.Fatalf("current=%s", h.current("current"))
	}
	os.Remove(filepath.Join(h.work, "unhealthy"))
	if code, out := h.app("migrate", "up"); code != 0 {
		t.Fatal(out)
	}

	// Database now carries a migration no release knows: rollback must refuse
	// and leave current untouched; upgrade must refuse too.
	h.dbExec(`INSERT INTO schema_migrations(version,name,checksum,applied_at) VALUES(9999,'9999_future.sql','x',0)`)
	if code, out := h.run("rollback.sh", "--name", name, "--to", "v2", "--timeout", "2"); code == 0 || !strings.Contains(out, "newer than or drifted") {
		t.Fatalf("rollback onto newer schema: %d\n%s", code, out)
	}
	if h.current("current") != "releases/v1" {
		t.Fatal("refused rollback changed current")
	}
	if code, out := upgrade("v4"); code == 0 || !strings.Contains(out, "newer than or drifted") {
		t.Fatalf("upgrade onto newer schema: %d\n%s", code, out)
	}
	if h.current("current") != "releases/v1" {
		t.Fatal("refused upgrade changed current")
	}

	// A version name cannot be reused for different bytes.
	other := filepath.Join(h.work, "other")
	os.WriteFile(other, []byte("not the same binary"), 0o755)
	os.WriteFile(sums, []byte(func() string { b, _ := os.ReadFile(other); s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }()+"  other\n"), 0o644)
	if code, out := h.run("upgrade.sh", "--name", name, "--binary", other, "--version", "v2", "--sums", sums, "--timeout", "2"); code == 0 || !strings.Contains(out, "different binary") {
		t.Fatalf("version reuse: %d\n%s", code, out)
	}
}

func TestLitestreamRestoreWrapper(t *testing.T) {
	h := setup(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "gostack.yaml")
	os.WriteFile(cfg, []byte("kits: [game]\n"), 0o644)
	live := filepath.Join(dir, "app.db")
	env := append(os.Environ(), "GOSTACK_DATABASE_PATH="+live, "GOSTACK_DATABASE_ANALYTICS_PATH="+filepath.Join(dir, "an.db"))
	cmd := exec.Command(h.bin, "--config", cfg, "migrate", "up")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	cmd = exec.Command(h.bin, "--config", cfg, "backup", "snapshot", "--to", filepath.Join(h.work, "replica.db"))
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	liveSum := func() string { b, _ := os.ReadFile(live); s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	before := liveSum()
	to := filepath.Join(dir, "restore", "app.db")
	args := []string{"--bin", h.bin, "--replica-config", "/etc/litestream.yml", "--db", live, "--to", to}
	code, out := h.run("litestream/restore.sh", args...)
	if code != 0 {
		t.Fatalf("restore: %d\n%s", code, out)
	}
	var rep struct{ OK bool }
	if i := strings.Index(out, "{"); i < 0 || json.Unmarshal([]byte(out[i:strings.LastIndex(out, "}")+1]), &rep) != nil || !rep.OK {
		t.Fatalf("verify report missing/failed:\n%s", out)
	}
	if code, out := h.run("litestream/restore.sh", args...); code == 0 || !strings.Contains(out, "exists") {
		t.Fatalf("restore over existing: %d\n%s", code, out)
	}
	if code, _ := h.run("litestream/restore.sh", "--bin", h.bin, "--replica-config", "x", "--db", live, "--to", live); code == 0 {
		t.Fatal("restore onto live path accepted")
	}
	// Failed litestream run leaves no partial file and does not touch the live DB.
	os.WriteFile(filepath.Join(h.work, "litestream_fail"), nil, 0o644)
	to2 := filepath.Join(dir, "restore", "app2.db")
	if code, _ := h.run("litestream/restore.sh", "--bin", h.bin, "--replica-config", "c", "--db", live, "--to", to2); code != 1 {
		t.Fatalf("failed restore exit %d", code)
	}
	if _, err := os.Stat(to2); !os.IsNotExist(err) {
		t.Fatal("partial restore left behind")
	}
	os.Remove(filepath.Join(h.work, "litestream_fail"))
	// Corrupt replica: restored but verification fails (exit 3).
	os.WriteFile(filepath.Join(h.work, "replica.db"), []byte(strings.Repeat("garbage", 500)), 0o644)
	if code, out := h.run("litestream/restore.sh", "--bin", h.bin, "--replica-config", "c", "--db", live, "--to", filepath.Join(dir, "restore", "app3.db")); code != 3 {
		t.Fatalf("corrupt replica exit %d\n%s", code, out)
	}
	if liveSum() != before {
		t.Fatal("live database modified by restore")
	}
}

func TestReleaseScript(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-compiles a release binary")
	}
	out := t.TempDir()
	cmd := exec.Command("bash", "scripts/release.sh", "--version", "v9.9.9-test", "--out", out, "--arches", "amd64")
	cmd.Dir = ".."
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	sums, _ := os.ReadFile(filepath.Join(out, "SHA256SUMS"))
	bin := "gostack-v9.9.9-test-linux-amd64"
	for _, f := range []string{bin, bin + ".sbom.json"} {
		b, _ := os.ReadFile(filepath.Join(out, f))
		s := sha256.Sum256(b)
		if !strings.Contains(string(sums), hex.EncodeToString(s[:])+"  "+f) {
			t.Fatalf("SHA256SUMS lacks %s:\n%s", f, sums)
		}
	}
	var sbom struct {
		Path     string
		Main     struct{ Path string }
		Deps     []struct{ Path, Version, Sum string }
		Settings []struct{ Key, Value string }
	}
	b, _ := os.ReadFile(filepath.Join(out, bin+".sbom.json"))
	if err := json.Unmarshal(b, &sbom); err != nil || sbom.Main.Path != "github.com/pillar/gostack" || len(sbom.Deps) == 0 {
		t.Fatalf("sbom: %v %+v", err, sbom)
	}
	settings := map[string]string{}
	for _, s := range sbom.Settings {
		settings[s.Key] = s.Value
	}
	if settings["GOOS"] != "linux" || settings["CGO_ENABLED"] != "0" || settings["-trimpath"] != "true" {
		t.Fatalf("build settings: %v", settings)
	}
	// -trimpath keeps -ldflags out of BuildInfo; check the embedded version string instead.
	if raw, _ := os.ReadFile(filepath.Join(out, bin)); !strings.Contains(string(raw), "v9.9.9-test") {
		t.Fatal("version not embedded via -ldflags")
	}
}
