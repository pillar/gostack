// Command litestream is the A08 local disaster-recovery drill (PRD §11):
// a GoStack game app replicates its SQLite database with Litestream (to an
// S3-compatible endpoint such as MinIO, or to a local "file" replica), takes
// write load through the HTTP API, is killed with SIGKILL together with
// Litestream, is restored with deploy/litestream/restore.sh into a new path,
// verified, restarted on the restored database and read back through the API.
// Acknowledged writes are compared with what survived; the loss window,
// replication lag, restore time and sizes are printed as JSON.
//
// Run it through scripts/drill/litestream_minio.sh, which builds the binaries
// and starts MinIO. S3 credentials come from LITESTREAM_ACCESS_KEY_ID /
// LITESTREAM_SECRET_ACCESS_KEY and are never written to files.
package main

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pillar/gostack/modules/storage"
	_ "modernc.org/sqlite"
)

var think time.Duration

type opts struct {
	app, litestream, restoreScript, work    string
	replica, s3Endpoint, s3Bucket, s3Region string
	s3CreateBucket                          bool
	runs                                    int
	duration                                time.Duration
	workers, payloadBytes                   int
	syncIntervals, checkpoints              []string
	seedPlayers                             int
}

func main() {
	var o opts
	var syncs, cps string
	flag.StringVar(&o.app, "app", "", "application binary built from `gostack init --kit game`")
	flag.StringVar(&o.litestream, "litestream", "litestream", "litestream binary")
	flag.StringVar(&o.restoreScript, "restore", "deploy/litestream/restore.sh", "restore script")
	flag.StringVar(&o.work, "work", "", "empty work directory")
	flag.StringVar(&o.replica, "replica", "s3", "s3 or file")
	flag.StringVar(&o.s3Endpoint, "s3-endpoint", "", "S3 endpoint, e.g. http://127.0.0.1:9000")
	flag.StringVar(&o.s3Bucket, "s3-bucket", "gostack-drill", "bucket (created if missing)")
	flag.StringVar(&o.s3Region, "s3-region", "us-east-1", "region")
	flag.BoolVar(&o.s3CreateBucket, "s3-create-bucket", true, "create the bucket first (false for an existing bucket and a bucket-scoped credential, e.g. R2)")
	flag.IntVar(&o.runs, "runs", 2, "number of drills")
	flag.DurationVar(&o.duration, "duration", 60*time.Second, "write load duration per run")
	flag.IntVar(&o.workers, "workers", 8, "concurrent load workers")
	flag.IntVar(&o.payloadBytes, "payload", 2048, "save payload bytes")
	flag.IntVar(&o.seedPlayers, "seed-players", 0, "players created (with a save each) before the timed load, to grow the DB")
	flag.DurationVar(&think, "think", 0, "pause between operations of one worker (0 = as fast as possible)")
	flag.StringVar(&syncs, "sync-interval", "10s", "litestream sync-interval per run (comma list, cycled)")
	flag.StringVar(&cps, "wal-autocheckpoint", "1000", "database.wal_autocheckpoint per run (comma list, cycled)")
	flag.Parse()
	o.syncIntervals, o.checkpoints = strings.Split(syncs, ","), strings.Split(cps, ",")
	if o.app == "" || o.work == "" {
		fatal("-app and -work are required")
	}
	if o.replica == "s3" && (o.s3Endpoint == "" || os.Getenv("LITESTREAM_ACCESS_KEY_ID") == "" || os.Getenv("LITESTREAM_SECRET_ACCESS_KEY") == "") {
		fatal("-replica s3 needs -s3-endpoint and LITESTREAM_ACCESS_KEY_ID/LITESTREAM_SECRET_ACCESS_KEY")
	}
	if o.replica == "s3" && o.s3CreateBucket {
		if err := createBucket(o); err != nil {
			fatal("create bucket: %v", err)
		}
	}
	var results []*runResult
	ok := true
	for i := 1; i <= o.runs; i++ {
		r, err := drill(o, i)
		if err != nil {
			r.Error = err.Error()
			ok = false
		}
		results = append(results, r)
	}
	out := map[string]any{
		"drill":      "litestream-" + o.replica,
		"generated":  time.Now().UTC().Format(time.RFC3339),
		"litestream": version(o.litestream, "version"),
		"replica":    o.replica,
		"endpoint":   o.s3Endpoint,
		"app":        version(o.app, "version"),
		"host": map[string]any{"os": runtime.GOOS, "arch": runtime.GOARCH, "cpus": runtime.NumCPU(),
			"cpu": version("sysctl", "-n", "machdep.cpu.brand_string"), "mem_bytes": version("sysctl", "-n", "hw.memsize")},
		"load": map[string]any{"workers": o.workers, "think": think.String(), "duration": o.duration.String(), "payload_bytes": o.payloadBytes, "seed_players": o.seedPlayers},
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
	fmt.Fprintf(os.Stderr, "drill: "+f+"\n", a...)
	os.Exit(2)
}

func logf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "[drill %s] "+f+"\n", append([]any{time.Now().Format("15:04:05.000")}, a...)...)
}

