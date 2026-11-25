-- GameKit G14 realtime bus (game.realtime.bus: postgres, profile scale).
-- realtime_streams is the sequence authority of each player's message stream:
-- every message takes last_seq+1 in the transaction that stores it, so seq is
-- monotonic and contiguous across instances. realtime_messages is the shared
-- replay window (bounded per player by count and age) that lets a client resume
-- on any instance. The tables exist on SQLite for schema parity only: profile
-- lite keeps the in-process hub and never writes them.

CREATE TABLE realtime_streams (
  game_id     TEXT NOT NULL,
  player_id   TEXT NOT NULL,
  epoch       TEXT NOT NULL,
  last_seq    INTEGER NOT NULL CHECK (last_seq >= 0),
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (game_id, player_id)
) STRICT, WITHOUT ROWID;

CREATE TABLE realtime_messages (
  game_id       TEXT NOT NULL,
  player_id     TEXT NOT NULL,
  seq           INTEGER NOT NULL CHECK (seq > 0),
  broadcast_id  TEXT,                 -- set for Broadcast fan-out: one message per player and broadcast
  body          TEXT NOT NULL,        -- the client envelope {"type":..,"seq":..,"data":..}
  created_at    INTEGER NOT NULL,
  PRIMARY KEY (game_id, player_id, seq)
) STRICT, WITHOUT ROWID;
CREATE UNIQUE INDEX realtime_messages_broadcast ON realtime_messages(game_id, player_id, broadcast_id) WHERE broadcast_id IS NOT NULL;
CREATE INDEX realtime_messages_created ON realtime_messages(created_at);
