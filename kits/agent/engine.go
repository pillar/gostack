package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/jobs"
)

// Output is what a step function returns.
type Output struct {
	Data []byte
	// CostUnits is charged to the run budget (e.g. tokens or provider cost).
	CostUnits int64
	// ExternalRef identifies the external effect of a tool call (audit).
	ExternalRef string
}

// ToolCall describes an external tool invocation for the audit log.
type ToolCall struct {
	Tool string
	// Request is hashed (SHA-256) into the audit log; it is not stored.
	Request []byte
	// Idempotent declares that the external system deduplicates by the
	// idempotency key passed to the step function, so a call interrupted by a
	// crash may be re-sent. Otherwise an interrupted call needs reconciliation.
	Idempotent bool
}

// StepError is returned by Step/Tool when the step function failed (also on
// replay of a failed step). The workflow decides whether that fails the run.
type StepError struct {
	Seq int64
	Msg string
}

func (e *StepError) Error() string { return fmt.Sprintf("agent step %d failed: %s", e.Seq, e.Msg) }

// halt is returned through the workflow when the run must stop executing in
// this job: its new state is already persisted.
type halt struct{ reason string }

func (h *halt) Error() string { return "agent: run halted: " + h.reason }

// ErrHalted reports whether err is a stop/yield signal that the workflow
// must return unchanged.
func ErrHalted(err error) bool {
	var h *halt
	return errors.As(err, &h)
}

var errLeaseLost = errors.New("agent: run is owned by another worker")

// infraErr marks storage/database failures: the job is retried and the run
// resumes from its checkpoints instead of failing.
type infraErr struct{ err error }

func (e *infraErr) Error() string { return "agent: infrastructure: " + e.err.Error() }
func (e *infraErr) Unwrap() error { return e.err }

func infra(err error) error {
	if err == nil || errors.Is(err, errLeaseLost) || ErrHalted(err) {
		return err
	}
	var ie *infraErr
	if errors.As(err, &ie) {
		return err
	}
	return &infraErr{err}
}

// Run is the handle a workflow uses during one execution.
type Run struct {
	k        *Kit
	wf       Workflow
	tenant   string
	id       string
	workflow string
	input    json.RawMessage
	token    string
	start    time.Time
	next     int64
	executed int
	known    map[int64]stepRow
	output   json.RawMessage

	mu      sync.Mutex
	secrets []string
}

type stepRow struct {
	name, kind, status, outputRef, tool string
	attempt                             int64
	idempotent                          bool
	wakeAt                              *int64
	errText                             string
}

func (r *Run) ID() string             { return r.id }
func (r *Run) Tenant() string         { return r.tenant }
func (r *Run) Workflow() string       { return r.workflow }
func (r *Run) Input() json.RawMessage { return r.input }
func (r *Run) Decode(v any) error     { return json.Unmarshal(r.input, v) }
func (r *Run) SetOutput(v any) error  { b, err := json.Marshal(v); r.output = b; return err }

// Secret resolves a credential declared in Workflow.Secrets. The value is
// never persisted and is redacted from recorded step errors.
func (r *Run) Secret(name string) (string, error) {
	ref, ok := r.wf.Secrets[name]
	if !ok {
		return "", fmt.Errorf("agent: secret %q is not declared for workflow %s", name, r.workflow)
	}
	v, err := config.ResolveSecret(ref)
	if err != nil {
		return "", fmt.Errorf("agent: secret %q: %w", name, err)
	}
	r.mu.Lock()
	r.secrets = append(r.secrets, v)
	r.mu.Unlock()
	return v, nil
}

func (r *Run) redact(s string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range r.secrets {
		if len(v) >= 4 {
			s = strings.ReplaceAll(s, v, "[REDACTED]")
		}
	}
	return truncate(s)
}

