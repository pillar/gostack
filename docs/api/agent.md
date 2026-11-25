# AgentKit API (PRD §19, §24 — P1)

Package `kits/agent`, enabled with `kits: [agent]`. Durable, queryable agent
runs executed on the jobs queue (`modules/jobs`). Migration `0017_agent.sql`
(SQLite and PostgreSQL twins).

AgentKit does **not** provide exactly-once external side effects. Steps run at
least once; tool calls whose outcome is unknown after a crash are parked for
reconciliation instead of being re-sent blindly.

## Go API

Register workflows through `app.Hooks.Workflows` (registered before serving)
or `agent.Kit.Register`:

```go
hooks := app.Hooks{Workflows: map[string]agent.Workflow{
	"summarize": {
		MaxConcurrent: 4,                                       // running runs of this type, all tenants (0 = unbounded)
		Secrets:       map[string]string{"model": "env:MODEL_API_KEY"}, // env:/file: references only
		Func: func(ctx context.Context, r *agent.Run) error {
			key, err := r.Secret("model") // resolved now; never stored; redacted from recorded errors
			if err != nil {
				return err
			}
			draft, err := r.Step(ctx, "draft", func(ctx context.Context) (agent.Output, error) {
				return callModel(ctx, key, r.Input()) // Output{Data, CostUnits}
			})
			if err != nil {
				return err // always return Step/Tool/Sleep errors unchanged (they carry stop signals)
			}
			_, err = r.Tool(ctx, "publish", agent.ToolCall{Tool: "cms", Request: draft, Idempotent: true},
				func(ctx context.Context, idemKey string) (agent.Output, error) {
					return publish(ctx, idemKey, draft) // pass idemKey to the external system
				})
			if err != nil {
				return err
			}
			if err := r.Sleep(ctx, "cooldown", time.Minute); err != nil { // parks the run, no worker held
				return err
			}
			return r.SetOutput(map[string]string{"draft": string(draft)})
		},
	},
}}
```

| Call | Kind | Recovery after a worker dies during the step |
|---|---|---|
| `r.Step(ctx, name, fn)` | `llm` | executed again (`attempt` + 1) |
| `r.Tool(ctx, name, ToolCall{Idempotent: true}, fn)` | `tool` | re-sent with the **same** idempotency key (`<run_id>:<seq>`); audit `retry` |
| `r.Tool(ctx, name, ToolCall{Idempotent: false}, fn)` | `tool` | step → `needs_reconciliation`, run → `waiting` (`wait_reason: reconciliation`), audit `unknown`; nothing is re-sent until an operator resolves it |
| `r.Sleep(ctx, name, d)` | `wait` | run `waiting` (`wait_reason: sleep`), resumed by a delayed job |

**Replay model.** A run is executed by calling its workflow function from the
start in every job. Completed steps return their checkpointed output
(`agent_checkpoints`, or object storage for outputs above
`agent.inline_output_bytes`) without calling `fn`. Workflows must issue the
same step names in the same order; a mismatch fails the run with
`nondeterministic_workflow`. A failed step returns `*agent.StepError` on replay
as well; the workflow decides whether that fails the run.

**Fencing.** The executing job's lease token is stored on the run
(`owner_token`); every write checks it, so a worker whose lease expired (and
whose job another worker re-claimed) cannot change the run.

**Yielding.** A run executes inside one job lease (`queue.lease`). Before a new
step, if `elapsed + agent.step_timeout + 1s > queue.lease`, the run hands over
to a continuation job (same status, new generation) instead of risking lease
expiry. A single step must finish within `agent.step_timeout`
(< `queue.lease`, enforced by config validation).

## Run lifecycle

Statuses: `queued`, `running`, `waiting`, `succeeded`, `failed`, `cancelled`,
`timed_out`. `error_code` on stopped runs: `budget_exceeded`, `timed_out`,
`cancelled`, `workflow_error`, `nondeterministic_workflow`, `unknown_workflow`,
`output_too_large`.

- **Cancellation** (cooperative): queued/waiting runs stop at once; a running
  run gets `cancel_requested` and stops before its next step, or during a step
  by cancellation of the step context (polled every `agent.cancel_poll`). A
  tool step interrupted this way gets audit outcome `unknown`.