func version(bin string, args ...string) string {
	b, _ := exec.Command(bin, args...).CombinedOutput()
	return strings.TrimSpace(string(b))
}

func createBucket(o opts) error {
	req, _ := http.NewRequest(http.MethodPut, strings.TrimRight(o.s3Endpoint, "/")+"/"+o.s3Bucket, nil)
	storage.Signer{AccessKeyID: os.Getenv("LITESTREAM_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("LITESTREAM_SECRET_ACCESS_KEY"),
		Region: o.s3Region, Service: "s3"}.Sign(req, storage.EmptySHA256, time.Now())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 409 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, b)
	}
	return nil
}

// ---- per-run result ----

type latency struct {
	Samples int     `json:"samples"`
	P50Ms   float64 `json:"p50_ms"`
	P95Ms   float64 `json:"p95_ms"`
	MaxMs   float64 `json:"max_ms"`
	Unseen  int     `json:"acked_but_never_seen_in_replica"`
}

type runResult struct {
	Run               int     `json:"run"`
	SyncInterval      string  `json:"sync_interval"`
	WALAutocheckpoint string  `json:"wal_autocheckpoint"`
	LoadSeconds       float64 `json:"load_seconds"`
	Players           int     `json:"players"`
	Acked             struct {
		Saves  int `json:"saves"`
		Claims int `json:"claims"`
		Scores int `json:"scores"`
		Guests int `json:"guests"`
	} `json:"acked"`
	AckedPerSecond   float64 `json:"acked_writes_per_second"`
	LiveDBBytes      int64   `json:"live_db_bytes_at_kill"`
	LiveWALBytes     int64   `json:"live_wal_bytes_at_kill"`
	ReplicaBytes     int64   `json:"replica_bytes,omitempty"`
	ReplicaObjects   int     `json:"replica_objects,omitempty"`
	ReplicationLag   latency `json:"replication_lag_ack_to_follower"`
	RestoreSeconds   float64 `json:"restore_seconds"`
	RestoredDBBytes  int64   `json:"restored_db_bytes"`
	VerifyOK         bool    `json:"verify_ok"`
	Verify           any     `json:"verify,omitempty"`
	AppReadySeconds  float64 `json:"restored_app_ready_seconds"`
	RTOSeconds       float64 `json:"rto_restore_plus_ready_seconds"`
	ReadbackSeconds  float64 `json:"readback_seconds"`
	Lost             lost    `json:"lost"`
	UnackedSurvived  int     `json:"unacknowledged_writes_that_survived"`
	LossWindowMs     int64   `json:"loss_window_ms"`
	NewestSurvivorMs int64   `json:"newest_surviving_ack_before_kill_ms"`
	AckOverlapMs     int64   `json:"surviving_acked_after_oldest_lost_ms"`
	Error            string  `json:"error,omitempty"`
}

type lost struct {
	Saves   int `json:"saves"`
	Claims  int `json:"claims"`
	Scores  int `json:"scores"`
	Players int `json:"players"`
}

// ---- process helpers ----

type proc struct {
	cmd  *exec.Cmd
	done chan struct{} // closed when the process has exited
	err  error
}

