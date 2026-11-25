package agent_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pillar/gostack/kits/agent"
)

// A non-idempotent tool that is still running when agent.step_timeout ends
// its context may have taken effect: the step must not be recorded as a
// definite failure (a retry-on-failure workflow would send it again with a
// new key). It is parked for reconciliation like a lost-lease step; an
// idempotent tool keeps the plain failure.
func TestNonIdempotentToolTimeoutNeedsReconciliation(t *testing.T) {
	e := newEnv(t)
	e.k.Cfg.StepTimeout = 100 * time.Millisecond
	var charges, puts atomic.Int32
	slow := func(n *atomic.Int32) func(ctx context.Context, key string) (agent.Output, error) {
		return func(ctx context.Context, key string) (agent.Output, error) {
			n.Add(1) // the request reached the provider
			<-ctx.Done()
			return agent.Output{}, ctx.Err()
		}
	}
	retrying := func(tool string, idempotent bool, n *atomic.Int32) agent.Workflow {
		return agent.Workflow{Func: func(ctx context.Context, r *agent.Run) error {
			var err error
			for i := 0; i < 2; i++ { // typical retry-on-failure workflow
				_, err = r.Tool(ctx, fmt.Sprintf("%s-%d", tool, i), agent.ToolCall{Tool: tool, Request: []byte(`{"amount":5}`), Idempotent: idempotent}, slow(n))
				var se *agent.StepError
				if err == nil || !errors.As(err, &se) {
					return err
				}
			}
			return err
		}}
	}
	must(t, e.k.Register("pay", retrying("psp", false, &charges)))
	must(t, e.k.Register("put", retrying("kv", true, &puts)))

	run := e.create(t, "t1", "pay", agent.CreateRequest{})
	e.drain(t)
	got := e.get(t, "t1", run.ID)
	if charges.Load() != 1 {
		t.Fatalf("non-idempotent tool sent %d times after a step timeout (double charge)", charges.Load())
	}
	if got.Status != agent.StatusWaiting || got.WaitReason != "reconciliation" || len(got.Steps) != 1 || got.Steps[0].Status != agent.StepNeedsReconciliation {
		t.Fatalf("run %+v", got)
	}
	audit, _ := e.k.Audit(context.Background(), "t1", run.ID)
	var outcomes []string
	for _, a := range audit {
		outcomes = append(outcomes, a.Outcome)
	}
	if strings.Join(outcomes, ",") != "started,unknown" {
		t.Fatalf("audit outcomes %v", outcomes)
	}
	if e.claim(t) != nil {
		t.Fatal("a parked run must not have a runnable job")
	}
	// The operator resolves it; the workflow continues without re-sending.
	_, err := e.k.Resolve(context.Background(), "t1", run.ID, 0, agent.StepFailed, nil, "", "PSP shows no charge")
	must(t, err)
	e.drain(t)
	if charges.Load() != 2 { // only now does the workflow's own retry (a new step) send again
		t.Fatalf("after resolution: %d sends", charges.Load())
	}

	// An idempotent tool keeps the plain failure (retries are safe).
	run2 := e.create(t, "t1", "put", agent.CreateRequest{})
	e.drain(t)
	got = e.get(t, "t1", run2.ID)
	if puts.Load() != 2 || got.Status != agent.StatusFailed || got.Steps[0].Status != agent.StepFailed {
		t.Fatalf("idempotent: %d calls, run %+v", puts.Load(), got)
	}
}
