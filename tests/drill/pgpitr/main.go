// Command pgpitr is the local PostgreSQL point-in-time-recovery drill for
// GoStack profile scale (PRD §11/§12): a throwaway primary with continuous WAL
// archiving (deploy/postgres/postgresql.pitr.conf + wal-archive.sh), a base
// backup taken under HTTP write load (deploy/postgres/base-backup.sh), a
// logical `backup snapshot` + `backup verify` under load, then SIGKILL of the
// whole primary process group. Two new clusters are recovered from the base
// backup + archive with deploy/postgres/pitr-restore.sh: one to a target time
// T in the middle of the load and one to the end of the archive ("latest").
// The app is started on each, `backup snapshot/verify`, `game reconcile` and
// `migrate status` run, and every acknowledged write is read back through the
// HTTP API:
//
//   - target T: every write acknowledged at or before T must be present and
//     every write whose request was sent after T (+1µs) must be absent;
//   - latest: every write acknowledged at or before the last replayed commit
//     must be present; the lost tail is reported as the observed RPO, together
//     with the WAL that was not archived at kill time.
//
// The client and the server share one clock (same host), so the bounds are
// exact up to PostgreSQL's microsecond timestamp granularity.
//
// Run it through scripts/drill/pg_pitr.sh. Passwords are random per run and
// travel only in environment variables (PGPASSWORD, the app's dsn_ref env).
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type opts struct {
	app, repo, pgbin, work string
	runs, workers, payload int
	duration, think        time.Duration
	archiveTimeout         string
}

var o opts

func main() {
	flag.StringVar(&o.app, "app", "", "application binary built from `gostack init --kit game`")
	flag.StringVar(&o.repo, "repo", ".", "GoStack repository (for deploy/postgres)")
	flag.StringVar(&o.pgbin, "pgbin", "", "directory with initdb, postgres, pg_basebackup, pg_dump, pg_restore")
	flag.StringVar(&o.work, "work", "", "empty work directory (keep the path short)")
	flag.IntVar(&o.runs, "runs", 2, "number of drills")
	flag.DurationVar(&o.duration, "duration", 60*time.Second, "write load duration per run")
	flag.IntVar(&o.workers, "workers", 8, "concurrent load workers")
	flag.DurationVar(&o.think, "think", 0, "pause between operations of one worker")
	flag.IntVar(&o.payload, "payload", 2048, "save payload bytes")
	flag.StringVar(&o.archiveTimeout, "archive-timeout", "10s", "archive_timeout of the primary")
	flag.Parse()
	if o.app == "" || o.work == "" || o.pgbin == "" {
		fatal("-app, -work and -pgbin are required")
	}
	os.Setenv("PATH", o.pgbin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var results []*runResult
	ok := true
	for i := 1; i <= o.runs; i++ {
		r, err := drill(i)
		if err != nil {
			r.Error = err.Error()
			ok = false
			logf("run %d failed: %v", i, err)
		}
		results = append(results, r)
	}
	out := map[string]any{
		"drill":     "postgres-pitr",
		"generated": time.Now().UTC().Format(time.RFC3339),
		"postgres":  version(filepath.Join(o.pgbin, "postgres"), "--version"),
		"app":       version(o.app, "version"),
		"host": map[string]any{"os": runtime.GOOS, "arch": runtime.GOARCH, "cpus": runtime.NumCPU(),
			"cpu": version("sysctl", "-n", "machdep.cpu.brand_string"), "mem_bytes": version("sysctl", "-n", "hw.memsize")},
		"load": map[string]any{"workers": o.workers, "think": o.think.String(), "duration": o.duration.String(),
			"payload_bytes": o.payload, "archive_timeout": o.archiveTimeout},
		"runs": results,
		"ok":   ok,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
	if !ok {
		os.Exit(1)
	}
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "pgpitr: "+f+"\n", a...)
	os.Exit(2)
}

func logf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "[pitr %s] "+f+"\n", append([]any{time.Now().Format("15:04:05.000")}, a...)...)
}

func version(bin string, args ...string) string {
	b, _ := exec.Command(bin, args...).CombinedOutput()
	return strings.TrimSpace(string(b))
}

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

func now() time.Time { return time.Now().Round(0) } // wall clock only (PostgreSQL compares wall time)

// ---- results ----

