-- PostgreSQL twin of migrations/sqlite/0015_realtime_bus.sql (INTEGER -> BIGINT).
-- GameKit G14 realtime bus (game.realtime.bus: postgres, profile scale).
-- realtime_streams is the sequence authority of each player's message stream:
-- every message takes last_seq+1 in the transaction that stores it, so seq is
-- monotonic and contiguous across instances. realtime_messages is the shared
-- replay window (bounded per player by count and age) that lets a client resume
-- on any instance.

CREATE TABLE realtime_streams (
  game_id     TEXT NOT NULL,
  player_id   TEXT NOT NULL,
  epoch       TEXT NOT NULL,
  last_seq    BIGINT NOT NULL CHECK (last_seq >= 0),
  updated_at  BIGINT NOT NULL,
  PRIMARY KEY (game_id, player_id)
);

CREATE TABLE realtime_messages (
  game_id       TEXT NOT NULL,
  player_id     TEXT NOT NULL,
  seq           BIGINT NOT NULL CHECK (seq > 0),
  broadcast_id  TEXT,                 -- set for Broadcast fan-out: one message per player and broadcast
  body          TEXT NOT NULL,        -- the client envelope {"type":..,"seq":..,"data":..}
  created_at    BIGINT NOT NULL,
  PRIMARY KEY (game_id, player_id, seq)
);
CREATE UNIQUE INDEX realtime_messages_broadcast ON realtime_messages(game_id, player_id, broadcast_id) WHERE broadcast_id IS NOT NULL;
CREATE INDEX realtime_messages_created ON realtime_messages(created_at);
