package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pillar/gostack/adapters/pgtest"
	"github.com/pillar/gostack/adapters/sqlite"
	"github.com/pillar/gostack/core/auth"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/kits/agent"
	"github.com/pillar/gostack/modules/jobs"
	"github.com/pillar/gostack/modules/storage"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

const lease = 30 * time.Second

type env struct {
	k   *agent.Kit
	q   *jobs.Queue
	clk *clock
}

func newEnv(t *testing.T) *env {
	t.Helper()
	o := sqlite.Options{Path: filepath.Join(t.TempDir(), "app.db"), ReadConnections: 4, Synchronous: "FULL",
		BusyTimeout: time.Second, WALAutocheckpoint: 1000, WriteQueue: 256}
	db := pgtest.Open(t, o)
	clk := &clock{t: time.UnixMilli(1_700_000_000_000)}
	q := &jobs.Queue{DB: db, Now: clk.Now}
	cfg := config.Defaults().Agent
	cfg.CancelPoll = 10 * time.Millisecond
	cfg.StepTimeout = 5 * time.Second
	k := &agent.Kit{DB: db, Jobs: q, Cfg: cfg, Lease: time.Hour, Now: clk.Now, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return &env{k: k, q: q, clk: clk}
}

// claim leases the next runnable agent job (nil when none).
func (e *env) claim(t *testing.T) *jobs.Claimed {
	t.Helper()
	c, err := e.q.Claim(context.Background(), []string{agent.JobType}, lease)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// handle runs a claimed job like jobs.Worker does.
func (e *env) handle(t *testing.T, c *jobs.Claimed) error {
	t.Helper()
	ctx := context.Background()
	err := e.k.HandleJob(ctx, c)
	if err == nil {
		_ = e.q.Ack(ctx, c)
	} else {
		_ = e.q.Nack(ctx, c, err)
	}
	return err
}

// drain processes runnable jobs until none is left.
func (e *env) drain(t *testing.T) {
	t.Helper()
	for i := 0; i < 50; i++ {
		c := e.claim(t)
		if c == nil {
			return
		}
		if err := e.handle(t, c); err != nil {
			t.Fatalf("job failed: %v", err)
		}
	}
	t.Fatal("jobs did not drain")
}

func (e *env) create(t *testing.T, tenant, wf string, req agent.CreateRequest) agent.RunInfo {
	t.Helper()
	req.Workflow = wf
	r, err := e.k.Create(context.Background(), tenant, req)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) get(t *testing.T, tenant, id string) agent.RunInfo {
	t.Helper()
	r, err := e.k.Get(context.Background(), tenant, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func out(b string) func(context.Context) (agent.Output, error) {
	return func(context.Context) (agent.Output, error) { return agent.Output{Data: []byte(b), CostUnits: 1}, nil }
}

func TestRunSucceedsWithStepsAndAudit(t *testing.T) {
	e := newEnv(t)
	var gotKey string
	must(t, e.k.Register("summarize", agent.Workflow{Func: func(ctx context.Context, r *agent.Run) error {
		var in struct{ Text string }
		if err := r.Decode(&in); err != nil {
			return err
		}
		plan, err := r.Step(ctx, "plan", out("plan:"+in.Text))
		if err != nil {
			return err
		}
		sent, err := r.Tool(ctx, "notify", agent.ToolCall{Tool: "mailer", Request: plan, Idempotent: true},
			func(ctx context.Context, key string) (agent.Output, error) {
				gotKey = key
				return agent.Output{Data: []byte("ok"), CostUnits: 2, ExternalRef: "msg-1"}, nil
			})
		if err != nil {
			return err
		}
		return r.SetOutput(map[string]string{"plan": string(plan), "sent": string(sent)})
	}}))
	run := e.create(t, "t1", "summarize", agent.CreateRequest{Input: json.RawMessage(`{"Text":"hi"}`)})
	if run.Status != agent.StatusQueued {
		t.Fatalf("status %s", run.Status)
	}
	e.drain(t)
	got := e.get(t, "t1", run.ID)
	if got.Status != agent.StatusSucceeded || got.StepsUsed != 2 || got.CostUnits != 3 || got.FinishedAt == nil {
		t.Fatalf("run %+v", got)
	}
	if string(got.Output) != `{"plan":"plan:hi","sent":"ok"}` {
		t.Fatalf("output %s", got.Output)
	}
	if len(got.Steps) != 2 || got.Steps[0].Kind != agent.KindLLM || got.Steps[1].Kind != agent.KindTool || got.Steps[1].Tool != "mailer" {
		t.Fatalf("steps %+v", got.Steps)
	}
	calls, err := e.k.Audit(context.Background(), "t1", run.ID)
	must(t, err)
	if len(calls) != 2 || calls[0].Outcome != "started" || calls[1].Outcome != "succeeded" || calls[1].ExternalRef != "msg-1" ||
		calls[0].IdempotencyKey != gotKey || calls[0].RequestHash == "" {
		t.Fatalf("audit %+v key %s", calls, gotKey)
	}
}

// blocker lets a test stop a step function mid-flight (simulating a worker
// that hangs or dies during the step) and release it later.
type blocker struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlocker() *blocker {
	return &blocker{started: make(chan struct{}), release: make(chan struct{})}
}

func (b *blocker) block() {
	b.once.Do(func() { close(b.started) })
	<-b.release
}

// A worker dies in the middle of step 2: after its lease expires another
// worker resumes from the checkpoints. Step 1 is not executed again, step 2
// is re-executed (attempt 2), and the stale worker is fenced off.
func TestCrashResumesFromCheckpoint(t *testing.T) {
	e := newEnv(t)
	var a, b, c atomic.Int32
	bl := newBlocker()
	must(t, e.k.Register("wf", agent.Workflow{Func: func(ctx context.Context, r *agent.Run) error {
		if _, err := r.Step(ctx, "a", func(context.Context) (agent.Output, error) {
			a.Add(1)
			return agent.Output{Data: []byte("A")}, nil
		}); err != nil {
			return err
		}
		x, err := r.Step(ctx, "b", func(context.Context) (agent.Output, error) {
			if b.Add(1) == 1 {
				bl.block() // first worker hangs here
			}
			return agent.Output{Data: []byte("B")}, nil
		})
		if err != nil {
			return err
		}
		_, err = r.Step(ctx, "c", func(context.Context) (agent.Output, error) {
			c.Add(1)
			return agent.Output{Data: append([]byte("C"), x...)}, nil
		})
		return err
	}}))
	run := e.create(t, "t1", "wf", agent.CreateRequest{})
	first := e.claim(t)
	done := make(chan error, 1)
	go func() { done <- e.k.HandleJob(context.Background(), first) }()
	<-bl.started

	e.clk.Add(lease + time.Second) // lease expires; another worker claims the job
	second := e.claim(t)
	if second == nil || second.ID != first.ID || second.Attempt != 2 {
		t.Fatalf("re-claim %+v", second)
	}
	must(t, e.handle(t, second))
	got := e.get(t, "t1", run.ID)
	if got.Status != agent.StatusSucceeded {
		t.Fatalf("status %s %s", got.Status, got.Error)
	}
	if a.Load() != 1 || b.Load() != 2 || c.Load() != 1 {
		t.Fatalf("executions a=%d b=%d c=%d", a.Load(), b.Load(), c.Load())
	}
	if got.Steps[1].Attempt != 2 || got.StepsUsed != 3 {
		t.Fatalf("steps %+v used %d", got.Steps, got.StepsUsed)
	}
	close(bl.release) // the stale worker wakes up: its writes are refused
	if err := <-done; err != nil {
		t.Fatalf("stale worker: %v", err)
	}
	if err := e.q.Ack(context.Background(), first); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatalf("stale ack: %v", err)
	}
	if again := e.get(t, "t1", run.ID); again.Status != agent.StatusSucceeded || again.UpdatedAt != got.UpdatedAt {
		t.Fatalf("stale worker changed the run: %+v", again)
	}
}

// A worker dies during a non-idempotent tool call: the outcome is unknown, so
// the step is parked as needs_reconciliation instead of being re-sent. An
// operator resolution resumes the run without calling the tool again.
func TestUnknownToolOutcomeNeedsReconciliation(t *testing.T) {
	e := newEnv(t)
	var calls, after atomic.Int32
	bl := newBlocker()
	must(t, e.k.Register("pay", agent.Workflow{Func: func(ctx context.Context, r *agent.Run) error {
		res, err := r.Tool(ctx, "charge", agent.ToolCall{Tool: "psp", Request: []byte(`{"amount":5}`)},
			func(ctx context.Context, key string) (agent.Output, error) {
				if calls.Add(1) == 1 {
					bl.block()
				}
				return agent.Output{Data: []byte("charged")}, nil
			})
		if err != nil {
			return err
		}
		_, err = r.Step(ctx, "receipt", func(context.Context) (agent.Output, error) {
			after.Add(1)
			return agent.Output{Data: res}, nil
		})
		return err
	}}))
	run := e.create(t, "t1", "pay", agent.CreateRequest{})
	first := e.claim(t)
	done := make(chan error, 1)
	go func() { done <- e.k.HandleJob(context.Background(), first) }()
	<-bl.started
	e.clk.Add(lease + time.Second)
	must(t, e.handle(t, e.claim(t)))

	got := e.get(t, "t1", run.ID)
	if got.Status != agent.StatusWaiting || got.WaitReason != "reconciliation" || got.Steps[0].Status != agent.StepNeedsReconciliation {
		t.Fatalf("run %+v", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("tool called %d times; must not be re-sent blindly", calls.Load())
	}
	audit, _ := e.k.Audit(context.Background(), "t1", run.ID)
	if len(audit) != 2 || audit[0].Outcome != "started" || audit[1].Outcome != "unknown" {
		t.Fatalf("audit %+v", audit)
	}
	close(bl.release)
	<-done
	if e.claim(t) != nil {
		t.Fatal("a parked run must not have a runnable job")
	}
	// Bad resolutions.
	if _, err := e.k.Resolve(context.Background(), "t1", run.ID, 0, "maybe", nil, "", "x"); code(err) != "invalid_request" {
		t.Fatalf("bad outcome: %v", err)
	}
	if _, err := e.k.Resolve(context.Background(), "t2", run.ID, 0, "succeeded", nil, "", "x"); code(err) != "not_found" {
		t.Fatalf("other tenant: %v", err)
	}
	res, err := e.k.Resolve(context.Background(), "t1", run.ID, 0, agent.StepSucceeded, []byte("charged-by-psp"), "psp-123", "verified in PSP dashboard")
	must(t, err)
	if res.Status != agent.StatusQueued {
		t.Fatalf("after resolve %s", res.Status)
	}
	if _, err := e.k.Resolve(context.Background(), "t1", run.ID, 0, agent.StepSucceeded, nil, "", "again"); code(err) != "invalid_state" {
		t.Fatalf("double resolve: %v", err)
	}
	e.drain(t)
	got = e.get(t, "t1", run.ID)
	if got.Status != agent.StatusSucceeded || calls.Load() != 1 || after.Load() != 1 {
		t.Fatalf("run %s calls %d after %d", got.Status, calls.Load(), after.Load())
	}
	audit, _ = e.k.Audit(context.Background(), "t1", run.ID)
	if audit[len(audit)-1].Outcome != "resolved_succeeded" || audit[len(audit)-1].ExternalRef != "psp-123" {
		t.Fatalf("audit %+v", audit)
	}
}

// An idempotent tool interrupted by a crash is re-sent with the same key.
func TestIdempotentToolRetriedWithSameKey(t *testing.T) {
	e := newEnv(t)
	var keys []string
	var mu sync.Mutex
	bl := newBlocker()
	must(t, e.k.Register("wf", agent.Workflow{Func: func(ctx context.Context, r *agent.Run) error {
		_, err := r.Tool(ctx, "put", agent.ToolCall{Tool: "kv", Request: []byte("x"), Idempotent: true},
			func(ctx context.Context, key string) (agent.Output, error) {
				mu.Lock()
				keys = append(keys, key)
				n := len(keys)
				mu.Unlock()
				if n == 1 {
					bl.block()
				}
				return agent.Output{ExternalRef: "v1"}, nil
			})
		return err
	}}))
	run := e.create(t, "t1", "wf", agent.CreateRequest{})
	first := e.claim(t)
	go func() { _ = e.k.HandleJob(context.Background(), first) }()
	<-bl.started
	e.clk.Add(lease + time.Second)
	must(t, e.handle(t, e.claim(t)))
	close(bl.release)
	if got := e.get(t, "t1", run.ID); got.Status != agent.StatusSucceeded {
		t.Fatalf("status %s", got.Status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 || keys[0] != keys[1] {
		t.Fatalf("keys %v", keys)
	}
	audit, _ := e.k.Audit(context.Background(), "t1", run.ID)
	var outcomes []string
	for _, a := range audit {
		outcomes = append(outcomes, a.Outcome)
	}
	if strings.Join(outcomes, ",") != "started,retry,started,succeeded" {
		t.Fatalf("audit outcomes %v", outcomes)
	}
}

func TestCancel(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	started := make(chan struct{}, 1)
	var second atomic.Int32
	must(t, e.k.Register("wf", agent.Workflow{Func: func(ctx context.Context, r *agent.Run) error {
		_, err := r.Step(ctx, "long", func(ctx context.Context) (agent.Output, error) {
			started <- struct{}{}
			<-ctx.Done() // cooperative: the step observes the cancelled context
			return agent.Output{}, ctx.Err()
		})
		if err != nil {
			return err
		}
		second.Add(1)
		return nil
	}}))
	// A queued run is cancelled at once and never executes.
	q := e.create(t, "t1", "wf", agent.CreateRequest{})
	got, err := e.k.Cancel(ctx, "t1", q.ID)
	must(t, err)
	if got.Status != agent.StatusCancelled || got.ErrorCode != agent.CodeCancelled {
		t.Fatalf("queued cancel %+v", got)
	}
	e.drain(t)
	if len(e.get(t, "t1", q.ID).Steps) != 0 {
		t.Fatal("cancelled run executed")
	}
	// Cancelling again is a no-op.
	if _, err := e.k.Cancel(ctx, "t1", q.ID); err != nil {
		t.Fatal(err)
	}
	// A running run stops during its step.
	run := e.create(t, "t1", "wf", agent.CreateRequest{})
	c := e.claim(t)
	done := make(chan error, 1)
	go func() { done <- e.handle(t, c) }()
	<-started
	got, err = e.k.Cancel(ctx, "t1", run.ID)
	must(t, err)
	if got.Status != agent.StatusRunning || !got.CancelReq {
		t.Fatalf("running cancel %+v", got)
	}
	must(t, <-done)
	got = e.get(t, "t1", run.ID)
	if got.Status != agent.StatusCancelled || got.Steps[0].Status != agent.StepFailed || second.Load() != 0 {
		t.Fatalf("after cancel %+v", got)
	}
	// Finished runs cannot be cancelled.
	must(t, e.k.Register("quick", agent.Workflow{Func: func(context.Context, *agent.Run) error { return nil }}))
	fin := e.create(t, "t1", "quick", agent.CreateRequest{})
	e.drain(t)
	if _, err := e.k.Cancel(ctx, "t1", fin.ID); code(err) != "invalid_state" {
		t.Fatalf("cancel finished: %v", err)
	}
}

func TestTimeouts(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	must(t, e.k.Register("wf", agent.Workflow{Func: func(ctx context.Context, r *agent.Run) error {
		_, err := r.Step(ctx, "slow", func(ctx context.Context) (agent.Output, error) {
			<-ctx.Done()
			return agent.Output{}, ctx.Err()
		})
		return err
	}}))
	// Deadline passed while queued.
	q := e.create(t, "t1", "wf", agent.CreateRequest{TimeoutSeconds: 10})
	e.clk.Add(11 * time.Second)
	e.drain(t)
	if got := e.get(t, "t1", q.ID); got.Status != agent.StatusTimedOut || len(got.Steps) != 0 {
		t.Fatalf("queued timeout %+v", got)
	}
	// Deadline reached during a step: the step context ends at the run deadline.
	run := e.create(t, "t1", "wf", agent.CreateRequest{TimeoutSeconds: 1})
	e.clk.Add(800 * time.Millisecond) // 200ms of the deadline left
	start := time.Now()
	e.drain(t)
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("step not bounded by the run deadline: %s", el)
	}
	if got := e.get(t, "t1", run.ID); got.Status != agent.StatusTimedOut || got.Steps[0].Status != agent.StepFailed {
		t.Fatalf("step timeout %+v", got)
	}
	// Step timeout (agent.step_timeout) fails the step, not the run deadline.
	e.k.Cfg.StepTimeout = 100 * time.Millisecond
	st := e.create(t, "t1", "wf", agent.CreateRequest{})
	e.drain(t)
	got := e.get(t, "t1", st.ID)
	if got.Status != agent.StatusFailed || got.ErrorCode != agent.CodeWorkflowError || got.Steps[0].Status != agent.StepFailed {
		t.Fatalf("step timeout %+v", got)
	}
	// Maintain times out runs nobody executes (waiting past the deadline).
	must(t, e.k.Register("sleepy", agent.Workflow{Func: func(ctx context.Context, r *agent.Run) error {
		return r.Sleep(ctx, "nap", time.Hour)
	}}))
	sl := e.create(t, "t1", "sleepy", agent.CreateRequest{TimeoutSeconds: 60})
	e.drain(t)
	if got := e.get(t, "t1", sl.ID); got.Status != agent.StatusWaiting {
		t.Fatalf("sleepy %s", got.Status)
	}
	e.clk.Add(2 * time.Minute)
	must(t, e.k.Maintain(ctx))
	if got := e.get(t, "t1", sl.ID); got.Status != agent.StatusTimedOut {
		t.Fatalf("maintain %s", got.Status)
	}
}

func TestBudget(t *testing.T) {
	e := newEnv(t)
	var n atomic.Int32
	must(t, e.k.Register("wf", agent.Workflow{Func: func(ctx context.Context, r *agent.Run) error {
		for i := 0; i < 5; i++ {
			if _, err := r.Step(ctx, fmt.Sprintf("s%d", i), func(context.Context) (agent.Output, error) {
				n.Add(1)
				return agent.Output{CostUnits: 6}, nil
			}); err != nil {
				return err
			}
		}
		return nil
	}}))
	steps := e.create(t, "t1", "wf", agent.CreateRequest{MaxSteps: 2})
	e.drain(t)
	got := e.get(t, "t1", steps.ID)
	if got.Status != agent.StatusFailed || got.ErrorCode != agent.CodeBudgetExceeded || got.StepsUsed != 2 || n.Load() != 2 {
		t.Fatalf("max_steps %+v executed %d", got, n.Load())
	}
	n.Store(0)
	cost := e.create(t, "t1", "wf", agent.CreateRequest{MaxCostUnits: 10})
	e.drain(t)
	got = e.get(t, "t1", cost.ID)
	if got.Status != agent.StatusFailed || got.ErrorCode != agent.CodeBudgetExceeded || got.CostUnits != 12 || n.Load() != 2 {
		t.Fatalf("max_cost %+v executed %d", got, n.Load())
	}
	if _, err := e.k.Create(context.Background(), "t1", agent.CreateRequest{Workflow: "wf", MaxSteps: 1_000_000}); code(err) != "invalid_request" {
		t.Fatalf("over max_steps: %v", err)
	}
}

func TestConcurrencyLimits(t *testing.T) {
	e := newEnv(t)
	e.k.Cfg.MaxRunningPerTenant = 1
	bl := newBlocker()
	must(t, e.k.Register("hold", agent.Workflow{MaxConcurrent: 1, Func: func(ctx context.Context, r *agent.Run) error {
		_, err := r.Step(ctx, "s", func(context.Context) (agent.Output, error) {
			if r.Tenant() == "t1" {
				bl.block()
			}
			return agent.Output{}, nil
		})
		return err
	}}))
	first := e.create(t, "t1", "hold", agent.CreateRequest{})
	c := e.claim(t)
	done := make(chan error, 1)
	go func() { done <- e.handle(t, c) }()
	<-bl.started

	same := e.create(t, "t1", "hold", agent.CreateRequest{})  // per-tenant limit
	other := e.create(t, "t2", "hold", agent.CreateRequest{}) // per-workflow limit (MaxConcurrent 1)
	for i := 0; i < 2; i++ {
		must(t, e.handle(t, e.claim(t)))
	}
	for _, id := range []struct{ tenant, id string }{{"t1", same.ID}, {"t2", other.ID}} {
		if got := e.get(t, id.tenant, id.id); got.Status != agent.StatusQueued || got.WaitReason != "concurrency" || len(got.Steps) != 0 {
			t.Fatalf("limited run %+v", got)
		}
	}
	if e.claim(t) != nil {
		t.Fatal("throttled runs must be retried later, not immediately")
	}
	close(bl.release)
	must(t, <-done)
	e.clk.Add(3 * time.Second)
	e.drain(t)
	for _, r := range []struct{ tenant, id string }{{"t1", first.ID}, {"t1", same.ID}, {"t2", other.ID}} {
		if got := e.get(t, r.tenant, r.id); got.Status != agent.StatusSucceeded {
			t.Fatalf("%s: %s", r.id, got.Status)
		}
	}
}

// A long run yields to a continuation job before its lease could expire; no
// step runs twice.
func TestYieldBeforeLeaseExpiry(t *testing.T) {
	e := newEnv(t)
	e.k.Lease = lease
	e.k.Cfg.StepTimeout = 20 * time.Second
	var n atomic.Int32
	must(t, e.k.Register("wf", agent.Workflow{Func: func(ctx context.Context, r *agent.Run) error {
		for i := 0; i < 3; i++ {
			if _, err := r.Step(ctx, fmt.Sprintf("s%d", i), func(context.Context) (agent.Output, error) {
				n.Add(1)
				e.clk.Add(20 * time.Second) // each step takes 20s of the 30s lease
				return agent.Output{}, nil
			}); err != nil {
				return err
			}
		}
		return nil
	}}))
	run := e.create(t, "t1", "wf", agent.CreateRequest{})
	jobsRun := 0
	for c := e.claim(t); c != nil; c = e.claim(t) {
		jobsRun++
		must(t, e.handle(t, c))
	}
	got := e.get(t, "t1", run.ID)
	if got.Status != agent.StatusSucceeded || n.Load() != 3 || jobsRun != 3 {
		t.Fatalf("status %s steps %d jobs %d", got.Status, n.Load(), jobsRun)
	}
}

func TestSleepAndLargeOutputInStorage(t *testing.T) {
	e := newEnv(t)
	st, err := storage.NewLocal(storage.LocalConfig{Root: t.TempDir(), PublicURL: "/v1/files", Secret: bytes.Repeat([]byte("s"), 32)})
	must(t, err)
	e.k.Storage = st
	big := bytes.Repeat([]byte("x"), 20<<10)
	var produced atomic.Int32
	var replayed []byte
	must(t, e.k.Register("wf", agent.Workflow{Func: func(ctx context.Context, r *agent.Run) error {
		b, err := r.Step(ctx, "big", func(context.Context) (agent.Output, error) {
			produced.Add(1)
			return agent.Output{Data: big}, nil
		})
		if err != nil {
			return err
		}
		if err := r.Sleep(ctx, "wait", time.Minute); err != nil {
			return err
		}
		replayed = b
		return nil
	}}))
	run := e.create(t, "t1", "wf", agent.CreateRequest{})
	e.drain(t)
	got := e.get(t, "t1", run.ID)
	if got.Status != agent.StatusWaiting || got.WaitReason != "sleep" || got.Steps[0].OutputRef == "" || got.Steps[1].Kind != agent.KindWait {
		t.Fatalf("sleeping %+v", got)
	}
	e.clk.Add(time.Minute)
	e.drain(t)
	got = e.get(t, "t1", run.ID)
	if got.Status != agent.StatusSucceeded || produced.Load() != 1 || !bytes.Equal(replayed, big) {
		t.Fatalf("status %s produced %d replayed %d bytes", got.Status, produced.Load(), len(replayed))
	}
	// Without object storage a large output fails the step.
	e.k.Storage = nil
	run = e.create(t, "t1", "wf", agent.CreateRequest{})
	e.drain(t)
	if got := e.get(t, "t1", run.ID); got.Status != agent.StatusFailed || got.Steps[0].Status != agent.StepFailed {
		t.Fatalf("no storage %+v", got)
	}
}

func TestSecretsAreRedacted(t *testing.T) {
	e := newEnv(t)
	t.Setenv("AGENT_TEST_MODEL_KEY", "sk-very-secret-value")
	must(t, e.k.Register("wf", agent.Workflow{Secrets: map[string]string{"model": "env:AGENT_TEST_MODEL_KEY"},
		Func: func(ctx context.Context, r *agent.Run) error {
			key, err := r.Secret("model")
			if err != nil {
				return err
			}
			if _, err := r.Step(ctx, "call", func(context.Context) (agent.Output, error) {
				return agent.Output{}, fmt.Errorf("provider rejected key %s", key)
			}); err != nil {
				return err
			}
			return nil
		}}))
	if err := e.k.Register("bad", agent.Workflow{Secrets: map[string]string{"x": "literal"}, Func: func(context.Context, *agent.Run) error { return nil }}); err == nil {
		t.Fatal("literal secret accepted")
	}
	run := e.create(t, "t1", "wf", agent.CreateRequest{})
	e.drain(t)
	got := e.get(t, "t1", run.ID)
	all, _ := json.Marshal(got)
	if got.Status != agent.StatusFailed || strings.Contains(string(all), "sk-very-secret-value") || !strings.Contains(got.Steps[0].Error, "[REDACTED]") {
		t.Fatalf("run %s", all)
	}
}

func TestNondeterministicWorkflowFails(t *testing.T) {
	e := newEnv(t)
	var flip atomic.Bool
	must(t, e.k.Register("wf", agent.Workflow{Func: func(ctx context.Context, r *agent.Run) error {
		name := "a"
		if flip.Load() {
			name = "b"
		}
		if _, err := r.Step(ctx, name, out("x")); err != nil {
			return err
		}
		return r.Sleep(ctx, "z", time.Second)
	}}))
	run := e.create(t, "t1", "wf", agent.CreateRequest{})
	e.drain(t)
	flip.Store(true)
	e.clk.Add(time.Second)
	e.drain(t)
	if got := e.get(t, "t1", run.ID); got.Status != agent.StatusFailed || got.ErrorCode != agent.CodeNondeterministic {
		t.Fatalf("run %+v", got)
	}
}

func TestHTTPTenantIsolationAndScopes(t *testing.T) {
	e := newEnv(t)
	must(t, e.k.Register("wf", agent.Workflow{Func: func(ctx context.Context, r *agent.Run) error {
		_, err := r.Step(ctx, "s", out("x"))
		return err
	}}))
	keys := auth.APIKeys{DB: e.k.DB}
	ctx := context.Background()
	_, ka, err := keys.Create(ctx, "ta", "a", []string{"agent"}, 0)
	must(t, err)
	_, kb, err := keys.Create(ctx, "tb", "b", []string{"agent"}, 0)
	must(t, err)
	_, kn, err := keys.Create(ctx, "ta", "n", []string{"billing"}, 0)
	must(t, err)
	mux := http.NewServeMux()
	e.k.Routes(mux, keys)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	do := func(method, path, key, body string, hdr ...string) (int, map[string]any) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		res, err := http.DefaultClient.Do(req)
		must(t, err)
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return res.StatusCode, m
	}
	st, m := do("POST", "/v1/agent/runs", ka, `{"workflow":"wf","input":{"q":1}}`, "Idempotency-Key", "k1")
	if st != 201 {
		t.Fatalf("create %d %v", st, m)
	}
	id := m["id"].(string)
	if st, m2 := do("POST", "/v1/agent/runs", ka, `{"workflow":"wf","input":{"q":1}}`, "Idempotency-Key", "k1"); st != 201 || m2["id"] != id {
		t.Fatalf("idempotent replay %d %v", st, m2)
	}
	if st, _ := do("POST", "/v1/agent/runs", ka, `{"workflow":"nope"}`); st != 400 {
		t.Fatalf("unknown workflow %d", st)
	}
	if st, _ := do("POST", "/v1/agent/runs", kn, `{"workflow":"wf"}`); st != 403 {
		t.Fatalf("missing scope %d", st)
	}
	if st, _ := do("GET", "/v1/agent/runs", ""+"gsk_bogus", ""); st != 401 {
		t.Fatalf("bad key %d", st)
	}
	e.drain(t)
	st, m = do("GET", "/v1/agent/runs/"+id, ka, "")
	if st != 200 || m["status"] != "succeeded" || len(m["steps"].([]any)) != 1 {
		t.Fatalf("get %d %v", st, m)
	}
	for _, p := range []string{"/v1/agent/runs/" + id, "/v1/agent/runs/" + id + "/audit"} {
		if st, _ := do("GET", p, kb, ""); st != 404 {
			t.Fatalf("other tenant GET %s: %d", p, st)
		}
	}
	if st, _ := do("POST", "/v1/agent/runs/"+id+"/cancel", kb, ""); st != 404 {
		t.Fatalf("other tenant cancel %d", st)
	}
	if st, _ := do("POST", "/v1/agent/runs/"+id+"/steps/0/resolve", kb, `{"outcome":"failed","note":"x"}`); st != 404 {
		t.Fatalf("other tenant resolve %d", st)
	}
	if _, m := do("GET", "/v1/agent/runs", kb, ""); len(m["runs"].([]any)) != 0 {
		t.Fatalf("other tenant list %v", m)
	}
	if _, m := do("GET", "/v1/agent/runs?status=succeeded", ka, ""); len(m["runs"].([]any)) != 1 {
		t.Fatalf("list %v", m)
	}
	if st, m := do("POST", "/v1/agent/runs/"+id+"/cancel", ka, ""); st != 409 {
		t.Fatalf("cancel finished %d %v", st, m)
	}
	if st, m := do("GET", "/v1/agent/runs/"+id+"/audit", ka, ""); st != 200 || len(m["tool_calls"].([]any)) != 0 {
		t.Fatalf("audit %d %v", st, m)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func code(err error) string {
	var e *httpx.Error
	if errors.As(err, &e) {
		return e.Code
	}
	if err == nil {
		return ""
	}
	return "untyped:" + err.Error()
}
