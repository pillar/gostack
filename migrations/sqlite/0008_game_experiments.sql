-- GameKit experiments (G12).
CREATE TABLE exp_experiments (
  game_id            TEXT NOT NULL REFERENCES games(game_id),
  id                 TEXT NOT NULL,
  layer              TEXT NOT NULL,
  status             TEXT NOT NULL CHECK (status IN ('draft','running','stopped')),
  variants           TEXT NOT NULL,          -- JSON [{id, weight, params}]
  control            TEXT NOT NULL,
  traffic_percent    INTEGER NOT NULL CHECK (traffic_percent BETWEEN 0 AND 100),
  platforms          TEXT NOT NULL DEFAULT '[]',
  min_client_version TEXT NOT NULL DEFAULT '',
  slice_start        INTEGER NOT NULL DEFAULT 0, -- layer slice [start, start+traffic_percent*100) of 10000, valid while running
  created_at         INTEGER NOT NULL,
  updated_at         INTEGER NOT NULL,
  PRIMARY KEY (game_id, id)
) STRICT, WITHOUT ROWID;
CREATE INDEX exp_experiments_layer ON exp_experiments(game_id, layer, status);

-- First exposure per (player, experiment); later exposures are ignored.
CREATE TABLE exp_exposures (
  game_id        TEXT NOT NULL,
  experiment_id  TEXT NOT NULL,
  player_id      TEXT NOT NULL,
  variant        TEXT NOT NULL,
  created_at     INTEGER NOT NULL,
  PRIMARY KEY (game_id, experiment_id, player_id),
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
) STRICT, WITHOUT ROWID;
