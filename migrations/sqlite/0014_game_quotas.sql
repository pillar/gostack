-- Per-game resource quotas for multi-game deployments (G-A09, docs/api/quotas.md).
-- game_quotas holds admin overrides; a NULL column inherits the configured
-- default (game.quota_defaults.*); 0 means unlimited.
CREATE TABLE game_quotas (
  game_id                   TEXT PRIMARY KEY REFERENCES games(game_id),
  max_players               INTEGER,
  requests_per_minute       INTEGER,
  save_bytes_total          INTEGER,
  analytics_events_per_day  INTEGER,
  write_concurrency         INTEGER,
  updated_by                TEXT NOT NULL,
  updated_at                INTEGER NOT NULL
) STRICT;

-- game_usage holds the counters enforced in the same write transaction as the
-- change: players created and the byte size of the current save payloads
-- (save_slots.payload; retained revisions are not counted).
CREATE TABLE game_usage (
  game_id     TEXT PRIMARY KEY REFERENCES games(game_id),
  players     INTEGER NOT NULL,
  save_bytes  INTEGER NOT NULL
) STRICT;

INSERT INTO game_usage(game_id, players, save_bytes)
SELECT g.game_id,
       (SELECT count(*) FROM players p WHERE p.game_id = g.game_id),
       (SELECT coalesce(sum(length(s.payload)), 0) FROM save_slots s WHERE s.game_id = g.game_id)
FROM games g;
