-- Billing ledger order without rowid (portable to PostgreSQL): seq is the
-- account's version right after the entry's balance change, i.e. 1, 2, 3, ...
-- per account in commit order. Existing rows are numbered in rowid order; the
-- append-only UPDATE trigger is dropped only for this one-time backfill and
-- recreated in the same migration transaction.
ALTER TABLE billing_ledger_entries ADD COLUMN seq INTEGER NOT NULL DEFAULT 0;

DROP TRIGGER billing_ledger_immutable_update;
UPDATE billing_ledger_entries SET seq = (
  SELECT count(*) FROM billing_ledger_entries e
  WHERE e.tenant_id = billing_ledger_entries.tenant_id
    AND e.account_id = billing_ledger_entries.account_id
    AND e.rowid <= billing_ledger_entries.rowid);
CREATE TRIGGER billing_ledger_immutable_update BEFORE UPDATE ON billing_ledger_entries
BEGIN SELECT RAISE(ABORT, 'billing_ledger_entries is append-only'); END;

CREATE UNIQUE INDEX billing_ledger_account_seq ON billing_ledger_entries(tenant_id, account_id, seq);