type check struct {
	Kind              string  `json:"kind"` // target_time | latest
	TargetTime        string  `json:"target_time,omitempty"`
	PrepareSeconds    float64 `json:"prepare_seconds"`  // pitr-restore.sh (copy base backup, write recovery settings)
	RecoverySeconds   float64 `json:"recovery_seconds"` // server start → WAL replay → promoted
	AppReadySeconds   float64 `json:"app_ready_seconds"`
	RTOSeconds        float64 `json:"rto_seconds"` // prepare + recovery + app ready
	ArchivedSegsUsed  int     `json:"archive_segments_restored"`
	RedoDoneLSN       string  `json:"redo_done_lsn"`
	LastReplayedTxAt  string  `json:"last_replayed_commit_time"`
	StopReason        string  `json:"recovery_stop,omitempty"`
	MigrateStatus     any     `json:"migrate_status"`
	ReconcileOK       bool    `json:"game_reconcile_ok"`
	BackupVerifyOK    bool    `json:"backup_verify_ok"`
	BackupVerify      any     `json:"backup_verify,omitempty"`
	Required          int     `json:"writes_required_present"`
	Forbidden         int     `json:"writes_required_absent"`
	Ambiguous         int     `json:"writes_in_flight_at_boundary"`
	MissingRequired   int     `json:"violations_missing"`
	PresentForbidden  int     `json:"violations_present"`
	PresentAmbiguous  int     `json:"in_flight_present"`
	LostAcked         int     `json:"acked_writes_lost"`
	LossWindowMs      int64   `json:"loss_window_ms,omitempty"` // kill − oldest lost ack
	RPOSeconds        float64 `json:"rpo_seconds,omitempty"`    // kill − last replayed commit
	ReachedArchiveEnd *bool   `json:"replayed_into_last_archived_segment,omitempty"`
	Error             string  `json:"error,omitempty"`
}

type runResult struct {
	Run               int     `json:"run"`
	LoadSeconds       float64 `json:"load_seconds"`
	Players           int     `json:"players"`
	AckedWrites       int     `json:"acked_writes"`
	AckedPerSecond    float64 `json:"acked_writes_per_second"`
	BaseBackupSeconds float64 `json:"base_backup_seconds"`
	BaseBackupBytes   int64   `json:"base_backup_bytes"`
	BaseBackup        any     `json:"base_backup"`
	LogicalSnapshot   struct {
		Seconds       float64 `json:"snapshot_seconds"`
		Bytes         int64   `json:"bytes"`
		VerifySeconds float64 `json:"verify_seconds"`
		VerifyOK      bool    `json:"verify_ok"`
	} `json:"logical_snapshot_under_load"`
	DBBytesAtKill       int64  `json:"database_bytes_at_kill"`
	ArchiveSegments     int    `json:"archive_segments"`
	ArchiveBytes        int64  `json:"archive_bytes"`
	LastArchived        string `json:"last_archived_wal"`
	LastArchivedAgoMs   int64  `json:"last_archive_before_kill_ms"`
	UnarchivedBytes     int64  `json:"unarchived_wal_bytes_at_last_sample"`
	SampleBeforeKillMs  int64  `json:"last_sample_before_kill_ms"`
	ReadySegmentsAtKill int    `json:"segments_ready_not_archived_at_kill"`
	Target              *check `json:"recovery_target_time"`
	Latest              *check `json:"recovery_latest"`
	Error               string `json:"error,omitempty"`
}

// ---- processes ----

type proc struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func start(logPath string, env []string, group bool, bin string, args ...string) (*proc, error) {
	lf, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	c := exec.Command(bin, args...)
	c.Stdout, c.Stderr = lf, lf
	c.Env = append(os.Environ(), env...)
	if group {
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := c.Start(); err != nil {
		lf.Close()
		return nil, err
	}
	p := &proc{cmd: c, done: make(chan struct{})}
	go func() { p.err = c.Wait(); lf.Close(); close(p.done) }()
	return p, nil
}

// killGroup SIGKILLs the process and its whole process group (postmaster and
// every backend, the archiver and the WAL writer at once: machine loss).
func (p *proc) killGroup() {
	if p == nil || p.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	<-p.done
}

func (p *proc) kill() {
	if p != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGKILL)
		<-p.done
	}
}

// stop sends sig (SIGINT = PostgreSQL fast shutdown, SIGTERM for the app).
func (p *proc) stop(sig syscall.Signal) {
	if p == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(sig)
	select {
	case <-p.done:
	case <-time.After(30 * time.Second):
		p.killGroup()
	}
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fatal("free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func run(env []string, bin string, args ...string) (string, error) {
	c := exec.Command(bin, args...)
	c.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err := c.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s %s: %v: %s %s", filepath.Base(bin), strings.Join(args, " "), err, out.String(), errb.String())
	}
	return out.String(), nil
}