// HandleJob executes (or resumes) the run referenced by job. It returns an
// error only for infrastructure failures (the queue retries the job); run
// outcomes are recorded on the run.
func (k *Kit) HandleJob(ctx context.Context, c *jobs.Claimed) error {
	var p jobPayload
	if err := json.Unmarshal(c.Payload, &p); err != nil || p.RunID == "" {
		k.log().Warn("agent: malformed job payload", "job", c.ID)
		return nil
	}
	tenant := c.TenantID
	var run RunInfo
	proceed := false
	err := k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		proceed = false
		r, err := k.getTx(ctx, tx, tenant, p.RunID)
		if err != nil {
			var he *httpx.Error
			if errors.As(err, &he) {
				return nil // run vanished; nothing to do
			}
			return err
		}
		run = r
		now := k.now()
		nowMs := now.UnixMilli()
		switch {
		case terminal(r.Status):
			return nil
		case r.Status == StatusWaiting && r.WaitReason == "reconciliation":
			return nil // waits for an operator; a stale job must not resume it
		case r.CancelReq:
			return finish(ctx, tx, tenant, r.ID, StatusCancelled, CodeCancelled, "cancelled by request", nil, nowMs)
		case nowMs >= r.DeadlineAt:
			return finish(ctx, tx, tenant, r.ID, StatusTimedOut, CodeTimedOut, "run deadline exceeded", nil, nowMs)
		}
		if r.Status != StatusRunning { // queued or waiting: needs a concurrency slot
			wf, _ := k.workflow(r.Workflow)
			var n int64
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_runs WHERE tenant_id=? AND status='running'`, tenant).Scan(&n); err != nil {
				return err
			}
			full := n >= int64(k.Cfg.MaxRunningPerTenant)
			if !full && wf.MaxConcurrent > 0 {
				if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM agent_runs WHERE workflow=? AND status='running'`, r.Workflow).Scan(&n); err != nil {
					return err
				}
				full = n >= int64(wf.MaxConcurrent)
			}
			if full {
				var gen int64
				if err := tx.QueryRowContext(ctx, `UPDATE agent_runs SET status=?, wait_reason='concurrency', generation=generation+1, updated_at=?
					WHERE tenant_id=? AND id=? RETURNING generation`, StatusQueued, nowMs, tenant, r.ID).Scan(&gen); err != nil {
					return err
				}
				return k.enqueue(ctx, tx, tenant, r.ID, gen, now.Add(retryBusy))
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agent_runs SET status=?, wait_reason='', owner_token=?, updated_at=? WHERE tenant_id=? AND id=?`,
			StatusRunning, c.Token, nowMs, tenant, r.ID); err != nil {
			return err
		}
		proceed = true
		return nil
	})
	if err != nil || !proceed {
		return err
	}
	wf, ok := k.workflow(run.Workflow)
	if !ok {
		return k.fenced(ctx, tenant, run.ID, c.Token, func(tx *sql.Tx, now int64) error {
			return finish(ctx, tx, tenant, run.ID, StatusFailed, CodeUnknownWorkflow, "workflow "+run.Workflow+" is not registered in this process", nil, now)
		})
	}
	known := map[int64]stepRow{}
	rows, err := k.DB.Read.QueryContext(ctx, `SELECT seq,name,kind,status,output_ref,tool,attempt,idempotent,wake_at,error FROM agent_steps WHERE tenant_id=? AND run_id=?`, tenant, run.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var seq, idem int64
		var s stepRow
		if err := rows.Scan(&seq, &s.name, &s.kind, &s.status, &s.outputRef, &s.tool, &s.attempt, &idem, &s.wakeAt, &s.errText); err != nil {
			rows.Close()
			return err
		}
		s.idempotent = idem != 0
		known[seq] = s
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	r := &Run{k: k, wf: wf, tenant: tenant, id: run.ID, workflow: run.Workflow, input: run.Input, token: c.Token,
		start: k.now(), known: known}
	werr := func() (err error) {
		defer func() {
			if v := recover(); v != nil {
				err = fmt.Errorf("panic: %v", v)
			}
		}()
		return wf.Func(ctx, r)
	}()
	var ie *infraErr
	switch {
	case errors.Is(werr, errLeaseLost):
		k.log().Warn("agent: lease lost; another worker owns the run", "run", run.ID)
		return nil
	case ErrHalted(werr):
		return nil
	case errors.As(werr, &ie):
		return ie.err // the queue retries the job; completed steps replay
	}
	ferr := k.fenced(ctx, tenant, run.ID, c.Token, func(tx *sql.Tx, now int64) error {
		if werr == nil {
			return finish(ctx, tx, tenant, run.ID, StatusSucceeded, "", "", r.output, now)
		}
		return finish(ctx, tx, tenant, run.ID, StatusFailed, CodeWorkflowError, r.redact(werr.Error()), nil, now)
	})
	if errors.Is(ferr, errLeaseLost) {
		return nil
	}
	return ferr
}

func finish(ctx context.Context, tx *sql.Tx, tenant, id, status, code, msg string, output []byte, now int64) error {
	if len(output) > maxOutputBytes {
		status, code, msg, output = StatusFailed, CodeOutputTooLarge, "run output exceeds 60 KiB", nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE agent_runs SET status=?, error_code=?, error=?, output=?, owner_token='', wait_reason='',
		finished_at=?, updated_at=? WHERE tenant_id=? AND id=?`, status, code, truncate(msg), output, now, now, tenant, id)
	return err
}

