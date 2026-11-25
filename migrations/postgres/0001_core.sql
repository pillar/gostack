-- PostgreSQL twin of migrations/sqlite/0001_core.sql: same tables, columns, keys,
-- constraints and indexes (INTEGER -> BIGINT, BLOB -> BYTEA). Keep in sync;
-- adapters/postgres schema tests compare both schemas.
-- Core: API keys, durable jobs, idempotency records.
-- Conventions: ids are prefixed random strings, times are UTC unix milliseconds,
-- money and quantities are BIGINT.

CREATE TABLE api_keys (
  id          TEXT PRIMARY KEY,
  tenant_id   TEXT NOT NULL DEFAULT 'default',
  name        TEXT NOT NULL,
  key_hash    TEXT NOT NULL UNIQUE,      -- sha256 hex of the full secret
  scopes      TEXT NOT NULL,             -- space separated
  created_at  BIGINT NOT NULL,
  expires_at  BIGINT,                   -- NULL = no expiry
  revoked_at  BIGINT
);

CREATE TABLE jobs (
  id            TEXT PRIMARY KEY,
  tenant_id     TEXT NOT NULL DEFAULT 'default',
  type          TEXT NOT NULL,
  payload       BYTEA NOT NULL,
  status        TEXT NOT NULL CHECK (status IN ('pending','running','succeeded','retry_wait','dead')),
  run_at        BIGINT NOT NULL,
  attempts      BIGINT NOT NULL DEFAULT 0,
  max_attempts  BIGINT NOT NULL CHECK (max_attempts > 0),
  lease_until   BIGINT,
  lease_token   TEXT,
  dedupe_key    TEXT,
  last_error    TEXT,
  created_at    BIGINT NOT NULL,
  updated_at    BIGINT NOT NULL,
  CHECK (length(payload) <= 65536)
);
CREATE UNIQUE INDEX jobs_dedupe ON jobs(tenant_id, type, dedupe_key) WHERE dedupe_key IS NOT NULL;
CREATE INDEX jobs_ready ON jobs(status, run_at);
CREATE INDEX jobs_lease ON jobs(status, lease_until) WHERE status = 'running';

CREATE TABLE idempotency_records (
  scope          TEXT NOT NULL,          -- e.g. game:player
  key            TEXT NOT NULL,
  op             TEXT NOT NULL,
  request_hash   TEXT NOT NULL,
  response_code  BIGINT NOT NULL,
  response_body  BYTEA NOT NULL,
  created_at     BIGINT NOT NULL,
  expires_at     BIGINT NOT NULL,
  PRIMARY KEY (scope, key)
);
CREATE INDEX idempotency_expiry ON idempotency_records(expires_at);