- **Timeouts**: the run deadline (`timeout_seconds`, default
  `agent.default_timeout`, max `agent.max_timeout`) bounds every step context;
  hitting it marks the run `timed_out`. `agent.step_timeout` fails the step,
  except for a non-idempotent tool: a call that fails after its step context
  ended (step timeout, or the worker shutting down) may have taken effect, so
  the step becomes `needs_reconciliation`, the run `waiting`
  (`wait_reason: reconciliation`) and the audit outcome `unknown` — the same
  path as a lost lease. Without this, a retry-on-failure workflow would send
  the call again under a new step key. Idempotent tools and `llm` steps
  still fail normally.
  Maintenance times out queued/waiting runs past their deadline and running
  runs abandoned for two leases after their deadline.
- **Budgets**: `max_steps` (checked before each new step) and `max_cost_units`
  (sum of `Output.CostUnits`; checked before each step and after each step) stop
  the run with `budget_exceeded`. A step's cost is known only after it ran, so
  the last step can overshoot `max_cost_units`.
- **Concurrency**: at most `agent.max_running_per_tenant` runs in `running` per
  tenant and `Workflow.MaxConcurrent` per workflow (all tenants). A run over a
  limit stays `queued` (`wait_reason: concurrency`) and is retried 2 s later.
  Waiting runs do not hold a slot.
- **Credentials**: only secret references are configured; values are resolved
  with `Run.Secret` at call time and replaced by `[REDACTED]` in recorded step
  and run errors. Run input/output and step outputs are never logged; avoid
  putting credentials in them.

## HTTP API

All routes need an API key with scope `agent` (`Authorization: Bearer gsk_...`).
The tenant is the key's tenant; other tenants' runs are `404`.

| Route | Purpose |
|---|---|
| `POST /v1/agent/runs` | `{workflow, input, max_steps, max_cost_units, timeout_seconds}` → `201` run (`queued`). Optional `Idempotency-Key` (72 h). |
| `GET /v1/agent/runs?status=&before=&limit=` | Newest first, cursor `next_before`. |
| `GET /v1/agent/runs/{id}` | Run with `steps` (metadata only; outputs are not exposed). |
| `POST /v1/agent/runs/{id}/cancel` | See cancellation; `409 invalid_state` for other finished runs. |
| `GET /v1/agent/runs/{id}/audit` | Append-only tool-call audit (`started`, `succeeded`, `failed`, `unknown`, `retry`, `resolved_*`) with idempotency key, SHA-256 of the request, external reference. |
| `POST /v1/agent/runs/{id}/steps/{seq}/resolve` | `{outcome: succeeded|failed, output, external_ref, note}` for a `needs_reconciliation` step; requeues the run. |

Example:

```bash
curl -XPOST -H "Authorization: Bearer $KEY" -d '{"workflow":"echo","input":{"q":"hi"}}' http://127.0.0.1:8080/v1/agent/runs
curl -H "Authorization: Bearer $KEY" http://127.0.0.1:8080/v1/agent/runs/run_...
```

## Configuration

```yaml
kits: [agent]
agent:
  max_running_per_tenant: 8
  default_timeout: 1h
  max_timeout: 24h
  step_timeout: 20s          # < queue.lease
  default_max_steps: 50
  max_steps: 1000
  inline_output_bytes: 16384 # larger outputs need storage.driver
  cancel_poll: 1s
```

Env overrides: `GOSTACK_AGENT_MAX_RUNNING_PER_TENANT`, `GOSTACK_AGENT_STEP_TIMEOUT`.

## Tables

`agent_runs`, `agent_steps`, `agent_checkpoints` (one row per completed step),
`agent_tool_calls` (append-only: UPDATE/DELETE triggers, TRUNCATE on
PostgreSQL). Finished runs are kept (no retention purge in P1: the audit log
references them).

## Tests (`kits/agent/agent_test.go`, SQLite and PostgreSQL)

Crash mid-step (lease expiry, re-claim) resumes from checkpoints without
re-executing completed steps, and the stale worker is fenced; non-idempotent
tool with unknown outcome → `needs_reconciliation`, resolution resumes without
re-sending; non-idempotent tool hitting `step_timeout` → `needs_reconciliation`
and a retrying workflow sends it once (`review_fixes_test.go`); idempotent
tool re-sent with the same key; cancel (queued and
during a step); run deadline (queued, during a step), step timeout,
maintenance timeout; `max_steps` and `max_cost_units`; per-tenant and
per-workflow concurrency; yielding before lease expiry; sleep + object-storage
outputs; secret redaction; nondeterminism detection; HTTP tenant isolation,
scopes and idempotent create. `app/kits_test.go` runs a hook workflow through
the real job worker.

## Not provided

Exactly-once external effects; workflow versioning (changing a workflow's
step sequence breaks in-flight runs with `nondeterministic_workflow`);
step-level automatic retry policies (the workflow decides); streaming model
output; retention purge of finished runs; per-run priority.
