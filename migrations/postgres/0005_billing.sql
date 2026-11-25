-- PostgreSQL twin of migrations/sqlite/0005_billing.sql: same tables, columns, keys,
-- constraints and indexes (INTEGER -> BIGINT, BLOB -> BYTEA). Keep in sync;
-- adapters/postgres schema tests compare both schemas.
-- Optional billing module (PRD §7): accounts, append-only ledger, reservations.
-- Money is BIGINT minor units. Every balance change writes exactly one ledger
-- entry in the same transaction, so for each account
--   available = SUM(delta_available) and reserved = SUM(delta_reserved).
-- Tables are prefixed billing_ to avoid clashing with application tables.

CREATE TABLE billing_accounts (
  tenant_id   TEXT NOT NULL,
  id          TEXT NOT NULL,
  currency    TEXT NOT NULL CHECK (length(currency) BETWEEN 1 AND 16),
  available   BIGINT NOT NULL DEFAULT 0 CHECK (available >= 0),
  reserved    BIGINT NOT NULL DEFAULT 0 CHECK (reserved >= 0),
  version     BIGINT NOT NULL DEFAULT 0,
  created_at  BIGINT NOT NULL,
  updated_at  BIGINT NOT NULL,
  PRIMARY KEY (tenant_id, id)
);

CREATE TABLE billing_reservations (
  tenant_id       TEXT NOT NULL,
  id              TEXT NOT NULL,
  account_id      TEXT NOT NULL,
  amount          BIGINT NOT NULL CHECK (amount > 0),
  settled_amount  BIGINT CHECK (settled_amount IS NULL OR (settled_amount >= 0 AND settled_amount <= amount)),
  status          TEXT NOT NULL CHECK (status IN ('reserved','settled','released','expired','reconciling')),
  expires_at      BIGINT NOT NULL,
  note            TEXT NOT NULL DEFAULT '',
  created_at      BIGINT NOT NULL,
  updated_at      BIGINT NOT NULL,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, account_id) REFERENCES billing_accounts(tenant_id, id)
);
CREATE INDEX billing_reservations_account ON billing_reservations(tenant_id, account_id, status);
CREATE INDEX billing_reservations_expiry ON billing_reservations(expires_at) WHERE status = 'reserved';

CREATE TABLE billing_ledger_entries (
  id               TEXT PRIMARY KEY,
  tenant_id        TEXT NOT NULL,
  account_id       TEXT NOT NULL,
  type             TEXT NOT NULL CHECK (type IN ('credit','debit','reserve','settle','release','expire')),
  amount           BIGINT NOT NULL CHECK (amount >= 0),
  delta_available  BIGINT NOT NULL,
  delta_reserved   BIGINT NOT NULL,
  reservation_id   TEXT,
  idempotency_key  TEXT NOT NULL,
  note             TEXT NOT NULL DEFAULT '',
  created_at       BIGINT NOT NULL,
  UNIQUE (tenant_id, idempotency_key),
  FOREIGN KEY (tenant_id, account_id) REFERENCES billing_accounts(tenant_id, id),
  FOREIGN KEY (tenant_id, reservation_id) REFERENCES billing_reservations(tenant_id, id)
);
CREATE INDEX billing_ledger_account ON billing_ledger_entries(tenant_id, account_id, created_at);

CREATE TRIGGER billing_ledger_immutable_update BEFORE UPDATE ON billing_ledger_entries
  FOR EACH ROW EXECUTE FUNCTION gostack_append_only();
CREATE TRIGGER billing_ledger_immutable_delete BEFORE DELETE ON billing_ledger_entries
  FOR EACH ROW EXECUTE FUNCTION gostack_append_only();
CREATE TRIGGER billing_ledger_immutable_truncate BEFORE TRUNCATE ON billing_ledger_entries
  FOR EACH STATEMENT EXECUTE FUNCTION gostack_append_only();

-- Permanent per-tenant idempotency records for every billing mutation (no TTL:
-- a money operation must never execute twice for the same key).
CREATE TABLE billing_idempotency (
  tenant_id     TEXT NOT NULL,
  key           TEXT NOT NULL,
  op            TEXT NOT NULL,
  request_hash  TEXT NOT NULL,
  response      BYTEA NOT NULL,
  created_at    BIGINT NOT NULL,
  PRIMARY KEY (tenant_id, key)
);