// fenced runs fn in a write transaction only while token still owns the run.
func (k *Kit) fenced(ctx context.Context, tenant, id, token string, fn func(tx *sql.Tx, now int64) error) error {
	return k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		now := k.now().UnixMilli()
		res, err := tx.ExecContext(ctx, `UPDATE agent_runs SET updated_at=? WHERE tenant_id=? AND id=? AND owner_token=? AND status='running'`, now, tenant, id, token)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errLeaseLost
		}
		return fn(tx, now)
	})
}

// stop persists a terminal or parked state and returns the halt signal.
func (r *Run) stop(ctx context.Context, fn func(tx *sql.Tx, now int64) error, reason string) error {
	if err := r.ownedTx(ctx, fn); err != nil {
		return err
	}
	return &halt{reason}
}

func (r *Run) stopWith(ctx context.Context, status, code, msg string) error {
	return r.stop(ctx, func(tx *sql.Tx, now int64) error {
		return finish(ctx, tx, r.tenant, r.id, status, code, msg, nil, now)
	}, code)
}

// park releases the run to a continuation job (yield, sleep).
func (r *Run) park(ctx context.Context, status, reason string, at time.Time, extra func(tx *sql.Tx, now int64) error) error {
	return r.stop(ctx, func(tx *sql.Tx, now int64) error {
		if extra != nil {
			if err := extra(tx, now); err != nil {
				return err
			}
		}
		var gen int64
		if err := tx.QueryRowContext(ctx, `UPDATE agent_runs SET status=?, wait_reason=?, owner_token='', generation=generation+1, updated_at=?
			WHERE tenant_id=? AND id=? RETURNING generation`, status, reason, now, r.tenant, r.id).Scan(&gen); err != nil {
			return err
		}
		if reason == "reconciliation" {
			return nil // resumed by Resolve
		}
		return r.k.enqueue(ctx, tx, r.tenant, r.id, gen, at)
	}, reason)
}

type stepSpec struct {
	name, kind string
	tool       ToolCall
}

// Step runs a model call or other computation that is safe to execute again
// after a crash (kind llm). On replay it returns the checkpointed output.
func (r *Run) Step(ctx context.Context, name string, fn func(ctx context.Context) (Output, error)) ([]byte, error) {
	return r.do(ctx, stepSpec{name: name, kind: KindLLM}, func(ctx context.Context, _ string) (Output, error) { return fn(ctx) })
}

