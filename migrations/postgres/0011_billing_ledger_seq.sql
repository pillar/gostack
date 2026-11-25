-- PostgreSQL twin of migrations/sqlite/0011_billing_ledger_seq.sql.
-- seq is the account's version right after the entry's balance change (1, 2, 3,
-- ... per account). A PostgreSQL database only receives billing rows after all
-- migrations ran (directly or through `migrate to-postgres`, which copies seq),
-- so the backfill below only matters for databases created by a pre-0011
-- binary; it numbers rows by (created_at, id) with the append-only trigger
-- disabled for this one statement.
ALTER TABLE billing_ledger_entries ADD COLUMN seq BIGINT NOT NULL DEFAULT 0;

ALTER TABLE billing_ledger_entries DISABLE TRIGGER billing_ledger_immutable_update;
UPDATE billing_ledger_entries b SET seq = n.rn
FROM (SELECT id, row_number() OVER (PARTITION BY tenant_id, account_id ORDER BY created_at, id) AS rn
      FROM billing_ledger_entries) n
WHERE b.id = n.id;
ALTER TABLE billing_ledger_entries ENABLE TRIGGER billing_ledger_immutable_update;

CREATE UNIQUE INDEX billing_ledger_account_seq ON billing_ledger_entries(tenant_id, account_id, seq);
