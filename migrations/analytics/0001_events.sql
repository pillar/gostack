-- Analytics lives in its own SQLite file so best-effort event ingestion never
-- competes with the main database write connection.
CREATE TABLE analytics_events (
  event_id        TEXT NOT NULL,
  game_id         TEXT NOT NULL,
  player_id       TEXT,
  session_id      TEXT NOT NULL,
  event_name      TEXT NOT NULL,
  schema_version  INTEGER NOT NULL,
  platform        TEXT NOT NULL,
  client_version  TEXT NOT NULL,
  client_time     INTEGER NOT NULL,
  server_time     INTEGER NOT NULL,
  properties      TEXT NOT NULL,
  PRIMARY KEY (game_id, event_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX analytics_events_time ON analytics_events(game_id, server_time);