func dirSize(p string) (n int64, files int) {
	_ = filepath.Walk(p, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			n += fi.Size()
			files++
		}
		return nil
	})
	return
}

// ---- PostgreSQL helpers ----

func dsn(port int, user, pass, db string) string {
	return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable", user, pass, port, db)
}

func waitPG(d string, p *proc, timeout time.Duration, promoted bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			return fmt.Errorf("postgres exited: %v", p.err)
		default:
		}
		db, err := sql.Open("pgx", d)
		if err == nil {
			var rec bool
			err = db.QueryRow(`SELECT pg_is_in_recovery()`).Scan(&rec)
			db.Close()
			if err == nil && (!promoted || !rec) {
				return nil
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return errors.New("postgres not ready in time")
}

func startPG(dataDir, logPath string, port int) (*proc, error) {
	return start(logPath, nil, true, filepath.Join(o.pgbin, "postgres"), "-D", dataDir, "-p", strconv.Itoa(port))
}

// segEnd returns the LSN just past the end of WAL segment file name (16 MiB segments).
func segEnd(name string) (uint64, bool) {
	if len(name) != 24 {
		return 0, false
	}
	hi, err1 := strconv.ParseUint(name[8:16], 16, 32)
	lo, err2 := strconv.ParseUint(name[16:24], 16, 32)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return hi<<32 + (lo+1)*(16<<20), true
}

func parseLSN(s string) uint64 {
	a, b, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return 0
	}
	hi, _ := strconv.ParseUint(a, 16, 32)
	lo, _ := strconv.ParseUint(b, 16, 32)
	return hi<<32 | lo
}

// ---- configuration ----

const gameID = "drill"

const gameConfig = `{
  "client": {"drill": true},
  "boards": {"main": {"order": "desc", "mode": "best", "period": "none", "min_score": 0, "max_score": 1000000000}},
  "rewards": {"tick": {"grants": {"coin": 1}, "period": "daily", "per_period": 1000}}
}`

func appConfig(port int) string {
	return fmt.Sprintf(`profile: scale
environment: development
http:
  listen: 127.0.0.1:%d
  max_body_bytes: 1048576
  request_timeout: 10s
database:
  driver: postgres
  dsn_ref: env:DRILL_DATABASE_DSN
  max_open_connections: 16
  read_connections: 4
cache:
  driver: memory
  max_bytes: 33554432
queue:
  driver: postgres
  workers: 2
rate_limit:
  accept_per_instance: true
log:
  level: warn
  format: json
kits: [game]
game:
  session_ttl: 24h
  save_max_bytes: 65536
  save_slots_per_player: 5
  auth_rate_per_minute: 100000
  score_rate_per_minute: 100000
  settlement:
    interval: 0s
`, port)
}

// ---- HTTP client and acknowledgement log ----

type client struct {
	base string
	hc   *http.Client
}

func (c client) do(method, path, token string, body any, hdr map[string]string) (int, []byte, error) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+"/v1/games/"+gameID+path, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

type guest struct {
	PlayerID    string `json:"player_id"`
	GuestID     string `json:"guest_id"`
	GuestSecret string `json:"guest_secret"`
	AccessToken string `json:"access_token"`
}

// write is one acknowledged write: the request was sent at sent and its
// success response arrived at acked; value is the save revision, the coin
// balance after a claim or the submitted score (monotonic per player).
type write struct {
	sent, acked time.Time
	value       int64
}

type player struct {
	g                     guest
	sent, acked           time.Time
	saves, claims, scores []write
}

type book struct {
	mu      sync.Mutex
	players []*player
}

