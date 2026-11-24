-- GameKit P0 schema. Every game-scoped table carries game_id; game_id is unique per
-- deployment and belongs to exactly one tenant, so (tenant, game) isolation is
-- enforced by scoping every query on game_id taken from the authenticated session.

CREATE TABLE games (
  game_id          TEXT PRIMARY KEY,
  tenant_id        TEXT NOT NULL DEFAULT 'default',
  name             TEXT NOT NULL,
  status           TEXT NOT NULL CHECK (status IN ('active','disabled')),
  allowed_origins  TEXT NOT NULL DEFAULT '[]',  -- JSON array
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
) STRICT;

CREATE TABLE players (
  game_id     TEXT NOT NULL REFERENCES games(game_id),
  player_id   TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (game_id, player_id)
) STRICT, WITHOUT ROWID;

-- Identities: 'guest' (subject = guest_id, secret_hash set) or a platform (subject = openid).
CREATE TABLE player_identities (
  game_id      TEXT NOT NULL,
  provider     TEXT NOT NULL,
  subject      TEXT NOT NULL,
  player_id    TEXT NOT NULL,
  secret_hash  TEXT,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL,
  PRIMARY KEY (game_id, provider, subject),
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX player_identities_player ON player_identities(game_id, player_id);

CREATE TABLE sessions (
  token_hash  TEXT PRIMARY KEY,
  game_id     TEXT NOT NULL,
  player_id   TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX sessions_player ON sessions(game_id, player_id);
CREATE INDEX sessions_expiry ON sessions(expires_at);

CREATE TABLE save_slots (
  game_id         TEXT NOT NULL,
  player_id       TEXT NOT NULL,
  slot            TEXT NOT NULL,
  revision        INTEGER NOT NULL CHECK (revision > 0),
  schema_version  INTEGER NOT NULL,
  checksum        TEXT NOT NULL,
  payload         BLOB NOT NULL,
  updated_at      INTEGER NOT NULL,
  PRIMARY KEY (game_id, player_id, slot),
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
) STRICT, WITHOUT ROWID;

CREATE TABLE save_revisions (
  game_id         TEXT NOT NULL,
  player_id       TEXT NOT NULL,
  slot            TEXT NOT NULL,
  revision        INTEGER NOT NULL,
  schema_version  INTEGER NOT NULL,
  checksum        TEXT NOT NULL,
  payload         BLOB NOT NULL,
  created_at      INTEGER NOT NULL,
  PRIMARY KEY (game_id, player_id, slot, revision)
) STRICT, WITHOUT ROWID;

CREATE TABLE config_revisions (
  game_id      TEXT NOT NULL REFERENCES games(game_id),
  revision     INTEGER NOT NULL,
  document     TEXT NOT NULL,          -- full JSON document
  published_by TEXT NOT NULL,
  note         TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  PRIMARY KEY (game_id, revision)
) STRICT, WITHOUT ROWID;

-- One row per (player, board, season). status 'quarantined' rows are hidden from ranking.
CREATE TABLE scores (
  game_id      TEXT NOT NULL,
  board_id     TEXT NOT NULL,
  season_id    TEXT NOT NULL,
  player_id    TEXT NOT NULL,
  score        INTEGER NOT NULL,
  achieved_at  INTEGER NOT NULL,
  status       TEXT NOT NULL CHECK (status IN ('accepted','quarantined')),
  updated_at   INTEGER NOT NULL,
  PRIMARY KEY (game_id, board_id, season_id, player_id),
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
) STRICT, WITHOUT ROWID;
-- Ranking index for descending boards uses (score DESC); ascending boards scan the same index backwards
-- only when tie-break order matches, so a dedicated ascending index exists as well.
CREATE INDEX scores_rank_desc ON scores(game_id, board_id, season_id, status, score DESC, achieved_at, player_id);
CREATE INDEX scores_rank_asc  ON scores(game_id, board_id, season_id, status, score ASC, achieved_at, player_id);

-- Quarantined submissions kept for review even when the best accepted score is higher.
CREATE TABLE score_quarantine (
  id           INTEGER PRIMARY KEY,
  game_id      TEXT NOT NULL,
  board_id     TEXT NOT NULL,
  season_id    TEXT NOT NULL,
  player_id    TEXT NOT NULL,
  score        INTEGER NOT NULL,
  reason       TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  review       TEXT NOT NULL DEFAULT 'pending' CHECK (review IN ('pending','approved','rejected'))
) STRICT;
CREATE INDEX score_quarantine_game ON score_quarantine(game_id, review, created_at);

CREATE TABLE wallet_balances (
  game_id     TEXT NOT NULL,
  player_id   TEXT NOT NULL,
  asset       TEXT NOT NULL,
  balance     INTEGER NOT NULL CHECK (balance >= 0),
  version     INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (game_id, player_id, asset),
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
) STRICT, WITHOUT ROWID;

-- Immutable ledger. source_key is unique per game so a source event can only post once per asset.
CREATE TABLE game_ledger (
  id             TEXT PRIMARY KEY,
  game_id        TEXT NOT NULL,
  player_id      TEXT NOT NULL,
  asset          TEXT NOT NULL,
  amount         INTEGER NOT NULL CHECK (amount <> 0),
  balance_after  INTEGER NOT NULL CHECK (balance_after >= 0),
  kind           TEXT NOT NULL,          -- reward | spend | adjust | iap | ad
  source_key     TEXT NOT NULL,
  created_at     INTEGER NOT NULL,
  UNIQUE (game_id, source_key, asset),
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
) STRICT;
CREATE INDEX game_ledger_player ON game_ledger(game_id, player_id, created_at);

CREATE TRIGGER game_ledger_immutable_update BEFORE UPDATE ON game_ledger
BEGIN SELECT RAISE(ABORT, 'game_ledger is append-only'); END;
CREATE TRIGGER game_ledger_immutable_delete BEFORE DELETE ON game_ledger
BEGIN SELECT RAISE(ABORT, 'game_ledger is append-only'); END;

CREATE TABLE reward_claims (
  claim_id         TEXT PRIMARY KEY,
  game_id          TEXT NOT NULL,
  player_id        TEXT NOT NULL,
  reward_id        TEXT NOT NULL,
  period_key       TEXT NOT NULL,       -- 'once' or UTC date
  seq              INTEGER NOT NULL,    -- 1..per_period
  config_revision  INTEGER NOT NULL,
  created_at       INTEGER NOT NULL,
  UNIQUE (game_id, player_id, reward_id, period_key, seq),
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
) STRICT;
