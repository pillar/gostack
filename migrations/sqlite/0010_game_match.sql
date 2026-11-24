-- GameKit G15 matchmaking and rooms.

CREATE TABLE match_queues (
  game_id        TEXT NOT NULL REFERENCES games(game_id),
  queue_id       TEXT NOT NULL,
  size           INTEGER NOT NULL CHECK (size BETWEEN 2 AND 64),
  rating_window  INTEGER NOT NULL CHECK (rating_window >= 0),
  widen_per_sec  INTEGER NOT NULL CHECK (widen_per_sec >= 0),
  max_window     INTEGER NOT NULL CHECK (max_window >= 0),   -- 0 = unbounded
  ttl_sec        INTEGER NOT NULL CHECK (ttl_sec > 0),
  board          TEXT NOT NULL DEFAULT '',                   -- board that receives settlement scores
  updated_at     INTEGER NOT NULL,
  PRIMARY KEY (game_id, queue_id)
) STRICT, WITHOUT ROWID;

-- waiting and matched tickets are "active": a player has at most one.
CREATE TABLE match_tickets (
  ticket_id   TEXT PRIMARY KEY,
  game_id     TEXT NOT NULL,
  queue_id    TEXT NOT NULL,
  player_id   TEXT NOT NULL,
  rating      INTEGER NOT NULL,
  status      TEXT NOT NULL CHECK (status IN ('waiting','matched','cancelled','expired','abandoned','done')),
  room_id     TEXT,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
) STRICT;
CREATE UNIQUE INDEX match_tickets_active ON match_tickets(game_id, player_id) WHERE status IN ('waiting','matched');
CREATE INDEX match_tickets_waiting ON match_tickets(game_id, queue_id, created_at) WHERE status = 'waiting';
CREATE INDEX match_tickets_room ON match_tickets(room_id) WHERE room_id IS NOT NULL;

-- Per-game fencing counter: every claim takes the next value, so tokens only grow.
CREATE TABLE match_epochs (
  game_id  TEXT PRIMARY KEY REFERENCES games(game_id),
  epoch    INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE TABLE match_rooms (
  room_id      TEXT PRIMARY KEY,
  game_id      TEXT NOT NULL REFERENCES games(game_id),
  queue_id     TEXT NOT NULL,
  status       TEXT NOT NULL CHECK (status IN ('open','active','settled','abandoned')),
  epoch        INTEGER NOT NULL DEFAULT 0,        -- fencing token, 0 until claimed
  server_id    TEXT NOT NULL DEFAULT '',
  connection   TEXT NOT NULL DEFAULT '{}',        -- JSON supplied by the battle server at claim
  lease_until  INTEGER NOT NULL,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
) STRICT;
CREATE INDEX match_rooms_open ON match_rooms(status, created_at) WHERE status = 'open';
CREATE INDEX match_rooms_lease ON match_rooms(lease_until) WHERE status IN ('open','active');
CREATE INDEX match_rooms_game ON match_rooms(game_id, status);

CREATE TABLE match_room_members (
  room_id     TEXT NOT NULL REFERENCES match_rooms(room_id),
  game_id     TEXT NOT NULL,
  player_id   TEXT NOT NULL,
  ticket_id   TEXT NOT NULL,
  state       TEXT NOT NULL CHECK (state IN ('assigned','connected','left')),
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (room_id, player_id),
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
) STRICT, WITHOUT ROWID;

-- At most one settlement per room: the PRIMARY KEY makes settlement idempotent.
CREATE TABLE match_settlements (
  room_id       TEXT PRIMARY KEY REFERENCES match_rooms(room_id),
  game_id       TEXT NOT NULL,
  epoch         INTEGER NOT NULL,
  request_hash  TEXT NOT NULL,
  response      TEXT NOT NULL,
  created_at    INTEGER NOT NULL
) STRICT;
