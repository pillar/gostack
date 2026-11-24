-- PostgreSQL twin of migrations/sqlite/0007_game_commerce.sql: same tables, columns, keys,
-- constraints and indexes (INTEGER -> BIGINT, BLOB -> BYTEA). Keep in sync;
-- adapters/postgres schema tests compare both schemas.
-- GameKit G11 commerce: IAP orders, external callback inbox, product mapping and
-- ad reward placements. Grants are posted through game_ledger (ApplyLedger) with
-- source keys iap:<provider>:<txn>, refund:<provider>:<txn>[:n] and ad:<provider>:<event>.

-- Products sold for real money and what one unit grants.
CREATE TABLE commerce_products (
  game_id     TEXT NOT NULL REFERENCES games(game_id),
  product_id  TEXT NOT NULL,
  grants      TEXT NOT NULL,            -- JSON {asset: positive int}
  updated_at  BIGINT NOT NULL,
  PRIMARY KEY (game_id, product_id)
);

-- Store product identifiers (e.g. App Store productId) mapped to products.
CREATE TABLE commerce_provider_products (
  game_id              TEXT NOT NULL,
  provider             TEXT NOT NULL,
  provider_product_id  TEXT NOT NULL,
  product_id           TEXT NOT NULL,
  PRIMARY KEY (game_id, provider, provider_product_id),
  FOREIGN KEY (game_id, product_id) REFERENCES commerce_products(game_id, product_id) ON DELETE CASCADE
);

-- Orders. A store transaction (provider, provider_txn_id) is globally unique, so it
-- can be delivered at most once, to at most one player of one game.
-- States: created (token issued) -> paid (verified, no player bound yet) -> delivered
-- -> refunded | refund_shortfall -> refund_compensated. created/paid -> refunded.
CREATE TABLE commerce_orders (
  order_id             TEXT PRIMARY KEY,
  game_id              TEXT NOT NULL REFERENCES games(game_id),
  player_id            TEXT,              -- NULL until bound to a player
  provider             TEXT NOT NULL,
  product_id           TEXT,              -- NULL when the store product is not mapped
  provider_product_id  TEXT,
  app_account_token    TEXT,              -- server-issued purchase token (Apple appAccountToken)
  provider_txn_id      TEXT,
  original_txn_id      TEXT,
  environment          TEXT,
  quantity             BIGINT NOT NULL DEFAULT 1 CHECK (quantity > 0),
  state                TEXT NOT NULL CHECK (state IN ('created','paid','delivered','refunded','refund_compensated','refund_shortfall')),
  grants               TEXT NOT NULL DEFAULT '{}',  -- JSON snapshot actually granted
  outstanding          TEXT NOT NULL DEFAULT '{}',  -- JSON refund amounts not yet recovered
  refund_attempts      BIGINT NOT NULL DEFAULT 0,
  created_at           BIGINT NOT NULL,
  delivered_at         BIGINT,
  refunded_at          BIGINT,
  updated_at           BIGINT NOT NULL,
  UNIQUE (provider, provider_txn_id),
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
);
CREATE INDEX commerce_orders_token ON commerce_orders(game_id, app_account_token);
CREATE INDEX commerce_orders_state ON commerce_orders(game_id, state, updated_at);
CREATE INDEX commerce_orders_player ON commerce_orders(game_id, player_id, created_at);

-- Inbox of verified external callbacks: duplicates are answered from here.
-- processed/ignored are final; failed is retried when the provider re-delivers.
CREATE TABLE commerce_inbox (
  game_id       TEXT NOT NULL REFERENCES games(game_id),
  provider      TEXT NOT NULL,
  event_id      TEXT NOT NULL,
  kind          TEXT NOT NULL,
  payload_hash  TEXT NOT NULL,
  status        TEXT NOT NULL CHECK (status IN ('processed','ignored','failed')),
  error         TEXT NOT NULL DEFAULT '',
  order_id      TEXT,
  received_at   BIGINT NOT NULL,
  updated_at    BIGINT NOT NULL,
  PRIMARY KEY (game_id, provider, event_id)
);
CREATE INDEX commerce_inbox_received ON commerce_inbox(received_at);

-- Rewarded-ad placements.
CREATE TABLE commerce_ad_placements (
  game_id       TEXT NOT NULL REFERENCES games(game_id),
  placement_id  TEXT NOT NULL,
  grants        TEXT NOT NULL,          -- JSON {asset: positive int}
  daily_cap     BIGINT NOT NULL CHECK (daily_cap > 0),
  updated_at    BIGINT NOT NULL,
  PRIMARY KEY (game_id, placement_id)
);

-- Opaque single-use tokens the client hands to the ad SDK (user_id/custom_data);
-- the server-to-server callback is credited to the token's player only.
CREATE TABLE commerce_ad_intents (
  token         TEXT PRIMARY KEY,
  game_id       TEXT NOT NULL,
  player_id     TEXT NOT NULL,
  placement_id  TEXT NOT NULL,
  created_at    BIGINT NOT NULL,
  expires_at    BIGINT NOT NULL,
  used_at       BIGINT,
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
);
CREATE INDEX commerce_ad_intents_player ON commerce_ad_intents(game_id, player_id, used_at, expires_at);
CREATE INDEX commerce_ad_intents_expiry ON commerce_ad_intents(expires_at);

CREATE TABLE commerce_ad_rewards (
  game_id       TEXT NOT NULL,
  provider      TEXT NOT NULL,
  event_id      TEXT NOT NULL,
  player_id     TEXT NOT NULL,
  placement_id  TEXT NOT NULL,
  day           TEXT NOT NULL,          -- UTC date
  seq           BIGINT NOT NULL,       -- 1..daily_cap
  created_at    BIGINT NOT NULL,
  PRIMARY KEY (game_id, provider, event_id),
  UNIQUE (game_id, player_id, placement_id, day, seq),
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
);