func worker(c client, b *book, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		sent := now()
		code, raw, err := c.do("POST", "/auth/guest", "", map[string]any{}, nil)
		if err != nil || code != 200 && code != 201 {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		p := &player{sent: sent, acked: now()}
		if json.Unmarshal(raw, &p.g) != nil {
			continue
		}
		b.mu.Lock()
		b.players = append(b.players, p)
		b.mu.Unlock()
		rev, score := int64(0), int64(0)
		for i := 0; i < 60; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if o.think > 0 {
				time.Sleep(o.think)
			}
			sent := now()
			switch i % 3 {
			case 0:
				body := map[string]any{"expected_revision": rev, "schema_version": 1,
					"payload": map[string]any{"seq": rev + 1, "blob": randHex(o.payload)}}
				code, raw, err := c.do("PUT", "/saves/main", p.g.AccessToken, body, map[string]string{"Idempotency-Key": randHex(16)})
				var out struct{ Revision int64 }
				if err == nil && code == 200 && json.Unmarshal(raw, &out) == nil {
					rev = out.Revision
					b.mu.Lock()
					p.saves = append(p.saves, write{sent, now(), rev})
					b.mu.Unlock()
				}
			case 1:
				code, raw, err := c.do("POST", "/rewards/tick/claim", p.g.AccessToken, map[string]any{}, map[string]string{"Idempotency-Key": randHex(16)})
				var out struct{ Balances map[string]int64 }
				if err == nil && code == 200 && json.Unmarshal(raw, &out) == nil {
					b.mu.Lock()
					p.claims = append(p.claims, write{sent, now(), out.Balances["coin"]})
					b.mu.Unlock()
				}
			case 2:
				score += 10
				code, _, err := c.do("POST", "/boards/main/scores", p.g.AccessToken, map[string]any{"score": score}, map[string]string{"Idempotency-Key": randHex(16)})
				if err == nil && code == 200 {
					b.mu.Lock()
					p.scores = append(p.scores, write{sent, now(), score})
					b.mu.Unlock()
				}
			}
		}
	}
}

func waitReady(base string, p *proc, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			return fmt.Errorf("app exited: %v", p.err)
		default:
		}
		if resp, err := http.Get(base + "/readyz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("app not ready in time")
}

// ---- archive sampler ----

type sample struct {
	at          time.Time
	currentLSN  uint64
	lastArchive string
}

func sampler(d string, stop <-chan struct{}, mu *sync.Mutex, last *sample) {
	db, err := sql.Open("pgx", d)
	if err != nil {
		return
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		var lsn string
		var arch sql.NullString
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := db.QueryRowContext(ctx, `SELECT pg_current_wal_lsn()::text, last_archived_wal FROM pg_stat_archiver`).Scan(&lsn, &arch)
		cancel()
		if err == nil {
			mu.Lock()
			*last = sample{at: now(), currentLSN: parseLSN(lsn), lastArchive: arch.String}
			mu.Unlock()
		}
	}
}

// ---- the drill ----