// Tool runs an external tool call (kind tool) with an audit trail. fn
// receives a stable idempotency key (same across retries of this step) that
// it should pass to the external system.
func (r *Run) Tool(ctx context.Context, name string, call ToolCall, fn func(ctx context.Context, idempotencyKey string) (Output, error)) ([]byte, error) {
	if call.Tool == "" {
		call.Tool = name
	}
	return r.do(ctx, stepSpec{name: name, kind: KindTool, tool: call}, fn)
}

// Sleep parks the run (status waiting) for d without holding a worker; the
// run resumes in a later job (kind wait).
func (r *Run) Sleep(ctx context.Context, name string, d time.Duration) error {
	seq := r.next
	r.next++
	if s, ok := r.known[seq]; ok {
		if s.name != name || s.kind != KindWait {
			return r.nondeterministic(ctx, seq, name)
		}
		if s.status == StepSucceeded {
			return nil
		}
		now := r.k.now()
		if s.wakeAt != nil && now.UnixMilli() >= *s.wakeAt {
			return r.ownedTx(ctx, func(tx *sql.Tx, ms int64) error {
				_, err := tx.ExecContext(ctx, `UPDATE agent_steps SET status=?, finished_at=? WHERE tenant_id=? AND run_id=? AND seq=?`,
					StepSucceeded, ms, r.tenant, r.id, seq)
				return err
			})
		}
		wake := now
		if s.wakeAt != nil {
			wake = time.UnixMilli(*s.wakeAt)
		}
		return r.park(ctx, StatusWaiting, "sleep", wake, nil)
	}
	if err := r.precheck(ctx, true); err != nil {
		return err
	}
	now := r.k.now()
	wake := now.Add(d)
	return r.park(ctx, StatusWaiting, "sleep", wake, func(tx *sql.Tx, ms int64) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_steps(tenant_id,run_id,seq,name,kind,status,attempt,wake_at,started_at)
			VALUES(?,?,?,?,?,?,1,?,?)`, r.tenant, r.id, seq, name, KindWait, StepWaiting, wake.UnixMilli(), ms); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE agent_runs SET steps_used=steps_used+1 WHERE tenant_id=? AND id=?`, r.tenant, r.id)
		return err
	})
}

func (r *Run) ownedTx(ctx context.Context, fn func(tx *sql.Tx, now int64) error) error {
	return infra(r.k.fenced(ctx, r.tenant, r.id, r.token, fn))
}

func (r *Run) nondeterministic(ctx context.Context, seq int64, name string) error {
	return r.stopWith(ctx, StatusFailed, CodeNondeterministic,
		fmt.Sprintf("step %d replayed as %q but was recorded differently; workflows must issue the same steps in the same order", seq, name))
}

// precheck runs before a new step executes: cancellation, deadline, budget,
// and yielding to a continuation job before the lease could expire.
func (r *Run) precheck(ctx context.Context, newStep bool) error {
	var cancel, steps, cost, maxSteps, maxCost, deadline int64
	var status string
	err := r.k.DB.Read.QueryRowContext(ctx, `SELECT status,cancel_requested,steps_used,cost_units,max_steps,max_cost_units,deadline_at
		FROM agent_runs WHERE tenant_id=? AND id=?`, r.tenant, r.id).Scan(&status, &cancel, &steps, &cost, &maxSteps, &maxCost, &deadline)
	if err != nil {
		return infra(err)
	}
	now := r.k.now()
	switch {
	case cancel != 0:
		return r.stopWith(ctx, StatusCancelled, CodeCancelled, "cancelled by request")
	case now.UnixMilli() >= deadline:
		return r.stopWith(ctx, StatusTimedOut, CodeTimedOut, "run deadline exceeded")
	case newStep && steps >= maxSteps:
		return r.stopWith(ctx, StatusFailed, CodeBudgetExceeded, fmt.Sprintf("max_steps %d reached", maxSteps))
	case maxCost > 0 && cost >= maxCost:
		return r.stopWith(ctx, StatusFailed, CodeBudgetExceeded, fmt.Sprintf("max_cost_units %d reached", maxCost))
	}
	if r.executed > 0 && r.k.Lease > 0 && now.Sub(r.start)+r.k.Cfg.StepTimeout+time.Second > r.k.Lease {
		return r.park(ctx, StatusRunning, "", time.Time{}, nil) // yield: continue in a fresh job
	}
	return nil
}

