// Package fault holds process-level fault-injection tests.
//
// Scope note: SIGKILL loses the process but not the OS page cache, so this
// verifies crash consistency for process failure. Power-loss durability of
// synchronous=FULL depends on the disk honouring fsync and must be verified on
// the target machine (PRD A04 "本地故障模型").
package fault

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func runCLI(t *testing.T, bin, cfg string, args ...string) {
	t.Helper()
	out, err := exec.Command(bin, append([]string{"--config", cfg}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

func TestSIGKILLPreservesAcknowledgedWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and kills a real server")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "example")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/example").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	addr := freePort(t)
	cfg := filepath.Join(dir, "gostack.yaml")
	os.WriteFile(cfg, []byte(fmt.Sprintf("http:\n  listen: %s\nkits: [game]\ngame:\n  auth_rate_per_minute: 100000\n  score_rate_per_minute: 100000\n", addr)), 0o600)
	runCLI(t, bin, cfg, "migrate", "up")
	runCLI(t, bin, cfg, "game", "create", "g1")
	doc := filepath.Join(dir, "doc.json")
	os.WriteFile(doc, []byte(`{"rewards":{"r":{"grants":{"coin":1},"period":"once","per_period":1000}}}`), 0o600)
	runCLI(t, bin, cfg, "game", "config", "publish", "g1", doc)

	srv := exec.Command(bin, "--config", cfg, "serve")
	srv.Stderr = io.Discard
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	base := "http://" + addr
	deadline := time.Now().Add(10 * time.Second)
	for {
		if r, err := http.Get(base + "/readyz"); err == nil && r.StatusCode == 200 {
			r.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			srv.Process.Kill()
			t.Fatal("server not ready")
		}
		time.Sleep(50 * time.Millisecond)
	}

	type ack struct {
		token, player string
		save          int64 // highest acknowledged save revision
		claims        int64 // acknowledged reward claims
	}
	client := &http.Client{Timeout: 5 * time.Second}
	call := func(method, path, token, body string) (int, map[string]any) {
		req, _ := http.NewRequest(method, base+path, bytes.NewBufferString(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := client.Do(req)
		if err != nil {
			return 0, nil
		}
		defer res.Body.Close()
		var m map[string]any
		json.NewDecoder(res.Body).Decode(&m)
		return res.StatusCode, m
	}
	const players = 8
	acks := make([]*ack, players)
	for i := range acks {
		st, m := call("POST", "/v1/games/g1/auth/guest", "", "{}")
		if st != 201 {
			t.Fatalf("guest %d", st)
		}
		acks[i] = &ack{token: m["access_token"].(string), player: m["player_id"].(string)}
	}
	var stop atomic.Bool
	var wg sync.WaitGroup
	var total atomic.Int64
	for _, a := range acks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				st, m := call("PUT", "/v1/games/g1/saves/main", a.token, fmt.Sprintf(`{"expected_revision":%d,"payload":{"n":%d}}`, a.save, a.save+1))
				if st == 200 {
					a.save = int64(m["revision"].(float64))
					total.Add(1)
				} else if st == 0 {
					return // server gone
				}
				st, _ = call("POST", "/v1/games/g1/rewards/r/claim", a.token, "{}")
				if st == 200 {
					a.claims++
					total.Add(1)
				} else if st == 0 {
					return
				}
			}
		}()
	}
	for total.Load() < 400 {
		time.Sleep(5 * time.Millisecond)
	}
	if err := srv.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	srv.Wait()
	stop.Store(true)
	wg.Wait()
	t.Logf("acknowledged writes before SIGKILL: %d", total.Load())

	// Recovery: the instance lock must be released by the kernel, the database
	// must pass integrity_check and contain every acknowledged write.
	runCLI(t, bin, cfg, "doctor")
	runCLI(t, bin, cfg, "game", "reconcile", "g1")
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "data", "app.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, a := range acks {
		var rev, coins int64
		db.QueryRowContext(ctx, `SELECT coalesce(max(revision),0) FROM save_slots WHERE game_id='g1' AND player_id=?`, a.player).Scan(&rev)
		db.QueryRowContext(ctx, `SELECT coalesce(sum(balance),0) FROM wallet_balances WHERE game_id='g1' AND player_id=?`, a.player).Scan(&coins)
		// A write may commit after the client lost the response, so ">=" is correct; "<" is data loss.
		if rev < a.save || coins < a.claims {
			t.Fatalf("player %s lost acknowledged data: save %d < %d or coins %d < %d", a.player, rev, a.save, coins, a.claims)
		}
		if rev > a.save+1 || coins > a.claims+1 {
			t.Fatalf("player %s has more unacknowledged writes than in-flight requests allow", a.player)
		}
	}
	// The server must restart cleanly on the same files.
	srv2 := exec.Command(bin, "--config", cfg, "serve")
	srv2.Stderr = io.Discard
	if err := srv2.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { srv2.Process.Signal(syscall.SIGTERM); srv2.Wait() }()
	deadline = time.Now().Add(10 * time.Second)
	for {
		if r, err := http.Get(base + "/readyz"); err == nil && r.StatusCode == 200 {
			r.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not restart after crash")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
