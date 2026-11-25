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
  input            BLOB NOT NULL,
  output           BLOB,
  error_code       TEXT NOT NULL DEFAULT '',
  error            TEXT NOT NULL DEFAULT '',
  max_steps        INTEGER NOT NULL CHECK (max_steps > 0),
  max_cost_units   INTEGER NOT NULL CHECK (max_cost_units >= 0),  -- 0 = unlimited
  steps_used       INTEGER NOT NULL DEFAULT 0,
  cost_units       INTEGER NOT NULL DEFAULT 0,
  deadline_at      INTEGER NOT NULL,
  cancel_requested INTEGER NOT NULL DEFAULT 0,
  owner_token      TEXT NOT NULL DEFAULT '',      -- lease token of the executing job (fencing)
  generation       INTEGER NOT NULL DEFAULT 0,    -- continuation job counter
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL,
  finished_at      INTEGER,
  PRIMARY KEY (tenant_id, id)
) STRICT, WITHOUT ROWID;
CREATE INDEX agent_runs_list ON agent_runs(tenant_id, created_at, id);
CREATE INDEX agent_runs_active ON agent_runs(tenant_id, workflow, status);
CREATE INDEX agent_runs_deadline ON agent_runs(status, deadline_at);

CREATE TABLE agent_steps (
  tenant_id    TEXT NOT NULL,
  run_id       TEXT NOT NULL,
  seq          INTEGER NOT NULL,
  name         TEXT NOT NULL,
  kind         TEXT NOT NULL CHECK (kind IN ('llm','tool','wait')),
  status       TEXT NOT NULL CHECK (status IN ('running','succeeded','failed','waiting','needs_reconciliation')),
  attempt      INTEGER NOT NULL,
  tool         TEXT NOT NULL DEFAULT '',
  idempotent   INTEGER NOT NULL DEFAULT 0,
  cost_units   INTEGER NOT NULL DEFAULT 0,
  output_ref   TEXT NOT NULL DEFAULT '',          -- object storage key when the output is large
  output_bytes INTEGER NOT NULL DEFAULT 0,
  wake_at      INTEGER,
  error        TEXT NOT NULL DEFAULT '',
  started_at   INTEGER NOT NULL,
  finished_at  INTEGER,
  PRIMARY KEY (tenant_id, run_id, seq),
  FOREIGN KEY (tenant_id, run_id) REFERENCES agent_runs(tenant_id, id)
) STRICT, WITHOUT ROWID;

-- Checkpoint per completed step: the step output (inline when small).
CREATE TABLE agent_checkpoints (
  tenant_id   TEXT NOT NULL,
  run_id      TEXT NOT NULL,
  seq         INTEGER NOT NULL,
  state       BLOB NOT NULL,
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (tenant_id, run_id, seq),
  FOREIGN KEY (tenant_id, run_id, seq) REFERENCES agent_steps(tenant_id, run_id, seq)
) STRICT, WITHOUT ROWID;

-- Append-only audit of external tool calls. One row per event: started is
-- committed before the external call; succeeded/failed after; unknown when a
-- worker died during the call; retry when an idempotent call is re-sent with
-- the same idempotency key; resolved_* for operator reconciliation.
CREATE TABLE agent_tool_calls (
  id               TEXT PRIMARY KEY,
  tenant_id        TEXT NOT NULL,
  run_id           TEXT NOT NULL,
  seq              INTEGER NOT NULL,
  n                INTEGER NOT NULL,              -- order of audit rows within the run
  attempt          INTEGER NOT NULL,
  tool             TEXT NOT NULL,
  idempotency_key  TEXT NOT NULL,
  request_hash     TEXT NOT NULL,
  outcome          TEXT NOT NULL CHECK (outcome IN ('started','succeeded','failed','unknown','retry','resolved_succeeded','resolved_failed')),
  external_ref     TEXT NOT NULL DEFAULT '',
  error            TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL,
  UNIQUE (tenant_id, run_id, n),
  FOREIGN KEY (tenant_id, run_id) REFERENCES agent_runs(tenant_id, id)
) STRICT;

CREATE TRIGGER agent_tool_calls_immutable_update BEFORE UPDATE ON agent_tool_calls
BEGIN SELECT RAISE(ABORT, 'agent_tool_calls is append-only'); END;
CREATE TRIGGER agent_tool_calls_immutable_delete BEFORE DELETE ON agent_tool_calls
BEGIN SELECT RAISE(ABORT, 'agent_tool_calls is append-only'); END;
