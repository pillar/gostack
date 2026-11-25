-- PostgreSQL twin of migrations/sqlite/0020_web_pay_generation.sql.
-- WebKit orders: payment generation. A Pay that stalls past the billing
-- reservation TTL finds its reservation expired; the retry releases it and
-- reserves again under billing keys suffixed with the next generation (the
-- old keys would replay the expired reservation forever).
ALTER TABLE web_orders ADD COLUMN pay_generation BIGINT NOT NULL DEFAULT 0;
