// Package agent is AgentKit P1 (PRD §19, §24): durable, queryable agent runs.
//
// A run is created over HTTP (or with Kit.Create) and executed by a job of
// type JobType on the durable queue (modules/jobs). The registered workflow is
// a Go function func(ctx, *Run) error that performs its work through steps:
//
//	out, err := run.Step("plan", func(ctx context.Context) (agent.Output, error) {...})
//	out, err := run.Tool("send", agent.ToolCall{Tool: "mailer", Request: req}, func(ctx context.Context, key string) (agent.Output, error) {...})
//	err = run.Sleep("cooldown", time.Minute)
//
// Every step is recorded (seq, kind, status, attempt, cost) and its output is
// checkpointed when it completes. Recovery is replay: when a worker dies, the
// job lease expires, another worker claims the job and calls the workflow
// again; completed steps return their checkpointed output without executing.
// Workflows must therefore be deterministic in the sequence of steps they
// issue (same names in the same order for the same input and step outputs).
//
// Guarantees and non-guarantees:
//   - Steps run at least once. A step interrupted by a crash is executed again
//     (kind llm), re-sent with the SAME idempotency key (kind tool, Idempotent),
//     or — for a tool call whose external effect may or may not have happened —
//     parked as needs_reconciliation for an operator decision instead of a
//     blind retry. Exactly-once external side effects are NOT provided.
//   - A worker whose lease was lost is fenced: its writes are refused.
//   - Cancellation is cooperative: checked between steps and, during a step,
//     by cancelling the step context (polled every agent.cancel_poll).
//   - Deadline (run timeout) and per-step timeout; budgets (max_steps,
//     max_cost_units) stop the run with error_code budget_exceeded.
//   - Concurrency: at most agent.max_running_per_tenant running runs per tenant
//     and Workflow.MaxConcurrent per workflow type (all tenants); excess runs
//     stay queued and are retried.
//   - Credentials: workflows read them with Run.Secret from secret references
//     declared at registration; values are never stored in run data and are
//     redacted from recorded step errors.
package agent

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pillar/gostack/adapters/store"
	"github.com/pillar/gostack/core/config"
	"github.com/pillar/gostack/core/httpx"
	"github.com/pillar/gostack/modules/jobs"
	"github.com/pillar/gostack/modules/storage"
)

// JobType is the jobs queue type that executes runs.
const JobType = "agent.run"

// Run statuses.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusWaiting   = "waiting"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
	StatusTimedOut  = "timed_out"
)

// Step kinds and statuses.
const (
	KindLLM  = "llm"
	KindTool = "tool"
	KindWait = "wait"

	StepRunning             = "running"
	StepSucceeded           = "succeeded"
	StepFailed              = "failed"
	StepWaiting             = "waiting"
	StepNeedsReconciliation = "needs_reconciliation"
)

// Error codes recorded on runs (error_code).
const (
	CodeBudgetExceeded   = "budget_exceeded"
	CodeTimedOut         = "timed_out"
	CodeCancelled        = "cancelled"
	CodeWorkflowError    = "workflow_error"
	CodeNondeterministic = "nondeterministic_workflow"
	CodeUnknownWorkflow  = "unknown_workflow"
	CodeOutputTooLarge   = "output_too_large"
)

const (
	maxInputBytes  = 64 << 10
	maxOutputBytes = 60 << 10
	maxErrorBytes  = 1000
	jobAttempts    = 10
	retryBusy      = 2 * time.Second
)

// Workflow is a registered run type.
type Workflow struct {
	// Func performs the run through Run.Step / Run.Tool / Run.Sleep and must
	// return the errors those return (they carry stop/yield signals).
	Func func(ctx context.Context, r *Run) error
	// MaxConcurrent bounds running runs of this workflow across all tenants (0 = unbounded).
	MaxConcurrent int
	// Secrets maps names used with Run.Secret to secret references (env:NAME
	// or file:/path). Values are resolved at call time and never persisted.
	Secrets map[string]string
}

// Kit is AgentKit. Set the fields, then Register workflows before serving.
type Kit struct {
	DB   *store.DB
	Jobs *jobs.Queue
	// Storage holds step outputs larger than Cfg.InlineOutputBytes (optional).
	Storage storage.Storage
	Cfg     config.AgentConfig
	// Lease is queue.lease: a run yields to a continuation job before its
	// job lease could run out.
	Lease time.Duration
	Log   *slog.Logger
	Now   func() time.Time

	mu        sync.RWMutex
	workflows map[string]Workflow
}