func drill(n int) (res *runResult, err error) {
	res = &runResult{Run: n}
	dir, _ := filepath.Abs(filepath.Join(o.work, fmt.Sprintf("r%d", n)))
	primary, archive := filepath.Join(dir, "primary"), filepath.Join(dir, "archive")
	for _, d := range []string{dir, archive} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return res, err
		}
	}
	superPW, appPW := randHex(24), randHex(24)
	pw := filepath.Join(dir, "pwfile")
	if err := os.WriteFile(pw, []byte(superPW+"\n"), 0o600); err != nil {
		return res, err
	}
	if _, err := run(nil, filepath.Join(o.pgbin, "initdb"), "-D", primary, "-U", "postgres", "--auth=scram-sha-256",
		"--pwfile="+pw, "--locale=C", "-E", "UTF8"); err != nil {
		return res, err
	}
	os.Remove(pw)
	shipped, err := os.ReadFile(filepath.Join(o.repo, "deploy/postgres/postgresql.pitr.conf"))
	if err != nil {
		return res, err
	}
	conf := strings.NewReplacer("/opt/gostack/deploy/postgres", filepath.Join(o.repo, "deploy/postgres"),
		"/var/lib/postgresql/wal-archive", archive, "archive_timeout = 60s", "archive_timeout = "+o.archiveTimeout).Replace(string(shipped))
	conf += "\nlisten_addresses = '127.0.0.1'\nunix_socket_directories = ''\nmax_connections = 100\n"
	f, err := os.OpenFile(filepath.Join(primary, "postgresql.conf"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return res, err
	}
	_, err = f.WriteString("\n# drill: deploy/postgres/postgresql.pitr.conf\n" + conf)
	f.Close()
	if err != nil {
		return res, err
	}
	port := freePort()
	pg, err := startPG(primary, filepath.Join(dir, "primary.log"), port)
	if err != nil {
		return res, err
	}
	defer pg.killGroup()
	superDSN := dsn(port, "postgres", superPW, "postgres")
	if err := waitPG(superDSN, pg, 30*time.Second, false); err != nil {
		return res, err
	}
	admin, err := sql.Open("pgx", superDSN)
	if err != nil {
		return res, err
	}
	for _, q := range []string{
		`CREATE ROLE gostack LOGIN CREATEDB PASSWORD '` + appPW + `'`,
		`CREATE DATABASE app OWNER gostack TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C'`,
	} {
		if _, err := admin.Exec(q); err != nil {
			admin.Close()
			return res, fmt.Errorf("setup: %w", err)
		}
	}
	admin.Close()
	appEnv := func(p int) []string { return []string{"DRILL_DATABASE_DSN=" + dsn(p, "gostack", appPW, "app")} }

	appPort := freePort()
	cfgPath := filepath.Join(dir, "gostack.yaml")
	if err := os.WriteFile(cfgPath, []byte(appConfig(appPort)), 0o600); err != nil {
		return res, err
	}
	gcfg := filepath.Join(dir, "game-config.json")
	if err := os.WriteFile(gcfg, []byte(gameConfig), 0o600); err != nil {
		return res, err
	}
	for _, args := range [][]string{{"migrate", "up"}, {"game", "create", gameID, "--name", "Drill"}, {"game", "config", "publish", gameID, gcfg}} {
		if _, err := run(appEnv(port), o.app, append([]string{"--config", cfgPath}, args...)...); err != nil {
			return res, err
		}
	}
	app, err := start(filepath.Join(dir, "app.log"), appEnv(port), false, o.app, "--config", cfgPath, "serve")
	if err != nil {
		return res, err
	}
	defer app.kill()
	base := fmt.Sprintf("http://127.0.0.1:%d", appPort)
	if err := waitReady(base, app, 30*time.Second); err != nil {
		return res, err
	}
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	c := client{base: base, hc: hc}

	stop := make(chan struct{})
	var smu sync.Mutex
	var last sample
	go sampler(superDSN, stop, &smu, &last)
	bk := &book{}
	var wg sync.WaitGroup
	loadStart := now()
	for w := 0; w < o.workers; w++ {
		wg.Add(1)
		go func() { defer wg.Done(); worker(c, bk, stop) }()
	}
	logf("run %d: load started (%d workers, %s, archive_timeout %s)", n, o.workers, o.duration, o.archiveTimeout)
	time.Sleep(min(3*time.Second, o.duration/6))

	// Base backup under load with the shipped script.
	baseDir := filepath.Join(dir, "base")
	t0 := time.Now()
	out, err := run([]string{"PGPASSWORD=" + superPW}, "bash", filepath.Join(o.repo, "deploy/postgres/base-backup.sh"),
		"--host", "127.0.0.1", "--port", strconv.Itoa(port), "--user", "postgres", "--to", baseDir, "--label", fmt.Sprintf("drill-%d", n))
	res.BaseBackupSeconds = time.Since(t0).Seconds()
	if err != nil {
		close(stop)
		return res, err
	}
	baseEnd := now()
	var bb map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(out)), &bb) == nil {
		res.BaseBackup = bb
	}
	res.BaseBackupBytes, _ = dirSize(baseDir)
	logf("run %d: base backup %.2fs %d bytes", n, res.BaseBackupSeconds, res.BaseBackupBytes)

	// Logical snapshot + verify under load (pg_dump, password via env).
	snap := filepath.Join(dir, "logical.dump")
	t1 := time.Now()
	if _, err := run(appEnv(port), o.app, "--config", cfgPath, "backup", "snapshot", "--to", snap); err != nil {
		close(stop)
		return res, err
	}
	res.LogicalSnapshot.Seconds = time.Since(t1).Seconds()
	res.LogicalSnapshot.Bytes, _ = dirSize(snap)
	t1 = time.Now()
	if vo, err := run(appEnv(port), o.app, "--config", cfgPath, "--json", "backup", "verify", snap); err == nil {
		res.LogicalSnapshot.VerifyOK = strings.Contains(vo, `"ok": true`)
	} else {
		logf("run %d: logical verify: %v", n, err)
	}
	res.LogicalSnapshot.VerifySeconds = time.Since(t1).Seconds()

	// Target time T in the middle of the load, after the base backup ended.
	mid := loadStart.Add(o.duration / 2)
	if !mid.After(baseEnd.Add(time.Second)) {
		mid = baseEnd.Add(time.Second)
	}
	time.Sleep(time.Until(mid))
	T := now().Truncate(time.Microsecond)
	logf("run %d: target time T=%s", n, T.UTC().Format(time.RFC3339Nano))
	time.Sleep(time.Until(loadStart.Add(o.duration)))

	// Machine loss: SIGKILL the whole PostgreSQL process group and the app.
	smu.Lock()
	lastBefore := last
	smu.Unlock()
	killAt := now()
	pg.killGroup()
	app.kill()
	close(stop)
	wg.Wait()
	res.LoadSeconds = killAt.Sub(loadStart).Seconds()
	res.SampleBeforeKillMs = killAt.Sub(lastBefore.at).Milliseconds()
	res.DBBytesAtKill, _ = dirSize(filepath.Join(primary, "base"))
	if ents, err := os.ReadDir(filepath.Join(primary, "pg_wal", "archive_status")); err == nil {
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), ".ready") {
				res.ReadySegmentsAtKill++
			}
		}
	}
	segs := archivedSegments(archive)
	res.ArchiveSegments = len(segs)
	res.ArchiveBytes, _ = dirSize(archive)
	if len(segs) > 0 {
		res.LastArchived = segs[len(segs)-1]
		if fi, err := os.Stat(filepath.Join(archive, res.LastArchived)); err == nil {
			res.LastArchivedAgoMs = killAt.Sub(fi.ModTime()).Milliseconds()
		}
		if end, ok := segEnd(res.LastArchived); ok && lastBefore.currentLSN > end {
			res.UnarchivedBytes = int64(lastBefore.currentLSN - end)
		}
	}
	for _, p := range bk.players {
		res.AckedWrites += 1 + len(p.saves) + len(p.claims) + len(p.scores)
	}
	res.Players = len(bk.players)
	res.AckedPerSecond = float64(res.AckedWrites) / res.LoadSeconds
	logf("run %d: killed primary; %d acked writes, %d archive segments, last %s, %d ready, ~%d bytes unarchived",
		n, res.AckedWrites, res.ArchiveSegments, res.LastArchived, res.ReadySegmentsAtKill, res.UnarchivedBytes)

	var errs []string
	res.Target = recoverAndCheck(dir, baseDir, archive, "target", T, killAt, bk, appEnv, segs)
	if res.Target.Error != "" {
		errs = append(errs, "target: "+res.Target.Error)
	}
	res.Latest = recoverAndCheck(dir, baseDir, archive, "latest", time.Time{}, killAt, bk, appEnv, segs)
	if res.Latest.Error != "" {
		errs = append(errs, "latest: "+res.Latest.Error)
	}
	if !res.LogicalSnapshot.VerifyOK {
		errs = append(errs, "logical snapshot under load did not verify")
	}
	if len(errs) > 0 {
		return res, errors.New(strings.Join(errs, "; "))
	}
	return res, nil
}

