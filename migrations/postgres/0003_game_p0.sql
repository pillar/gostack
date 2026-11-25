-- PostgreSQL twin of migrations/sqlite/0003_game_p0.sql: same tables, columns, keys,
-- constraints and indexes (INTEGER -> BIGINT, BLOB -> BYTEA). Keep in sync;
-- adapters/postgres schema tests compare both schemas.
-- GameKit P0 gaps: season settlement, offers/purchases, admin adjustments,
-- one-time score runs and quarantine review metadata.

-- One settlement per (game, board, season). status 'running' is resumable: payouts
-- already recorded are skipped, so re-running never pays twice.
CREATE TABLE season_settlements (
  game_id          TEXT NOT NULL REFERENCES games(game_id),
  board_id         TEXT NOT NULL,
  season_id        TEXT NOT NULL,
  status           TEXT NOT NULL CHECK (status IN ('running','completed')),
  config_revision  BIGINT NOT NULL,
  paid_players     BIGINT NOT NULL DEFAULT 0,
  created_at       BIGINT NOT NULL,
  updated_at       BIGINT NOT NULL,
  completed_at     BIGINT,
  PRIMARY KEY (game_id, board_id, season_id)
);

-- Per-player payout of a settlement; the primary key is the double-pay guard
-- (the ledger source key season:<board>:<season>:<player> is the second one).
CREATE TABLE season_payouts (
  game_id     TEXT NOT NULL,
  board_id    TEXT NOT NULL,
  season_id   TEXT NOT NULL,
  player_id   TEXT NOT NULL,
  rank        BIGINT NOT NULL,
  score       BIGINT NOT NULL,
  grants      TEXT NOT NULL,          -- JSON object actually granted (after stack caps)
  created_at  BIGINT NOT NULL,
  PRIMARY KEY (game_id, board_id, season_id, player_id),
  FOREIGN KEY (game_id, board_id, season_id) REFERENCES season_settlements(game_id, board_id, season_id),
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
);
CREATE INDEX season_payouts_rank ON season_payouts(game_id, board_id, season_id, rank);

CREATE TABLE offer_purchases (
  purchase_id      TEXT PRIMARY KEY,
  game_id          TEXT NOT NULL,
  player_id        TEXT NOT NULL,
  offer_id         TEXT NOT NULL,
  period_key       TEXT NOT NULL,       -- 'once', 'unlimited' or UTC date
  seq              BIGINT NOT NULL,
  config_revision  BIGINT NOT NULL,
  created_at       BIGINT NOT NULL,
  UNIQUE (game_id, player_id, offer_id, period_key, seq),
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
);

-- Audit of operator balance adjustments; source_key makes an adjustment idempotent.
CREATE TABLE wallet_adjustments (
  game_id       TEXT NOT NULL,
  source_key    TEXT NOT NULL,
  player_id     TEXT NOT NULL,
  deltas        TEXT NOT NULL,        -- JSON object
  reason        TEXT NOT NULL,
  admin_key_id  TEXT NOT NULL,
  created_at    BIGINT NOT NULL,
  PRIMARY KEY (game_id, source_key),
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
);
CREATE INDEX wallet_adjustments_player ON wallet_adjustments(game_id, player_id, created_at);

-- One-time run tokens (stored as SHA-256) for boards with require_run.
CREATE TABLE score_runs (
  token_hash  TEXT PRIMARY KEY,
  game_id     TEXT NOT NULL,
  board_id    TEXT NOT NULL,
  player_id   TEXT NOT NULL,
  started_at  BIGINT NOT NULL,
  expires_at  BIGINT NOT NULL,
  used_at     BIGINT,
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
);
CREATE INDEX score_runs_expiry ON score_runs(expires_at);

ALTER TABLE score_quarantine ADD COLUMN reviewed_by TEXT;
ALTER TABLE score_quarantine ADD COLUMN reviewed_at BIGINT;
CREATE INDEX score_quarantine_review ON score_quarantine(game_id, review, id);