func start(logPath string, env []string, bin string, args ...string) (*proc, error) {
	lf, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	c := exec.Command(bin, args...)
	c.Stdout, c.Stderr = lf, lf
	c.Env = append(os.Environ(), env...)
	if err := c.Start(); err != nil {
		lf.Close()
		return nil, err
	}
	p := &proc{cmd: c, done: make(chan struct{})}
	go func() { p.err = c.Wait(); lf.Close(); close(p.done) }()
	return p, nil
}

func (p *proc) kill() {
	if p != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGKILL)
		<-p.done
	}
}

func (p *proc) term() {
	if p == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		p.kill()
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
	b, err := c.CombinedOutput()
	if err != nil {
		return string(b), fmt.Errorf("%s %s: %v: %s", filepath.Base(bin), strings.Join(args, " "), err, b)
	}
	return string(b), nil
}

func waitReady(base string, p *proc, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			return fmt.Errorf("process exited: %v", p.err)
		default:
		}
		resp, err := http.Get(base + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("not ready in time")
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// ---- configuration files ----

const gameID = "drill"

const gameConfig = `{
  "client": {"drill": true},
  "boards": {"main": {"order": "desc", "mode": "best", "period": "none", "min_score": 0, "max_score": 1000000000}},
  "rewards": {"tick": {"grants": {"coin": 1}, "period": "daily", "per_period": 1000}}
}`

func appConfig(dbPath, analytics string, port int, cp string) string {
	return fmt.Sprintf(`profile: lite
environment: development
http:
  listen: 127.0.0.1:%d
  max_body_bytes: 1048576
  request_timeout: 10s
database:
  driver: sqlite
  path: %s
  analytics_path: %s
  read_connections: 2
  write_connections: 1
  synchronous: FULL
  busy_timeout: 5s
  wal_autocheckpoint: %s
cache:
  driver: memory
  max_bytes: 33554432
queue:
  driver: sqlite
  workers: 2
backup:
  enabled: true
  driver: litestream
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
`, port, dbPath, analytics, cp)
}

func litestreamConfig(o opts, dbPath, replicaDir, s3Path, syncInterval string) string {
	if o.replica == "file" {
		return fmt.Sprintf(`dbs:
  - path: %s
    replica:
      type: file
      path: %s
      sync-interval: %s
`, dbPath, replicaDir, syncInterval)
	}
	// Same shape as deploy/litestream/litestream.yml.tmpl, pointed at the drill endpoint.
	return fmt.Sprintf(`snapshot:
  interval: 24h
  retention: 168h
dbs:
  - path: %s
    replica:
      type: s3
      bucket: %s
      path: %s
      endpoint: %s
      region: %s
      force-path-style: true
      sync-interval: %s
`, dbPath, o.s3Bucket, s3Path, o.s3Endpoint, o.s3Region, syncInterval)
}

// ---- HTTP client ----

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

func (c client) newGuest() (guest, error) {
	code, b, err := c.do("POST", "/auth/guest", "", map[string]any{}, nil)
	if err != nil || code != 200 && code != 201 {
		return guest{}, fmt.Errorf("guest: %d %s %v", code, b, err)
	}
	var g guest
	return g, json.Unmarshal(b, &g)
}

func (c client) login(g guest) (string, int, error) {
	code, b, err := c.do("POST", "/auth/guest", "", map[string]any{"guest_id": g.GuestID, "guest_secret": g.GuestSecret}, nil)
	if err != nil || code != 200 && code != 201 {
		return "", code, fmt.Errorf("login: %d %s %v", code, b, err)
	}
	var out guest
	return out.AccessToken, code, json.Unmarshal(b, &out)
}

// ---- acknowledgement log ----

type ack struct {
	at    time.Time
	value int64 // save revision, wallet balance after claim, or score
}

type player struct {
	g      guest
	ackAt  time.Time
	saves  []ack
	claims []ack
	scores []ack
}

type ledger struct {
	mu      sync.Mutex
	players []*player
}

func (l *ledger) add(p *player) {
	l.mu.Lock()
	l.players = append(l.players, p)
	l.mu.Unlock()
}

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

// worker plays: new guest, then cycles save → claim → score; a new guest every 60 ops.
func worker(c client, l *ledger, payloadBytes int, stop <-chan struct{}, mu *sync.Mutex) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		g, err := c.newGuest()
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		p := &player{g: g, ackAt: time.Now()}
		l.add(p)
		rev, score := int64(0), int64(0)
		for i := 0; i < 60; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if think > 0 {
				time.Sleep(think)
			}
			switch i % 3 {
			case 0:
				body := map[string]any{"expected_revision": rev, "schema_version": 1,
					"payload": map[string]any{"seq": rev + 1, "blob": randHex(payloadBytes)}}
				code, b, err := c.do("PUT", "/saves/main", g.AccessToken, body, map[string]string{"Idempotency-Key": randHex(16)})
				if err != nil || code != 200 {
					continue
				}
				var out struct{ Revision int64 }
				if json.Unmarshal(b, &out) == nil {
					now := time.Now()
					mu.Lock()
					rev = out.Revision
					p.saves = append(p.saves, ack{now, rev})
					mu.Unlock()
				}
			case 1:
				code, b, err := c.do("POST", "/rewards/tick/claim", g.AccessToken, map[string]any{}, map[string]string{"Idempotency-Key": randHex(16)})
				if err != nil || code != 200 {
					continue
				}
				var out struct{ Balances map[string]int64 }
				if json.Unmarshal(b, &out) == nil {
					now := time.Now()
					mu.Lock()
					p.claims = append(p.claims, ack{now, out.Balances["coin"]})
					mu.Unlock()
				}
			case 2:
				score += 10
				code, _, err := c.do("POST", "/boards/main/scores", g.AccessToken, map[string]any{"score": score}, map[string]string{"Idempotency-Key": randHex(16)})
				if err != nil || code != 200 {
					continue
				}
				now := time.Now()
				mu.Lock()
				p.scores = append(p.scores, ack{now, score})
				mu.Unlock()
			}
		}
	}
}

