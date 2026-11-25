-- PostgreSQL twin of migrations/sqlite/0016_commerce_providers.sql.
-- G11 commerce: Google Play and WeChat Pay providers.
-- amount: price in the provider's minor unit (WeChat Pay: fen) fixed on the order
-- before the provider is called; a payment notification must report the same
-- total. 0 = no server-side price (App Store, Google Play).
-- prepay_id: WeChat Pay JSAPI prepay_id stored after the (out-of-transaction)
-- prepay call so a retried prepay returns it instead of calling WeChat again.
ALTER TABLE commerce_orders ADD COLUMN amount BIGINT NOT NULL DEFAULT 0;
ALTER TABLE commerce_orders ADD COLUMN prepay_id TEXT NOT NULL DEFAULT '';