func (r *Run) do(ctx context.Context, spec stepSpec, fn func(ctx context.Context, key string) (Output, error)) ([]byte, error) {
	seq := r.next
	r.next++
	key := idemKey(r.id, seq)
	attempt := int64(1)
	retryAudit := false
	if s, ok := r.known[seq]; ok {
		if s.name != spec.name || s.kind != spec.kind {
			return nil, r.nondeterministic(ctx, seq, spec.name)
		}
		switch s.status {
		case StepSucceeded:
			return r.checkpoint(ctx, seq, s.outputRef)
		case StepFailed:
			return nil, &StepError{Seq: seq, Msg: s.errText}
		case StepNeedsReconciliation:
			return nil, r.park(ctx, StatusWaiting, "reconciliation", time.Time{}, nil)
		case StepRunning:
			// The previous execution died during this step.
			if spec.kind == KindTool && !s.idempotent {
				return nil, r.park(ctx, StatusWaiting, "reconciliation", time.Time{}, func(tx *sql.Tx, now int64) error {
					if _, err := tx.ExecContext(ctx, `UPDATE agent_steps SET status=?, error=? WHERE tenant_id=? AND run_id=? AND seq=?`,
						StepNeedsReconciliation, "worker lost during a non-idempotent tool call; outcome unknown", r.tenant, r.id, seq); err != nil {
						return err
					}
					return audit(ctx, tx, r.tenant, r.id, seq, s.attempt, s.tool, key, hashRequest(spec.tool.Request), "unknown", "",
						"worker lost during the call", now)
				})
			}
			attempt = s.attempt + 1
			retryAudit = spec.kind == KindTool
		}
	}
	if err := r.precheck(ctx, attempt == 1); err != nil {
		return nil, err
	}
	reqHash := ""
	if spec.kind == KindTool {
		reqHash = hashRequest(spec.tool.Request)
	}
	// Record the step (and, for tools, the audit "started" row) before any external IO.
	err := r.ownedTx(ctx, func(tx *sql.Tx, now int64) error {
		if attempt == 1 {
			idem := 0
			if spec.tool.Idempotent {
				idem = 1
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO agent_steps(tenant_id,run_id,seq,name,kind,status,attempt,tool,idempotent,started_at)
				VALUES(?,?,?,?,?,?,?,?,?,?)`, r.tenant, r.id, seq, spec.name, spec.kind, StepRunning, attempt, spec.tool.Tool, idem, now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE agent_runs SET steps_used=steps_used+1 WHERE tenant_id=? AND id=?`, r.tenant, r.id); err != nil {
				return err
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE agent_steps SET attempt=?, started_at=? WHERE tenant_id=? AND run_id=? AND seq=?`,
			attempt, now, r.tenant, r.id, seq); err != nil {
			return err
		}
		if spec.kind != KindTool {
			return nil
		}
		if retryAudit {
			if err := audit(ctx, tx, r.tenant, r.id, seq, attempt, spec.tool.Tool, key, reqHash, "retry", "", "re-sent after worker loss with the same idempotency key", now); err != nil {
				return err
			}
		}
		return audit(ctx, tx, r.tenant, r.id, seq, attempt, spec.tool.Tool, key, reqHash, "started", "", "", now)
	})
	if err != nil {
		return nil, err
	}
	r.executed++
	out, ferr, cause := r.execute(ctx, fn, key)
	switch cause {
	case CodeCancelled:
		return nil, r.stepStop(ctx, seq, attempt, spec, key, reqHash, StatusCancelled, CodeCancelled, "cancelled by request")
	case CodeTimedOut:
		return nil, r.stepStop(ctx, seq, attempt, spec, key, reqHash, StatusTimedOut, CodeTimedOut, "run deadline exceeded during step")
	case causeInterrupted:
		// The step context ended (step_timeout, or the worker is shutting
		// down) while the call was in flight: the request may have reached
		// the external system. A non-idempotent call must not be reported as
		// a definite failure (a retrying workflow would send it again under a
		// new key); it goes to reconciliation like a lost-lease step.
		if spec.kind == KindTool && !spec.tool.Idempotent {
			msg := "step context ended during a non-idempotent tool call; outcome unknown"
			return nil, r.park(ctx, StatusWaiting, "reconciliation", time.Time{}, func(tx *sql.Tx, now int64) error {
				if _, err := tx.ExecContext(ctx, `UPDATE agent_steps SET status=?, error=? WHERE tenant_id=? AND run_id=? AND seq=?`,
					StepNeedsReconciliation, msg, r.tenant, r.id, seq); err != nil {
					return err
				}
				return audit(ctx, tx, r.tenant, r.id, seq, attempt, spec.tool.Tool, key, reqHash, "unknown", "", r.redact(ferr.Error()), now)
			})
		}
	}
	ref := ""
	data := out.Data
	if ferr == nil && len(data) > r.k.Cfg.InlineOutputBytes {
		if r.k.Storage == nil {
			ferr = fmt.Errorf("step output of %d bytes exceeds agent.inline_output_bytes and no object storage is configured", len(data))
		} else {
			ref = fmt.Sprintf("agent/%s/%d-%d", r.id, seq, attempt)
			if perr := r.k.Storage.Put(ctx, ref, bytes.NewReader(data), int64(len(data)), "application/octet-stream"); perr != nil {
				return nil, infra(fmt.Errorf("agent: store step output: %w", perr)) // the job retries the step
			}
			data = nil
		}
	}
	if out.CostUnits < 0 {
		out.CostUnits = 0
	}
	var overBudget bool
	err = r.ownedTx(ctx, func(tx *sql.Tx, now int64) error {
		status, errText, outcome := StepSucceeded, "", "succeeded"
		if ferr != nil {
			status, errText, outcome = StepFailed, r.redact(ferr.Error()), "failed"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agent_steps SET status=?, cost_units=?, output_ref=?, output_bytes=?, error=?, finished_at=?
			WHERE tenant_id=? AND run_id=? AND seq=?`, status, out.CostUnits, ref, len(out.Data), errText, now, r.tenant, r.id, seq); err != nil {
			return err
		}
		if ferr == nil {
			state := data
			if state == nil {
				state = []byte{}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO agent_checkpoints(tenant_id,run_id,seq,state,created_at) VALUES(?,?,?,?,?)`,
				r.tenant, r.id, seq, state, now); err != nil {
				return err
			}
		}
		var cost, maxCost int64
		if err := tx.QueryRowContext(ctx, `UPDATE agent_runs SET cost_units=cost_units+? WHERE tenant_id=? AND id=? RETURNING cost_units, max_cost_units`,
			out.CostUnits, r.tenant, r.id).Scan(&cost, &maxCost); err != nil {
			return err
		}
		overBudget = maxCost > 0 && cost > maxCost
		if spec.kind == KindTool {
			return audit(ctx, tx, r.tenant, r.id, seq, attempt, spec.tool.Tool, key, reqHash, outcome, out.ExternalRef, errText, now)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if overBudget {
		return nil, r.stopWith(ctx, StatusFailed, CodeBudgetExceeded, "max_cost_units exceeded")
	}
	if ferr != nil {
		return nil, &StepError{Seq: seq, Msg: r.redact(ferr.Error())}
	}
	return out.Data, nil
}

// stepStop marks the step failed and stops the run (cancel / timeout during a step).
func (r *Run) stepStop(ctx context.Context, seq, attempt int64, spec stepSpec, key, reqHash, status, code, msg string) error {
	return r.stop(ctx, func(tx *sql.Tx, now int64) error {
		if _, err := tx.ExecContext(ctx, `UPDATE agent_steps SET status=?, error=?, finished_at=? WHERE tenant_id=? AND run_id=? AND seq=?`,
			StepFailed, msg, now, r.tenant, r.id, seq); err != nil {
			return err
		}
		if spec.kind == KindTool {
			// The call was interrupted; whether it took effect is unknown.
			if err := audit(ctx, tx, r.tenant, r.id, seq, attempt, spec.tool.Tool, key, reqHash, "unknown", "", msg, now); err != nil {
				return err
			}
		}
		return finish(ctx, tx, r.tenant, r.id, status, code, msg, nil, now)
	}, code)
}

// execute runs fn under the step timeout, the run deadline and a cancel watcher.
// cause is CodeCancelled / CodeTimedOut when the run (not the step) was
// stopped, causeInterrupted when fn failed after the step context ended.
func (r *Run) execute(ctx context.Context, fn func(ctx context.Context, key string) (Output, error), key string) (out Output, err error, cause string) {
	var deadline int64
	if err := r.k.DB.Read.QueryRowContext(ctx, `SELECT deadline_at FROM agent_runs WHERE tenant_id=? AND id=?`, r.tenant, r.id).Scan(&deadline); err != nil {
		deadline = r.k.now().Add(r.k.Cfg.StepTimeout).UnixMilli() // fall back to the step timeout; precheck already read it
	}
	limit := r.k.Cfg.StepTimeout
	runLeft := time.UnixMilli(deadline).Sub(r.k.now())
	runBound := runLeft < limit
	if runBound {
		limit = runLeft
	}
	sctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	var cancelled sync.WaitGroup
	stopWatch := make(chan struct{})
	var wasCancelled bool
	cancelled.Add(1)
	go func() {
		defer cancelled.Done()
		poll := r.k.Cfg.CancelPoll
		if poll <= 0 {
			poll = time.Second
		}
		t := time.NewTicker(poll)
		defer t.Stop()
		for {
			select {
			case <-stopWatch:
				return
			case <-sctx.Done():
				return
			case <-t.C:
				var c int64
				if err := r.k.DB.Read.QueryRowContext(sctx, `SELECT cancel_requested FROM agent_runs WHERE tenant_id=? AND id=?`, r.tenant, r.id).Scan(&c); err == nil && c != 0 {
					wasCancelled = true
					cancel()
					return
				}
			}
		}
	}()
	out, err = func() (o Output, e error) {
		defer func() {
			if v := recover(); v != nil {
				e = fmt.Errorf("panic: %v", v)
			}
		}()
		return fn(sctx, key)
	}()
	close(stopWatch)
	cancelled.Wait()
	switch {
	case wasCancelled:
		return out, err, CodeCancelled
	case runBound && err != nil && errors.Is(sctx.Err(), context.DeadlineExceeded):
		return out, err, CodeTimedOut
	case err != nil && sctx.Err() != nil:
		return out, err, causeInterrupted
	}
	return out, err, ""
}

// causeInterrupted: fn failed after its context ended (step timeout or the
// caller's context), so a call it made may or may not have taken effect.
const causeInterrupted = "interrupted"

// checkpoint loads a completed step's output.
func (r *Run) checkpoint(ctx context.Context, seq int64, ref string) ([]byte, error) {
	if ref != "" {
		if r.k.Storage == nil {
			return nil, errors.New("agent: step output is in object storage but no storage is configured")
		}
		rc, _, err := r.k.Storage.Get(ctx, ref)
		if err != nil {
			return nil, infra(fmt.Errorf("agent: load step output: %w", err))
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		return b, infra(err)
	}
	var b []byte
	err := r.k.DB.Read.QueryRowContext(ctx, `SELECT state FROM agent_checkpoints WHERE tenant_id=? AND run_id=? AND seq=?`, r.tenant, r.id, seq).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return []byte{}, nil
	}
	if b == nil {
		b = []byte{}
	}
	return b, infra(err)
}
