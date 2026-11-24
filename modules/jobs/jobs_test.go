package jobs_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pillar/gostack/adapters/pgtest"
	"github.com/pillar/gostack/adapters/sqlite"
	"github.com/pillar/gostack/modules/jobs"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newQueue(t *testing.T) (*jobs.Queue, *clock) {
	t.Helper()
	o := sqlite.Options{Path: filepath.Join(t.TempDir(), "app.db"), ReadConnections: 2, Synchronous: "FULL",
		BusyTimeout: time.Second, WALAutocheckpoint: 1000, WriteQueue: 64}
	db := pgtest.Open(t, o)
	c := &clock{t: time.UnixMilli(1_700_000_000_000)}
	return &jobs.Queue{DB: db, Now: c.Now}, c
}

// A06: a crashed worker's lease is reclaimed and its stale token can no longer Ack.
func TestLeaseReclaimRejectsStaleToken(t *testing.T) {
	q, clk := newQueue(t)
	ctx := context.Background()
	id, err := q.EnqueueNow(ctx, jobs.Job{Type: "email", Payload: []byte(`{"to":"x"}`), MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	first, err := q.Claim(ctx, nil, 10*time.Second)
	if err != nil || first == nil || first.ID != id {
		t.Fatalf("claim: %+v %v", first, err)
	}
	if again, _ := q.Claim(ctx, nil, 10*time.Second); again != nil {
		t.Fatal("job claimed twice while leased")
	}
	clk.Add(11 * time.Second) // worker 1 "crashed"
	second, err := q.Claim(ctx, nil, 10*time.Second)
	if err != nil || second == nil || second.Attempt != 2 {
		t.Fatalf("reclaim: %+v %v", second, err)
	}
	if err := q.Ack(ctx, first); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("stale ack err=%v", err)
	}
	if err := q.Ack(ctx, second); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := q.Status(ctx, id); st != "succeeded" {
		t.Fatalf("status=%s", st)
	}
}

func TestRetryThenDeadAndReplay(t *testing.T) {
	q, clk := newQueue(t)
	ctx := context.Background()
	id, _ := q.EnqueueNow(ctx, jobs.Job{Type: "t", MaxAttempts: 2})
	for i := 0; i < 2; i++ {
		c, err := q.Claim(ctx, []string{"t"}, time.Minute)
		if err != nil || c == nil {
			t.Fatalf("attempt %d claim: %v", i, err)
		}
		if err := q.Nack(ctx, c, errors.New("boom")); err != nil {
			t.Fatal(err)
		}
		clk.Add(2 * time.Hour)
	}
	if st, n, _ := q.Status(ctx, id); st != "dead" || n != 2 {
		t.Fatalf("status=%s attempts=%d", st, n)
	}
	if c, _ := q.Claim(ctx, nil, time.Minute); c != nil {
		t.Fatal("dead job claimed")
	}
	if err := q.Replay(ctx, id); err != nil {
		t.Fatal(err)
	}
	if c, _ := q.Claim(ctx, nil, time.Minute); c == nil || c.ID != id {
		t.Fatal("replayed job not claimable")
	}
}

func TestLeaseExpiredOnFinalAttemptGoesDead(t *testing.T) {
	q, clk := newQueue(t)
	ctx := context.Background()
	id, _ := q.EnqueueNow(ctx, jobs.Job{Type: "t", MaxAttempts: 1})
	if c, _ := q.Claim(ctx, nil, time.Second); c == nil {
		t.Fatal("no claim")
	}
	clk.Add(2 * time.Second)
	if c, _ := q.Claim(ctx, nil, time.Second); c != nil {
		t.Fatal("job past max attempts re-claimed")
	}
	if st, _, _ := q.Status(ctx, id); st != "dead" {
		t.Fatalf("status=%s", st)
	}
}

func TestDedupeAndPayloadLimit(t *testing.T) {
	q, _ := newQueue(t)
	ctx := context.Background()
	a, _ := q.EnqueueNow(ctx, jobs.Job{Type: "season_settle", DedupeKey: "2026-W47"})
	b, _ := q.EnqueueNow(ctx, jobs.Job{Type: "season_settle", DedupeKey: "2026-W47"})
	if a != b {
		t.Fatalf("dedupe produced two jobs %s %s", a, b)
	}
	if _, err := q.EnqueueNow(ctx, jobs.Job{Type: "big", Payload: make([]byte, 70<<10)}); err == nil {
		t.Fatal("oversized payload accepted")
	}
}

func TestConcurrentClaimsAreExclusive(t *testing.T) {
	q, _ := newQueue(t)
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		q.EnqueueNow(ctx, jobs.Job{Type: "t"})
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				c, err := q.Claim(ctx, nil, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if c == nil {
					return
				}
				mu.Lock()
				seen[c.ID]++
				mu.Unlock()
				if err := q.Ack(ctx, c); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if len(seen) != 50 {
		t.Fatalf("processed %d distinct jobs", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("job %s claimed %d times", id, n)
		}
	}
}
