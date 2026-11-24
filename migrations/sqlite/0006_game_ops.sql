-- GameKit live ops (G10): announcements, mail, check-in, tasks, events, redeem codes.
-- Every table is scoped by game_id; per-player tables reference players(game_id, player_id).

CREATE TABLE ops_announcements (
  game_id     TEXT NOT NULL REFERENCES games(game_id),
  id          TEXT NOT NULL,
  title       TEXT NOT NULL,
  body        TEXT NOT NULL,
  priority    INTEGER NOT NULL DEFAULT 0,
  starts_at   INTEGER NOT NULL DEFAULT 0,   -- epoch ms, 0 = immediately
  ends_at     INTEGER NOT NULL DEFAULT 0,   -- epoch ms, 0 = open ended
  platforms   TEXT NOT NULL DEFAULT '[]',   -- JSON array, empty = all
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (game_id, id)
) STRICT, WITHOUT ROWID;

-- A mail with target_player NULL is a broadcast stored once; per-player state
-- (read / claimed / deleted) is created lazily in ops_mail_state.
CREATE TABLE ops_mail (
  game_id        TEXT NOT NULL REFERENCES games(game_id),
  mail_id        TEXT NOT NULL,
  target_player  TEXT,
  title          TEXT NOT NULL,
  body           TEXT NOT NULL,
  grants         TEXT NOT NULL DEFAULT '{}', -- JSON {asset: amount}
  created_at     INTEGER NOT NULL,
  expires_at     INTEGER NOT NULL DEFAULT 0, -- 0 = never
  PRIMARY KEY (game_id, mail_id),
  FOREIGN KEY (game_id, target_player) REFERENCES players(game_id, player_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX ops_mail_target ON ops_mail(game_id, target_player, created_at);

CREATE TABLE ops_mail_state (
  game_id     TEXT NOT NULL,
  mail_id     TEXT NOT NULL,
  player_id   TEXT NOT NULL,
  read_at     INTEGER NOT NULL DEFAULT 0,
  claimed_at  INTEGER NOT NULL DEFAULT 0,
  deleted_at  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (game_id, player_id, mail_id),
  FOREIGN KEY (game_id, mail_id) REFERENCES ops_mail(game_id, mail_id) ON DELETE CASCADE,
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
) STRICT, WITHOUT ROWID;

CREATE TABLE ops_checkin_config (
  game_id     TEXT PRIMARY KEY REFERENCES games(game_id),
  cycle       TEXT NOT NULL,                -- JSON [{asset: amount}, ...]
  timezone    TEXT NOT NULL,
  streak_reset INTEGER NOT NULL CHECK (streak_reset IN (0,1)),
  updated_at  INTEGER NOT NULL
) STRICT;

CREATE TABLE ops_checkin_state (
  game_id     TEXT NOT NULL,
  player_id   TEXT NOT NULL,
  streak      INTEGER NOT NULL,
  total       INTEGER NOT NULL,
  last_day    TEXT NOT NULL,                -- YYYY-MM-DD in the configured timezone
  PRIMARY KEY (game_id, player_id),
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
) STRICT, WITHOUT ROWID;

CREATE TABLE ops_events (
  game_id     TEXT NOT NULL REFERENCES games(game_id),
  id          TEXT NOT NULL,
  title       TEXT NOT NULL,
  starts_at   INTEGER NOT NULL,
  ends_at     INTEGER NOT NULL,
  timezone    TEXT NOT NULL,
  metadata    TEXT NOT NULL DEFAULT '{}',
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (game_id, id)
) STRICT, WITHOUT ROWID;

CREATE TABLE ops_tasks (
  game_id     TEXT NOT NULL REFERENCES games(game_id),
  id          TEXT NOT NULL,
  title       TEXT NOT NULL DEFAULT '',
  metric      TEXT NOT NULL,
  target      INTEGER NOT NULL CHECK (target > 0),
  period      TEXT NOT NULL CHECK (period IN ('once','daily','weekly')),
  grants      TEXT NOT NULL,
  timezone    TEXT NOT NULL,
  event_id    TEXT NOT NULL DEFAULT '',     -- optional gate: only while that event is active
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (game_id, id)
) STRICT, WITHOUT ROWID;
CREATE INDEX ops_tasks_metric ON ops_tasks(game_id, metric);

CREATE TABLE ops_task_progress (
  game_id     TEXT NOT NULL,
  player_id   TEXT NOT NULL,
  task_id     TEXT NOT NULL,
  period_key  TEXT NOT NULL,                -- 'once', YYYY-MM-DD or YYYY-Www
  progress    INTEGER NOT NULL,
  claimed_at  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (game_id, player_id, task_id, period_key),
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
) STRICT, WITHOUT ROWID;

CREATE TABLE ops_redeem_batches (
  game_id          TEXT NOT NULL REFERENCES games(game_id),
  batch_id         TEXT NOT NULL,
  label            TEXT NOT NULL DEFAULT '',
  shared           INTEGER NOT NULL CHECK (shared IN (0,1)),
  max_redemptions  INTEGER NOT NULL CHECK (max_redemptions > 0), -- per code
  grants           TEXT NOT NULL,
  expires_at       INTEGER NOT NULL DEFAULT 0,
  disabled         INTEGER NOT NULL DEFAULT 0,
  created_at       INTEGER NOT NULL,
  PRIMARY KEY (game_id, batch_id)
) STRICT, WITHOUT ROWID;

-- Only SHA-256(game_id ":" normalized code) is stored.
CREATE TABLE ops_redeem_codes (
  game_id     TEXT NOT NULL,
  code_hash   TEXT NOT NULL,
  batch_id    TEXT NOT NULL,
  redeemed    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (game_id, code_hash),
  FOREIGN KEY (game_id, batch_id) REFERENCES ops_redeem_batches(game_id, batch_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX ops_redeem_codes_batch ON ops_redeem_codes(game_id, batch_id);

CREATE TABLE ops_redeem_log (
  game_id     TEXT NOT NULL,
  code_hash   TEXT NOT NULL,
  player_id   TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (game_id, code_hash, player_id),
  FOREIGN KEY (game_id, code_hash) REFERENCES ops_redeem_codes(game_id, code_hash),
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
) STRICT, WITHOUT ROWID;