var segRE = regexp.MustCompile(`^[0-9A-F]{24}$`)

func archivedSegments(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if segRE.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

var (
	redoRE    = regexp.MustCompile(`redo done at ([0-9A-F]+/[0-9A-F]+)`)
	lastTxRE  = regexp.MustCompile(`last completed transaction was at log time ([0-9-]+ [0-9:.]+[+-][0-9:]+)`)
	stopRE    = regexp.MustCompile(`recovery stopping (before|after) commit of transaction \d+, time ([0-9-]+ [0-9:.]+[+-][0-9:]+)`)
	restoreRE = regexp.MustCompile(`restored log file "([0-9A-F]{24})" from archive`)
)

func parsePGTime(s string) (time.Time, error) {
	for _, l := range []string{"2006-01-02 15:04:05.999999-07", "2006-01-02 15:04:05.999999-07:00"} {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("bad time %q", s)
}

func recoverAndCheck(dir, baseDir, archive, kind string, T, killAt time.Time, bk *book, appEnv func(int) []string, segs []string) *check {
	ck := &check{Kind: "target_time"}
	if T.IsZero() {
		ck.Kind = "latest"
	}
	err := func() error {
		data := filepath.Join(dir, "rec-"+kind)
		args := []string{filepath.Join(o.repo, "deploy/postgres/pitr-restore.sh"), "--base", baseDir, "--archive", archive, "--to", data}
		if !T.IsZero() {
			ck.TargetTime = T.UTC().Format("2006-01-02 15:04:05.000000+00")
			args = append(args, "--target-time", ck.TargetTime)
		}
		t0 := time.Now()
		if _, err := run(nil, "bash", args...); err != nil {
			return err
		}
		ck.PrepareSeconds = time.Since(t0).Seconds()
		port := freePort()
		logPath := filepath.Join(dir, "rec-"+kind+".log")
		t1 := time.Now()
		pg, err := startPG(data, logPath, port)
		if err != nil {
			return err
		}
		defer pg.stop(syscall.SIGINT)
		if err := waitPG(strings.TrimPrefix(appEnv(port)[0], "DRILL_DATABASE_DSN="), pg, 10*time.Minute, true); err != nil {
			return err
		}
		ck.RecoverySeconds = time.Since(t1).Seconds()
		// Start the app on the recovered cluster.
		appPort := freePort()
		cfgPath := filepath.Join(dir, "gostack-"+kind+".yaml")
		if err := os.WriteFile(cfgPath, []byte(appConfig(appPort)), 0o600); err != nil {
			return err
		}
		t2 := time.Now()
		app, err := start(filepath.Join(dir, "app-"+kind+".log"), appEnv(port), false, o.app, "--config", cfgPath, "serve")
		if err != nil {
			return err
		}
		defer app.stop(syscall.SIGTERM)
		base := fmt.Sprintf("http://127.0.0.1:%d", appPort)
		if err := waitReady(base, app, 60*time.Second); err != nil {
			return err
		}
		ck.AppReadySeconds = time.Since(t2).Seconds()
		ck.RTOSeconds = ck.PrepareSeconds + ck.RecoverySeconds + ck.AppReadySeconds

		// Recovery facts from the server log.
		var lastTx time.Time
		if lb, err := os.ReadFile(logPath); err == nil {
			s := string(lb)
			if m := redoRE.FindStringSubmatch(s); m != nil {
				ck.RedoDoneLSN = m[1]
			}
			if m := lastTxRE.FindStringSubmatch(s); m != nil {
				ck.LastReplayedTxAt = m[1]
				lastTx, _ = parsePGTime(m[1])
			}
			if m := stopRE.FindStringSubmatch(s); m != nil {
				ck.StopReason = m[0]
			}
			seen := map[string]bool{}
			for _, m := range restoreRE.FindAllStringSubmatch(s, -1) {
				seen[m[1]] = true
			}
			ck.ArchivedSegsUsed = len(seen)
		}
		if T.IsZero() && len(segs) > 0 && ck.RedoDoneLSN != "" {
			lastStart, _ := segEnd(segs[len(segs)-1])
			reached := parseLSN(ck.RedoDoneLSN) >= lastStart-(16<<20)
			ck.ReachedArchiveEnd = &reached
		}

		// backup verify-style checks on the recovered database.
		env := appEnv(port)
		if out, err := run(env, o.app, "--config", cfgPath, "--json", "migrate", "status"); err == nil {
			var v any
			if json.Unmarshal([]byte(out), &v) == nil {
				ck.MigrateStatus = v
			}
		} else {
			return err
		}
		if _, err := run(env, o.app, "--config", cfgPath, "game", "reconcile"); err == nil {
			ck.ReconcileOK = true
		}
		snap := filepath.Join(dir, "rec-"+kind+".dump")
		if _, err := run(env, o.app, "--config", cfgPath, "backup", "snapshot", "--to", snap); err != nil {
			return err
		}
		if out, err := run(env, o.app, "--config", cfgPath, "--json", "backup", "verify", snap); err == nil {
			var v map[string]any
			if json.Unmarshal([]byte(out), &v) == nil {
				ck.BackupVerifyOK, _ = v["ok"].(bool)
				ck.BackupVerify = map[string]any{"schema": v["schema"], "reconcile_mismatches": v["reconcile_mismatches"],
					"manifest_differences": v["manifest_differences"], "database": v["database"]}
			}
		}

		// HTTP read-back.
		bound := T
		if T.IsZero() {
			if lastTx.IsZero() {
				return errors.New("latest recovery: no 'last completed transaction' in the server log")
			}
			bound = lastTx
			ck.RPOSeconds = killAt.Sub(lastTx).Seconds()
		}
		if err := readBack(client{base: base, hc: &http.Client{Timeout: 10 * time.Second}}, bk, bound, !T.IsZero(), killAt, ck); err != nil {
			return err
		}
		var probs []string
		if ck.MissingRequired > 0 {
			probs = append(probs, fmt.Sprintf("%d writes acknowledged before the bound are missing", ck.MissingRequired))
		}
		if ck.PresentForbidden > 0 {
			probs = append(probs, fmt.Sprintf("%d writes sent after the target are present", ck.PresentForbidden))
		}
		if !ck.ReconcileOK || !ck.BackupVerifyOK {
			probs = append(probs, "reconcile/backup verify failed on the recovered database")
		}
		if ck.ReachedArchiveEnd != nil && !*ck.ReachedArchiveEnd {
			probs = append(probs, "latest recovery stopped before the last archived segment")
		}
		if len(probs) > 0 {
			return errors.New(strings.Join(probs, "; "))
		}
		return nil
	}()
	if err != nil {
		ck.Error = err.Error()
	}
	logf("%s recovery: rto %.2fs (prepare %.2f, recovery %.2f, app %.2f), required %d missing %d, forbidden %d present %d, in-flight %d (present %d), lost acked %d, rpo %.2fs %s",
		kind, ck.RTOSeconds, ck.PrepareSeconds, ck.RecoverySeconds, ck.AppReadySeconds, ck.Required, ck.MissingRequired,
		ck.Forbidden, ck.PresentForbidden, ck.Ambiguous, ck.PresentAmbiguous, ck.LostAcked, ck.RPOSeconds, ck.Error)
	return ck
}

// readBack logs every player in again and classifies every acknowledged write
// against bound: acked ≤ bound ⇒ must be present; for a target-time recovery
// sent > bound (+1µs) ⇒ must be absent; anything else was in flight.
func readBack(c client, bk *book, bound time.Time, target bool, killAt time.Time, ck *check) error {
	forbidAfter := bound.Add(time.Microsecond)
	var oldestLost time.Time
	classify := func(w write, present bool) {
		switch {
		case !w.acked.After(bound):
			ck.Required++
			if !present {
				ck.MissingRequired++
			}
		case target && w.sent.After(forbidAfter):
			ck.Forbidden++
			if present {
				ck.PresentForbidden++
			}
		default:
			ck.Ambiguous++
			if present {
				ck.PresentAmbiguous++
			}
		}
		if !present {
			ck.LostAcked++
			if oldestLost.IsZero() || w.acked.Before(oldestLost) {
				oldestLost = w.acked
			}
		}
	}
	var errs []string
	for _, p := range bk.players {
		code, raw, err := c.do("POST", "/auth/guest", "", map[string]any{"guest_id": p.g.GuestID, "guest_secret": p.g.GuestSecret}, nil)
		if err != nil {
			return err
		}
		if code == 401 {
			classify(write{p.sent, p.acked, 0}, false)
			for _, ws := range [][]write{p.saves, p.claims, p.scores} {
				for _, w := range ws {
					classify(w, false)
				}
			}
			continue
		}
		if code != 200 && code != 201 {
			errs = append(errs, fmt.Sprintf("login %d %s", code, raw))
			continue
		}
		classify(write{p.sent, p.acked, 0}, true)
		var g guest
		_ = json.Unmarshal(raw, &g)
		tok := g.AccessToken
		var rev, coin, best int64
		if code, b, err := c.do("GET", "/saves/main", tok, nil, nil); err == nil && code == 200 {
			var s struct{ Revision int64 }
			_ = json.Unmarshal(b, &s)
			rev = s.Revision
		} else if code != 404 {
			errs = append(errs, fmt.Sprintf("get save %d %s %v", code, b, err))
		}
		if code, b, err := c.do("GET", "/wallet", tok, nil, nil); err == nil && code == 200 {
			var w struct{ Balances map[string]int64 }
			_ = json.Unmarshal(b, &w)
			coin = w.Balances["coin"]
		} else {
			errs = append(errs, fmt.Sprintf("wallet %d %s %v", code, b, err))
		}
		if code, b, err := c.do("GET", "/boards/main?limit=1", tok, nil, nil); err == nil && code == 200 {
			var bd struct {
				Me *struct{ Score int64 } `json:"me"`
			}
			_ = json.Unmarshal(b, &bd)
			if bd.Me != nil {
				best = bd.Me.Score
			}
		} else {
			errs = append(errs, fmt.Sprintf("board %d %s %v", code, b, err))
		}
		for _, w := range p.saves {
			classify(w, w.value <= rev)
		}
		for _, w := range p.claims {
			classify(w, w.value <= coin)
		}
		for _, w := range p.scores {
			classify(w, w.value <= best)
		}
	}
	if !oldestLost.IsZero() {
		ck.LossWindowMs = killAt.Sub(oldestLost).Milliseconds()
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d read-back errors, first: %s", len(errs), errs[0])
	}
	return nil
}
