-- PostgreSQL twin of migrations/sqlite/0017_agent.sql (INTEGER -> BIGINT, BLOB -> BYTEA).
-- AgentKit P1 (PRD §19, §24): durable agent runs executed by the jobs queue.
-- A run is replayed from the start by its workflow function; every completed
-- step returns its checkpointed output instead of executing again.
-- Credentials are never stored here (workflows resolve secret references at
-- call time). Large step outputs live in object storage (output_ref).

CREATE TABLE agent_runs (
  tenant_id        TEXT NOT NULL,
  id               TEXT NOT NULL,
  workflow         TEXT NOT NULL,
  status           TEXT NOT NULL CHECK (status IN ('queued','running','waiting','succeeded','failed','cancelled','timed_out')),
  wait_reason      TEXT NOT NULL DEFAULT '',      -- sleep | reconciliation | concurrency
  input            BYTEA NOT NULL,
  output           BYTEA,
  error_code       TEXT NOT NULL DEFAULT '',
  error            TEXT NOT NULL DEFAULT '',
  max_steps        BIGINT NOT NULL CHECK (max_steps > 0),
  max_cost_units   BIGINT NOT NULL CHECK (max_cost_units >= 0),  -- 0 = unlimited
  steps_used       BIGINT NOT NULL DEFAULT 0,
  cost_units       BIGINT NOT NULL DEFAULT 0,
  deadline_at      BIGINT NOT NULL,
  cancel_requested BIGINT NOT NULL DEFAULT 0,
  owner_token      TEXT NOT NULL DEFAULT '',      -- lease token of the executing job (fencing)
  generation       BIGINT NOT NULL DEFAULT 0,    -- continuation job counter
  created_at       BIGINT NOT NULL,
  updated_at       BIGINT NOT NULL,
  finished_at      BIGINT,
  PRIMARY KEY (tenant_id, id)
);
CREATE INDEX agent_runs_list ON agent_runs(tenant_id, created_at, id);
CREATE INDEX agent_runs_active ON agent_runs(tenant_id, workflow, status);
CREATE INDEX agent_runs_deadline ON agent_runs(status, deadline_at);

CREATE TABLE agent_steps (
  tenant_id    TEXT NOT NULL,
  run_id       TEXT NOT NULL,
  seq          BIGINT NOT NULL,
  name         TEXT NOT NULL,
  kind         TEXT NOT NULL CHECK (kind IN ('llm','tool','wait')),
  status       TEXT NOT NULL CHECK (status IN ('running','succeeded','failed','waiting','needs_reconciliation')),
  attempt      BIGINT NOT NULL,
  tool         TEXT NOT NULL DEFAULT '',
  idempotent   BIGINT NOT NULL DEFAULT 0,
  cost_units   BIGINT NOT NULL DEFAULT 0,
  output_ref   TEXT NOT NULL DEFAULT '',          -- object storage key when the output is large
  output_bytes BIGINT NOT NULL DEFAULT 0,
  wake_at      BIGINT,
  error        TEXT NOT NULL DEFAULT '',
  started_at   BIGINT NOT NULL,
  finished_at  BIGINT,
  PRIMARY KEY (tenant_id, run_id, seq),
  FOREIGN KEY (tenant_id, run_id) REFERENCES agent_runs(tenant_id, id)
);

-- Checkpoint per completed step: the step output (inline when small).
CREATE TABLE agent_checkpoints (
  tenant_id   TEXT NOT NULL,
  run_id      TEXT NOT NULL,
  seq         BIGINT NOT NULL,
  state       BYTEA NOT NULL,
  created_at  BIGINT NOT NULL,
  PRIMARY KEY (tenant_id, run_id, seq),
  FOREIGN KEY (tenant_id, run_id, seq) REFERENCES agent_steps(tenant_id, run_id, seq)
);

-- Append-only audit of external tool calls. One row per event: started is
-- committed before the external call; succeeded/failed after; unknown when a
-- worker died during the call; retry when an idempotent call is re-sent with
-- the same idempotency key; resolved_* for operator reconciliation.
CREATE TABLE agent_tool_calls (
  id               TEXT PRIMARY KEY,
  tenant_id        TEXT NOT NULL,
  run_id           TEXT NOT NULL,
  seq              BIGINT NOT NULL,
  n                BIGINT NOT NULL,              -- order of audit rows within the run
  attempt          BIGINT NOT NULL,
  tool             TEXT NOT NULL,
  idempotency_key  TEXT NOT NULL,
  request_hash     TEXT NOT NULL,
  outcome          TEXT NOT NULL CHECK (outcome IN ('started','succeeded','failed','unknown','retry','resolved_succeeded','resolved_failed')),
  external_ref     TEXT NOT NULL DEFAULT '',
  error            TEXT NOT NULL DEFAULT '',
  created_at       BIGINT NOT NULL,
  UNIQUE (tenant_id, run_id, n),
  FOREIGN KEY (tenant_id, run_id) REFERENCES agent_runs(tenant_id, id)
);

CREATE TRIGGER agent_tool_calls_immutable_update BEFORE UPDATE ON agent_tool_calls
  FOR EACH ROW EXECUTE FUNCTION gostack_append_only();
CREATE TRIGGER agent_tool_calls_immutable_delete BEFORE DELETE ON agent_tool_calls
  FOR EACH ROW EXECUTE FUNCTION gostack_append_only();
CREATE TRIGGER agent_tool_calls_immutable_truncate BEFORE TRUNCATE ON agent_tool_calls
  FOR EACH STATEMENT EXECUTE FUNCTION gostack_append_only();