func (k *Kit) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

func (k *Kit) log() *slog.Logger {
	if k.Log != nil {
		return k.Log
	}
	return slog.Default()
}

// Register adds or replaces a workflow type. name: 1-64 chars [a-z0-9_.-].
func (k *Kit) Register(name string, w Workflow) error {
	if !validName(name) {
		return fmt.Errorf("agent: invalid workflow name %q", name)
	}
	if w.Func == nil {
		return errors.New("agent: workflow Func is required")
	}
	for n, ref := range w.Secrets {
		if !config.IsSecretRef(ref) {
			return fmt.Errorf("agent: workflow %s secret %s must be env:NAME or file:/path", name, n)
		}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.workflows == nil {
		k.workflows = map[string]Workflow{}
	}
	k.workflows[name] = w
	return nil
}

func (k *Kit) workflow(name string) (Workflow, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	w, ok := k.workflows[name]
	return w, ok
}

// Workflows lists registered workflow names.
func (k *Kit) Workflows() []string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := make([]string, 0, len(k.workflows))
	for n := range k.workflows {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func validName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

// RunInfo is the queryable state of a run.
type RunInfo struct {
	ID           string          `json:"id"`
	Workflow     string          `json:"workflow"`
	Status       string          `json:"status"`
	WaitReason   string          `json:"wait_reason,omitempty"`
	Input        json.RawMessage `json:"input"`
	Output       json.RawMessage `json:"output,omitempty"`
	ErrorCode    string          `json:"error_code,omitempty"`
	Error        string          `json:"error,omitempty"`
	MaxSteps     int64           `json:"max_steps"`
	MaxCostUnits int64           `json:"max_cost_units"`
	StepsUsed    int64           `json:"steps_used"`
	CostUnits    int64           `json:"cost_units"`
	DeadlineAt   int64           `json:"deadline_at"`
	CancelReq    bool            `json:"cancel_requested"`
	CreatedAt    int64           `json:"created_at"`
	UpdatedAt    int64           `json:"updated_at"`
	FinishedAt   *int64          `json:"finished_at,omitempty"`
	Steps        []StepInfo      `json:"steps,omitempty"`
}

// StepInfo describes one step (outputs are not exposed over the API).
type StepInfo struct {
	Seq         int64  `json:"seq"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	Attempt     int64  `json:"attempt"`
	Tool        string `json:"tool,omitempty"`
	Idempotent  bool   `json:"idempotent,omitempty"`
	CostUnits   int64  `json:"cost_units"`
	OutputRef   string `json:"output_ref,omitempty"`
	OutputBytes int64  `json:"output_bytes"`
	WakeAt      *int64 `json:"wake_at,omitempty"`
	Error       string `json:"error,omitempty"`
	StartedAt   int64  `json:"started_at"`
	FinishedAt  *int64 `json:"finished_at,omitempty"`
}

// ToolCallInfo is one append-only audit row.
type ToolCallInfo struct {
	ID             string `json:"id"`
	Seq            int64  `json:"seq"`
	Attempt        int64  `json:"attempt"`
	Tool           string `json:"tool"`
	IdempotencyKey string `json:"idempotency_key"`
	RequestHash    string `json:"request_hash"`
	Outcome        string `json:"outcome"`
	ExternalRef    string `json:"external_ref,omitempty"`
	Error          string `json:"error,omitempty"`
	CreatedAt      int64  `json:"created_at"`
}

// CreateRequest starts a run.
type CreateRequest struct {
	Workflow     string          `json:"workflow"`
	Input        json.RawMessage `json:"input"`
	MaxSteps     int64           `json:"max_steps"`
	MaxCostUnits int64           `json:"max_cost_units"`
	// TimeoutSeconds sets the deadline (0 = agent.default_timeout).
	TimeoutSeconds int64 `json:"timeout_seconds"`
}

type jobPayload struct {
	RunID string `json:"run_id"`
}

func terminal(s string) bool {
	return s == StatusSucceeded || s == StatusFailed || s == StatusCancelled || s == StatusTimedOut
}

// Create inserts a queued run and its job in one transaction.
func (k *Kit) Create(ctx context.Context, tenant string, req CreateRequest) (RunInfo, error) {
	var info RunInfo
	err := k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		info, err = k.CreateTx(ctx, tx, tenant, req)
		return err
	})
	return info, err
}

// CreateTx is Create inside the caller's transaction.
func (k *Kit) CreateTx(ctx context.Context, tx *sql.Tx, tenant string, req CreateRequest) (RunInfo, error) {
	if tenant == "" {
		return RunInfo{}, httpx.BadRequest("tenant required")
	}
	if _, ok := k.workflow(req.Workflow); !ok {
		return RunInfo{}, httpx.BadRequest(fmt.Sprintf("workflow %q is not registered", req.Workflow))
	}
	if len(req.Input) == 0 {
		req.Input = json.RawMessage(`{}`)
	}
	if len(req.Input) > maxInputBytes || !json.Valid(req.Input) {
		return RunInfo{}, httpx.BadRequest("input must be valid JSON of at most 64 KiB")
	}
	if req.MaxSteps == 0 {
		req.MaxSteps = int64(k.Cfg.DefaultMaxSteps)
	}
	if req.MaxSteps < 1 || req.MaxSteps > int64(k.Cfg.MaxSteps) {
		return RunInfo{}, httpx.BadRequest(fmt.Sprintf("max_steps must be within [1,%d]", k.Cfg.MaxSteps))
	}
	if req.MaxCostUnits < 0 {
		return RunInfo{}, httpx.BadRequest("max_cost_units must be >= 0 (0 = unlimited)")
	}
	timeout := k.Cfg.DefaultTimeout
	if req.TimeoutSeconds != 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}
	if timeout < time.Second || timeout > k.Cfg.MaxTimeout {
		return RunInfo{}, httpx.BadRequest(fmt.Sprintf("timeout_seconds must be within [1,%d]", int64(k.Cfg.MaxTimeout/time.Second)))
	}
	now := k.now().UnixMilli()
	id := httpx.RandomID("run_")
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_runs(tenant_id,id,workflow,status,input,max_steps,max_cost_units,deadline_at,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, tenant, id, req.Workflow, StatusQueued, []byte(req.Input), req.MaxSteps, req.MaxCostUnits,
		now+timeout.Milliseconds(), now, now); err != nil {
		return RunInfo{}, err
	}
	if err := k.enqueue(ctx, tx, tenant, id, 0, time.Time{}); err != nil {
		return RunInfo{}, err
	}
	return k.getTx(ctx, tx, tenant, id)
}

func (k *Kit) enqueue(ctx context.Context, tx *sql.Tx, tenant, id string, gen int64, at time.Time) error {
	b, _ := json.Marshal(jobPayload{RunID: id})
	_, err := k.Jobs.Enqueue(ctx, tx, jobs.Job{TenantID: tenant, Type: JobType, Payload: b, RunAt: at,
		MaxAttempts: jobAttempts, DedupeKey: fmt.Sprintf("%s:%d", id, gen)})
	return err
}

const runCols = `id,workflow,status,wait_reason,input,output,error_code,error,max_steps,max_cost_units,steps_used,cost_units,deadline_at,cancel_requested,created_at,updated_at,finished_at`

type scanner interface{ Scan(...any) error }

func scanRun(s scanner) (RunInfo, error) {
	var r RunInfo
	var in, out []byte
	var cancel int64
	err := s.Scan(&r.ID, &r.Workflow, &r.Status, &r.WaitReason, &in, &out, &r.ErrorCode, &r.Error, &r.MaxSteps, &r.MaxCostUnits,
		&r.StepsUsed, &r.CostUnits, &r.DeadlineAt, &cancel, &r.CreatedAt, &r.UpdatedAt, &r.FinishedAt)
	r.Input, r.CancelReq = json.RawMessage(in), cancel != 0
	if out != nil {
		r.Output = json.RawMessage(out)
	}
	return r, err
}

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (k *Kit) getTx(ctx context.Context, q querier, tenant, id string) (RunInfo, error) {
	r, err := scanRun(q.QueryRowContext(ctx, `SELECT `+runCols+` FROM agent_runs WHERE tenant_id=? AND id=?`, tenant, id))
	if errors.Is(err, sql.ErrNoRows) {
		return RunInfo{}, httpx.NotFound("run not found")
	}
	return r, err
}

// Get returns a run with its steps.
func (k *Kit) Get(ctx context.Context, tenant, id string) (RunInfo, error) {
	r, err := k.getTx(ctx, k.DB.Read, tenant, id)
	if err != nil {
		return r, err
	}
	r.Steps, err = k.steps(ctx, k.DB.Read, tenant, id)
	return r, err
}

func (k *Kit) steps(ctx context.Context, q querier, tenant, id string) ([]StepInfo, error) {
	rows, err := q.QueryContext(ctx, `SELECT seq,name,kind,status,attempt,tool,idempotent,cost_units,output_ref,output_bytes,wake_at,error,started_at,finished_at
		FROM agent_steps WHERE tenant_id=? AND run_id=? ORDER BY seq`, tenant, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []StepInfo{}
	for rows.Next() {
		var s StepInfo
		var idem int64
		if err := rows.Scan(&s.Seq, &s.Name, &s.Kind, &s.Status, &s.Attempt, &s.Tool, &idem, &s.CostUnits, &s.OutputRef, &s.OutputBytes,
			&s.WakeAt, &s.Error, &s.StartedAt, &s.FinishedAt); err != nil {
			return nil, err
		}
		s.Idempotent = idem != 0
		out = append(out, s)
	}
	return out, rows.Err()
}

// List returns runs newest first. before is a run id cursor (exclusive).
func (k *Kit) List(ctx context.Context, tenant, status, before string, limit int) ([]RunInfo, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := `SELECT ` + runCols + ` FROM agent_runs WHERE tenant_id=?`
	args := []any{tenant}
	if status != "" {
		q += ` AND status=?`
		args = append(args, status)
	}
	if before != "" {
		c, err := k.getTx(ctx, k.DB.Read, tenant, before)
		if err != nil {
			return nil, httpx.BadRequest("unknown cursor")
		}
		q += ` AND (created_at<? OR (created_at=? AND id<?))`
		args = append(args, c.CreatedAt, c.CreatedAt, c.ID)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := k.DB.Read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunInfo{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Audit returns the tool-call audit log of a run (append-only).
func (k *Kit) Audit(ctx context.Context, tenant, id string) ([]ToolCallInfo, error) {
	if _, err := k.getTx(ctx, k.DB.Read, tenant, id); err != nil {
		return nil, err
	}
	rows, err := k.DB.Read.QueryContext(ctx, `SELECT id,seq,attempt,tool,idempotency_key,request_hash,outcome,external_ref,error,created_at
		FROM agent_tool_calls WHERE tenant_id=? AND run_id=? ORDER BY n`, tenant, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ToolCallInfo{}
	for rows.Next() {
		var c ToolCallInfo
		if err := rows.Scan(&c.ID, &c.Seq, &c.Attempt, &c.Tool, &c.IdempotencyKey, &c.RequestHash, &c.Outcome, &c.ExternalRef, &c.Error, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Cancel cancels a queued/waiting run at once, or asks a running run to stop
// (cooperatively, between steps or by cancelling the step context).
// Cancelling a cancelled run is a no-op; other finished runs yield 409.
func (k *Kit) Cancel(ctx context.Context, tenant, id string) (RunInfo, error) {
	var info RunInfo
	err := k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		r, err := k.getTx(ctx, tx, tenant, id)
		if err != nil {
			return err
		}
		now := k.now().UnixMilli()
		switch {
		case r.Status == StatusCancelled:
		case terminal(r.Status):
			return httpx.Conflict("invalid_state", "run already finished with status "+r.Status)
		case r.Status == StatusRunning:
			if _, err := tx.ExecContext(ctx, `UPDATE agent_runs SET cancel_requested=1, updated_at=? WHERE tenant_id=? AND id=?`, now, tenant, id); err != nil {
				return err
			}
		default:
			if _, err := tx.ExecContext(ctx, `UPDATE agent_runs SET status=?, cancel_requested=1, error_code=?, error='cancelled by request',
				wait_reason='', owner_token='', finished_at=?, updated_at=? WHERE tenant_id=? AND id=?`,
				StatusCancelled, CodeCancelled, now, now, tenant, id); err != nil {
				return err
			}
		}
		info, err = k.getTx(ctx, tx, tenant, id)
		return err
	})
	return info, err
}

// Resolve records an operator decision for a step in needs_reconciliation
// (outcome succeeded with optional output, or failed) and requeues the run.
func (k *Kit) Resolve(ctx context.Context, tenant, id string, seq int64, outcome string, output []byte, externalRef, note string) (RunInfo, error) {
	if outcome != StepSucceeded && outcome != StepFailed {
		return RunInfo{}, httpx.BadRequest("outcome must be succeeded or failed")
	}
	if strings.TrimSpace(note) == "" || len(note) > maxErrorBytes || len(externalRef) > 256 {
		return RunInfo{}, httpx.BadRequest("note (1-1000 bytes) is required; external_ref at most 256 bytes")
	}
	if len(output) > k.Cfg.InlineOutputBytes {
		return RunInfo{}, httpx.BadRequest("output larger than agent.inline_output_bytes")
	}
	var info RunInfo
	err := k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		r, err := k.getTx(ctx, tx, tenant, id)
		if err != nil {
			return err
		}
		var status, tool string
		var attempt int64
		err = tx.QueryRowContext(ctx, `SELECT status, tool, attempt FROM agent_steps WHERE tenant_id=? AND run_id=? AND seq=?`, tenant, id, seq).
			Scan(&status, &tool, &attempt)
		if errors.Is(err, sql.ErrNoRows) {
			return httpx.NotFound("step not found")
		}
		if err != nil {
			return err
		}
		if status != StepNeedsReconciliation {
			return httpx.Conflict("invalid_state", "step is "+status+", want needs_reconciliation")
		}
		now := k.now().UnixMilli()
		errText := ""
		if outcome == StepFailed {
			errText = "resolved as failed: " + note
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agent_steps SET status=?, error=?, output_bytes=?, finished_at=? WHERE tenant_id=? AND run_id=? AND seq=?`,
			outcome, truncate(errText), len(output), now, tenant, id, seq); err != nil {
			return err
		}
		if outcome == StepSucceeded {
			if output == nil {
				output = []byte{}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO agent_checkpoints(tenant_id,run_id,seq,state,created_at) VALUES(?,?,?,?,?)`,
				tenant, id, seq, output, now); err != nil {
				return err
			}
		}
		if err := audit(ctx, tx, tenant, id, seq, attempt, tool, idemKey(id, seq), "", "resolved_"+outcome, externalRef, note, now); err != nil {
			return err
		}
		if r.Status == StatusWaiting && r.WaitReason == "reconciliation" {
			var gen int64
			if err := tx.QueryRowContext(ctx, `UPDATE agent_runs SET status=?, wait_reason='', generation=generation+1, updated_at=?
				WHERE tenant_id=? AND id=? RETURNING generation`, StatusQueued, now, tenant, id).Scan(&gen); err != nil {
				return err
			}
			if err := k.enqueue(ctx, tx, tenant, id, gen, time.Time{}); err != nil {
				return err
			}
		}
		info, err = k.getTx(ctx, tx, tenant, id)
		return err
	})
	return info, err
}

func idemKey(runID string, seq int64) string { return fmt.Sprintf("%s:%d", runID, seq) }

func audit(ctx context.Context, tx *sql.Tx, tenant, run string, seq, attempt int64, tool, key, reqHash, outcome, ref, errText string, now int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO agent_tool_calls(id,tenant_id,run_id,seq,n,attempt,tool,idempotency_key,request_hash,outcome,external_ref,error,created_at)
		VALUES(?,?,?,?,(SELECT coalesce(max(n),0)+1 FROM agent_tool_calls WHERE tenant_id=? AND run_id=?),?,?,?,?,?,?,?,?)`,
		httpx.RandomID("tc_"), tenant, run, seq, tenant, run, attempt, tool, key, reqHash, outcome, ref, truncate(errText), now)
	return err
}

func truncate(s string) string {
	if len(s) > maxErrorBytes {
		return s[:maxErrorBytes]
	}
	return s
}

func hashRequest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Maintain times out runs whose deadline passed while nobody executes them:
// queued/waiting runs, and running runs abandoned for longer than two leases
// (their job died). Fencing (owner_token cleared) stops a late worker.
func (k *Kit) Maintain(ctx context.Context) error {
	now := k.now().UnixMilli()
	stale := now - 2*k.Lease.Milliseconds()
	return k.DB.WriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE agent_runs SET status=?, error_code=?, error='run deadline exceeded', owner_token='',
			wait_reason='', finished_at=?, updated_at=?
			WHERE ((status IN ('queued','waiting') AND deadline_at<=?) OR (status='running' AND deadline_at<=? AND updated_at<=?))`,
			StatusTimedOut, CodeTimedOut, now, now, now, now, stale)
		return err
	})
}