// ---- lag probe: a dedicated player saves slot "probe" every 250ms; a
// follower restore (litestream restore -f) is polled for the revision. The
// measured lag is an upper bound: it includes the follower poll interval
// (100ms) and this poller's interval (50ms). ----

type probe struct {
	mu       sync.Mutex
	playerID string
	acks     map[int64]time.Time
	seen     map[int64]time.Time
}

func (pr *probe) write(c client, stop <-chan struct{}) {
	g, err := c.newGuest()
	if err != nil {
		logf("probe guest: %v", err)
		return
	}
	pr.mu.Lock()
	pr.playerID = g.PlayerID
	pr.mu.Unlock()
	rev := int64(0)
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		code, b, err := c.do("PUT", "/saves/probe", g.AccessToken, map[string]any{"expected_revision": rev, "schema_version": 1, "payload": map[string]any{"r": rev + 1}}, nil)
		if err != nil || code != 200 {
			continue
		}
		var out struct{ Revision int64 }
		if json.Unmarshal(b, &out) == nil {
			now := time.Now()
			rev = out.Revision
			pr.mu.Lock()
			pr.acks[rev] = now
			pr.mu.Unlock()
		}
	}
}

func (pr *probe) poll(followDB string, stop <-chan struct{}) {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		pr.mu.Lock()
		pid := pr.playerID
		pr.mu.Unlock()
		if pid == "" || fileSize(followDB) == 0 {
			continue
		}
		db, err := sql.Open("sqlite", "file:"+followDB+"?mode=ro&_pragma=busy_timeout(100)")
		if err != nil {
			continue
		}
		var r int64
		err = db.QueryRow(`SELECT revision FROM save_slots WHERE game_id=? AND player_id=? AND slot='probe'`, gameID, pid).Scan(&r)
		db.Close()
		if err != nil {
			continue
		}
		now := time.Now()
		pr.mu.Lock()
		for rev := range pr.acks {
			if rev <= r {
				if _, ok := pr.seen[rev]; !ok {
					pr.seen[rev] = now
				}
			}
		}
		pr.mu.Unlock()
	}
}

