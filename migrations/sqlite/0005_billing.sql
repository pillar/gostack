-- Optional billing module (PRD §7): accounts, append-only ledger, reservations.
-- Money is INTEGER minor units. Every balance change writes exactly one ledger
-- entry in the same transaction, so for each account
--   available = SUM(delta_available) and reserved = SUM(delta_reserved).
-- Tables are prefixed billing_ to avoid clashing with application tables.

CREATE TABLE billing_accounts (
  tenant_id   TEXT NOT NULL,
  id          TEXT NOT NULL,
  currency    TEXT NOT NULL CHECK (length(currency) BETWEEN 1 AND 16),
  available   INTEGER NOT NULL DEFAULT 0 CHECK (available >= 0),
  reserved    INTEGER NOT NULL DEFAULT 0 CHECK (reserved >= 0),
  version     INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (tenant_id, id)
) STRICT, WITHOUT ROWID;

CREATE TABLE billing_reservations (
  tenant_id       TEXT NOT NULL,
  id              TEXT NOT NULL,
  account_id      TEXT NOT NULL,
  amount          INTEGER NOT NULL CHECK (amount > 0),
  settled_amount  INTEGER CHECK (settled_amount IS NULL OR (settled_amount >= 0 AND settled_amount <= amount)),
  status          TEXT NOT NULL CHECK (status IN ('reserved','settled','released','expired','reconciling')),
  expires_at      INTEGER NOT NULL,
  note            TEXT NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, account_id) REFERENCES billing_accounts(tenant_id, id)
) STRICT, WITHOUT ROWID;
CREATE INDEX billing_reservations_account ON billing_reservations(tenant_id, account_id, status);
CREATE INDEX billing_reservations_expiry ON billing_reservations(expires_at) WHERE status = 'reserved';

CREATE TABLE billing_ledger_entries (
  id               TEXT PRIMARY KEY,
  tenant_id        TEXT NOT NULL,
  account_id       TEXT NOT NULL,
  type             TEXT NOT NULL CHECK (type IN ('credit','debit','reserve','settle','release','expire')),
  amount           INTEGER NOT NULL CHECK (amount >= 0),
  delta_available  INTEGER NOT NULL,
  delta_reserved   INTEGER NOT NULL,
  reservation_id   TEXT,
  idempotency_key  TEXT NOT NULL,
  note             TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL,
  UNIQUE (tenant_id, idempotency_key),
  FOREIGN KEY (tenant_id, account_id) REFERENCES billing_accounts(tenant_id, id),
  FOREIGN KEY (tenant_id, reservation_id) REFERENCES billing_reservations(tenant_id, id)
) STRICT;
CREATE INDEX billing_ledger_account ON billing_ledger_entries(tenant_id, account_id, created_at);

CREATE TRIGGER billing_ledger_immutable_update BEFORE UPDATE ON billing_ledger_entries
BEGIN SELECT RAISE(ABORT, 'billing_ledger_entries is append-only'); END;
CREATE TRIGGER billing_ledger_immutable_delete BEFORE DELETE ON billing_ledger_entries
BEGIN SELECT RAISE(ABORT, 'billing_ledger_entries is append-only'); END;

-- Permanent per-tenant idempotency records for every billing mutation (no TTL:
-- a money operation must never execute twice for the same key).
CREATE TABLE billing_idempotency (
  tenant_id     TEXT NOT NULL,
  key           TEXT NOT NULL,
  op            TEXT NOT NULL,
  request_hash  TEXT NOT NULL,
  response      BLOB NOT NULL,
  created_at    INTEGER NOT NULL,
  PRIMARY KEY (tenant_id, key)
) STRICT, WITHOUT ROWID;
