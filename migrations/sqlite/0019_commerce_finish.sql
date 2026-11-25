-- G11 commerce: store-side completion of delivered purchases (Google Play
-- acknowledge/consume) recorded on the order, for reconciliation and admin.
-- finish_state: '' none required or unknown (orders delivered before this
-- migration whose job is gone), 'pending' (finish job enqueued), 'done',
-- 'needs_attention' (the store rejected the call with a non-retryable status;
-- the job keeps retrying). finish_error: last rejection. finished_at: when the
-- store confirmed. purchase_token: the store's purchase handle, so a finish
-- job that is missing can be re-enqueued by reconciliation.
ALTER TABLE commerce_orders ADD COLUMN finish_state TEXT NOT NULL DEFAULT '';
ALTER TABLE commerce_orders ADD COLUMN finish_error TEXT NOT NULL DEFAULT '';
ALTER TABLE commerce_orders ADD COLUMN finished_at INTEGER;
ALTER TABLE commerce_orders ADD COLUMN purchase_token TEXT;
CREATE INDEX commerce_orders_finish ON commerce_orders(finish_state, delivered_at)
  WHERE finish_state IN ('pending', 'needs_attention');
-- WeChat Pay order-query reconciliation scans unpaid orders by provider and age.
CREATE INDEX commerce_orders_unpaid ON commerce_orders(provider, created_at) WHERE state = 'created';

-- Backfill from the finish jobs that still exist (succeeded jobs are purged after 7 days).
UPDATE commerce_orders SET finish_state = 'done'
  WHERE provider = 'google' AND EXISTS (SELECT 1 FROM jobs WHERE jobs.type = 'commerce.google.finish'
    AND jobs.dedupe_key = 'finish:' || commerce_orders.order_id AND jobs.status = 'succeeded');
UPDATE commerce_orders SET finish_state = 'pending'
  WHERE provider = 'google' AND state = 'delivered' AND EXISTS (SELECT 1 FROM jobs WHERE jobs.type = 'commerce.google.finish'
    AND jobs.dedupe_key = 'finish:' || commerce_orders.order_id AND jobs.status <> 'succeeded');