func (pr *probe) stats(killAt time.Time, syncInterval time.Duration) latency {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	var ds []float64
	unseen := 0
	for rev, at := range pr.acks {
		if s, ok := pr.seen[rev]; ok {
			ds = append(ds, float64(s.Sub(at).Microseconds())/1000)
		} else if killAt.Sub(at) > 3*syncInterval+2*time.Second {
			unseen++ // old enough that it should have been replicated
		}
	}
	sort.Float64s(ds)
	l := latency{Samples: len(ds), Unseen: unseen}
	if len(ds) > 0 {
		q := func(p float64) float64 { return ds[int(p*float64(len(ds)-1)+0.5)] }
		l.P50Ms, l.P95Ms, l.MaxMs = q(0.5), q(0.95), ds[len(ds)-1]
	}
	return l
}

// ---- the drill ----

func drill(o opts, n int) (res *runResult, err error) {
	res = &runResult{Run: n, SyncInterval: o.syncIntervals[(n-1)%len(o.syncIntervals)], WALAutocheckpoint: o.checkpoints[(n-1)%len(o.checkpoints)]}
	syncInterval, err := time.ParseDuration(res.SyncInterval)
	if err != nil {
		return res, err
	}
	dir, _ := filepath.Abs(filepath.Join(o.work, fmt.Sprintf("run-%d", n)))
	live := filepath.Join(dir, "live")
	blank := filepath.Join(dir, "blank") // the "new machine": only config + binaries
	for _, d := range []string{live, filepath.Join(blank, "restore"), filepath.Join(dir, "follow")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return res, err
		}
	}
	liveDB := filepath.Join(live, "app.db")
	port := freePort()
	cfgPath := filepath.Join(live, "gostack.yaml")
	if err := os.WriteFile(cfgPath, []byte(appConfig(liveDB, filepath.Join(live, "analytics.db"), port, res.WALAutocheckpoint)), 0o600); err != nil {
		return res, err
	}
	ltCfg := filepath.Join(dir, "litestream.yml")
	s3Path := fmt.Sprintf("drill/%s/run-%d", time.Now().UTC().Format("20060102T150405"), n)
	replicaDir := filepath.Join(dir, "replica")
	if err := os.WriteFile(ltCfg, []byte(litestreamConfig(o, liveDB, replicaDir, s3Path, res.SyncInterval)), 0o600); err != nil {
		return res, err
	}
	gcfg := filepath.Join(dir, "game-config.json")
	if err := os.WriteFile(gcfg, []byte(gameConfig), 0o600); err != nil {
		return res, err
	}
	for _, args := range [][]string{{"migrate", "up"}, {"game", "create", gameID, "--name", "Drill"}, {"game", "config", "publish", gameID, gcfg}} {
		if _, err := run(nil, o.app, append([]string{"--config", cfgPath}, args...)...); err != nil {
			return res, err
		}
	}
	lt, err := start(filepath.Join(dir, "litestream.log"), nil, o.litestream, "replicate", "-config", ltCfg)
	if err != nil {
		return res, err
	}
	defer lt.kill()
	app, err := start(filepath.Join(dir, "app.log"), nil, o.app, "--config", cfgPath, "serve")
	if err != nil {
		return res, err
	}
	defer app.kill()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := waitReady(base, app, 30*time.Second); err != nil {
		return res, fmt.Errorf("app: %w", err)
	}
	c := client{base: base, hc: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 64}}}

	if o.seedPlayers > 0 { // grow the database before the timed phase
		logf("run %d: seeding %d players", n, o.seedPlayers)
		var wg sync.WaitGroup
		ch := make(chan int)
		for w := 0; w < o.workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range ch {
					if g, err := c.newGuest(); err == nil {
						_, _, _ = c.do("PUT", "/saves/seed", g.AccessToken, map[string]any{"expected_revision": 0, "schema_version": 1, "payload": map[string]any{"blob": randHex(o.payloadBytes)}}, nil)
					}
				}
			}()
		}
		for i := 0; i < o.seedPlayers; i++ {
			ch <- i
		}
		close(ch)
		wg.Wait()
		time.Sleep(2*syncInterval + time.Second)
	}

	pr := &probe{acks: map[int64]time.Time{}, seen: map[int64]time.Time{}}
	stop := make(chan struct{})
	followDB := filepath.Join(dir, "follow", "app.db")
	followerDone := make(chan struct{})
	go func() { // (re)start the follower until stopped; it fails until the first snapshot exists
		defer close(followerDone)
		for {
			for _, p := range []string{followDB, followDB + "-wal", followDB + "-shm"} {
				_ = os.Remove(p)
			}
			f, err := start(filepath.Join(dir, "follow.log"), nil, o.litestream, "restore", "-f", "-follow-interval", "100ms", "-config", ltCfg, "-o", followDB, liveDB)
			if err != nil {
				return
			}
			select {
			case <-f.done:

				time.Sleep(500 * time.Millisecond)
			case <-stop:
				f.kill()
				return
			}
		}
	}()
	go pr.write(c, stop)
	go pr.poll(followDB, stop)

	l := &ledger{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	loadStart := time.Now()
	for w := 0; w < o.workers; w++ {
		wg.Add(1)
		go func() { defer wg.Done(); worker(c, l, o.payloadBytes, stop, &mu) }()
	}
	logf("run %d: load for %s (sync-interval %s, wal_autocheckpoint %s, replica %s)", n, o.duration, res.SyncInterval, res.WALAutocheckpoint, o.replica)
	time.Sleep(o.duration)

	// Simulated machine loss: SIGKILL Litestream and the app back to back
	// while the load is still running.
	killAt := time.Now()
	lt.kill()
	app.kill()
	res.LoadSeconds = killAt.Sub(loadStart).Seconds()
	res.LiveDBBytes, res.LiveWALBytes = fileSize(liveDB), fileSize(liveDB+"-wal")
	close(stop)
	wg.Wait()
	<-followerDone
	logf("run %d: killed app and litestream; db=%d wal=%d", n, res.LiveDBBytes, res.LiveWALBytes)

	mu.Lock()
	for _, p := range l.players {
		res.Acked.Saves += len(p.saves)
		res.Acked.Claims += len(p.claims)
		res.Acked.Scores += len(p.scores)
	}
	res.Players = len(l.players)
	res.Acked.Guests = res.Players
	mu.Unlock()
	res.AckedPerSecond = float64(res.Acked.Saves+res.Acked.Claims+res.Acked.Scores+res.Acked.Guests) / res.LoadSeconds
	res.ReplicationLag = pr.stats(killAt, syncInterval)
	if o.replica == "file" {
		_ = filepath.WalkDir(replicaDir, func(_ string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if fi, e := d.Info(); e == nil {
					res.ReplicaBytes += fi.Size()
					res.ReplicaObjects++
				}
			}
			return nil
		})
	}

	// Restore on the "blank" machine into a new path with the shipped script.
	restored := filepath.Join(blank, "restore", "app.db")
	t0 := time.Now()
	out, rerr := run([]string{"PATH=" + filepath.Dir(o.litestream) + ":" + os.Getenv("PATH")}, "bash", o.restoreScript,
		"--bin", o.app, "--replica-config", ltCfg, "--db", liveDB, "--to", restored)
	res.RestoreSeconds = time.Since(t0).Seconds()
	_ = os.WriteFile(filepath.Join(dir, "restore.log"), []byte(out), 0o600)
	if i := strings.Index(out, "\n{"); i >= 0 || strings.HasPrefix(out, "{") {
		var v map[string]any
		if json.NewDecoder(strings.NewReader(out[i+1:])).Decode(&v) == nil {
			res.Verify = v
			res.VerifyOK, _ = v["ok"].(bool)
		}
	}
	if rerr != nil {
		return res, fmt.Errorf("restore: %w", rerr)
	}
	res.RestoredDBBytes = fileSize(restored)

	// Start the app on the restored database (new port, fresh analytics DB).
	port2 := freePort()
	cfg2 := filepath.Join(blank, "gostack.yaml")
	if err := os.WriteFile(cfg2, []byte(appConfig(restored, filepath.Join(blank, "analytics.db"), port2, "1000")), 0o600); err != nil {
		return res, err
	}
	t1 := time.Now()
	app2, err := start(filepath.Join(blank, "app.log"), nil, o.app, "--config", cfg2, "serve")
	if err != nil {
		return res, err
	}
	defer app2.term()
	base2 := fmt.Sprintf("http://127.0.0.1:%d", port2)
	if err := waitReady(base2, app2, 60*time.Second); err != nil {
		return res, fmt.Errorf("restored app: %w", err)
	}
	res.AppReadySeconds = time.Since(t1).Seconds()
	res.RTOSeconds = res.RestoreSeconds + res.AppReadySeconds

	// Read everything back through the HTTP API and compare with the acks.
	t2 := time.Now()
	c2 := client{base: base2, hc: c.hc}
	var oldestLost, newestKept time.Time
	noteLost := func(at time.Time) {
		if oldestLost.IsZero() || at.Before(oldestLost) {
			oldestLost = at
		}
	}
	noteKept := func(at time.Time) {
		if at.After(newestKept) {
			newestKept = at
		}
	}
	var errs []string
	for _, p := range l.players {
		tok, code, err := c2.login(p.g)
		if err != nil {
			if code == 401 {
				res.Lost.Players++
				noteLost(p.ackAt)
				for _, as := range [][]ack{p.saves, p.claims, p.scores} {
					for _, a := range as {
						noteLost(a.at)
					}
				}
				res.Lost.Saves += len(p.saves)
				res.Lost.Claims += len(p.claims)
				res.Lost.Scores += len(p.scores)
				continue
			}
			errs = append(errs, err.Error())
			continue
		}
		noteKept(p.ackAt)
		var rev, coin, best int64
		if code, b, err := c2.do("GET", "/saves/main", tok, nil, nil); err == nil && code == 200 {
			var s struct{ Revision int64 }
			_ = json.Unmarshal(b, &s)
			rev = s.Revision
		} else if code != 404 {
			errs = append(errs, fmt.Sprintf("get save %d %s %v", code, b, err))
		}
		if code, b, err := c2.do("GET", "/wallet", tok, nil, nil); err == nil && code == 200 {
			var w struct{ Balances map[string]int64 }
			_ = json.Unmarshal(b, &w)
			coin = w.Balances["coin"]
		} else {
			errs = append(errs, fmt.Sprintf("wallet %d %s %v", code, b, err))
		}
		if code, b, err := c2.do("GET", "/boards/main?limit=1", tok, nil, nil); err == nil && code == 200 {
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
		cmp := func(acks []ack, have int64, lostN *int) {
			maxAck := int64(0)
			for _, a := range acks {
				if a.value > have {
					*lostN++
					noteLost(a.at)
				} else {
					noteKept(a.at)
				}
				maxAck = max(maxAck, a.value)
			}
			if have > maxAck {
				res.UnackedSurvived++ // committed, but the response never reached the client
			}
		}
		cmp(p.saves, rev, &res.Lost.Saves)
		cmp(p.claims, coin, &res.Lost.Claims)
		cmp(p.scores, best, &res.Lost.Scores)
	}
	res.ReadbackSeconds = time.Since(t2).Seconds()
	if !oldestLost.IsZero() {
		res.LossWindowMs = killAt.Sub(oldestLost).Milliseconds()
	}
	if !newestKept.IsZero() {
		res.NewestSurvivorMs = killAt.Sub(newestKept).Milliseconds()
	}
	if !oldestLost.IsZero() && newestKept.After(oldestLost) {
		// Litestream restores a transaction prefix, so every surviving write
		// should be older than every lost one; acks are timestamped by the
		// client after the response, so a few ms of overlap is expected.
		res.AckOverlapMs = newestKept.Sub(oldestLost).Milliseconds()
	}
	if len(errs) > 0 {
		return res, fmt.Errorf("%d readback errors, first: %s", len(errs), errs[0])
	}
	if !res.VerifyOK {
		return res, errors.New("backup verify did not report ok")
	}
	logf("run %d: restore %.2fs ready %.2fs lost=%+v window=%dms", n, res.RestoreSeconds, res.AppReadySeconds, res.Lost, res.LossWindowMs)
	return res, nil
}
