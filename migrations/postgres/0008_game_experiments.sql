-- PostgreSQL twin of migrations/sqlite/0008_game_experiments.sql: same tables, columns, keys,
-- constraints and indexes (INTEGER -> BIGINT, BLOB -> BYTEA). Keep in sync;
-- adapters/postgres schema tests compare both schemas.
-- GameKit experiments (G12).
CREATE TABLE exp_experiments (
  game_id            TEXT NOT NULL REFERENCES games(game_id),
  id                 TEXT NOT NULL,
  layer              TEXT NOT NULL,
  status             TEXT NOT NULL CHECK (status IN ('draft','running','stopped')),
  variants           TEXT NOT NULL,          -- JSON [{id, weight, params}]
  control            TEXT NOT NULL,
  traffic_percent    BIGINT NOT NULL CHECK (traffic_percent BETWEEN 0 AND 100),
  platforms          TEXT NOT NULL DEFAULT '[]',
  min_client_version TEXT NOT NULL DEFAULT '',
  slice_start        BIGINT NOT NULL DEFAULT 0, -- layer slice [start, start+traffic_percent*100) of 10000, valid while running
  created_at         BIGINT NOT NULL,
  updated_at         BIGINT NOT NULL,
  PRIMARY KEY (game_id, id)
);
CREATE INDEX exp_experiments_layer ON exp_experiments(game_id, layer, status);

-- First exposure per (player, experiment); later exposures are ignored.
CREATE TABLE exp_exposures (
  game_id        TEXT NOT NULL,
  experiment_id  TEXT NOT NULL,
  player_id      TEXT NOT NULL,
  variant        TEXT NOT NULL,
  created_at     BIGINT NOT NULL,
  PRIMARY KEY (game_id, experiment_id, player_id),
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
);
