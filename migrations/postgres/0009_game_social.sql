-- PostgreSQL twin of migrations/sqlite/0009_game_social.sql: same tables, columns, keys,
-- constraints and indexes (INTEGER -> BIGINT, BLOB -> BYTEA). Keep in sync;
-- adapters/postgres schema tests compare both schemas.
-- GameKit G13 social: profiles/privacy, friends, blocks, invites, shares.
-- Every table is scoped by game_id; player references use players(game_id, player_id).

CREATE TABLE social_profiles (
  game_id               TEXT NOT NULL,
  player_id             TEXT NOT NULL,
  display_name          TEXT NOT NULL DEFAULT '',
  friend_code           TEXT NOT NULL,
  searchable            BIGINT NOT NULL DEFAULT 1 CHECK (searchable IN (0,1)),
  friend_requests       TEXT NOT NULL DEFAULT 'everyone' CHECK (friend_requests IN ('everyone','nobody')),
  show_on_friend_boards BIGINT NOT NULL DEFAULT 1 CHECK (show_on_friend_boards IN (0,1)),
  created_at            BIGINT NOT NULL,
  updated_at            BIGINT NOT NULL,
  PRIMARY KEY (game_id, player_id),
  UNIQUE (game_id, friend_code),
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
);

-- Friendship is stored symmetrically (two rows) and always written in one transaction.
CREATE TABLE friendships (
  game_id     TEXT NOT NULL,
  player_id   TEXT NOT NULL,
  friend_id   TEXT NOT NULL,
  created_at  BIGINT NOT NULL,
  PRIMARY KEY (game_id, player_id, friend_id),
  CHECK (player_id <> friend_id),
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id),
  FOREIGN KEY (game_id, friend_id) REFERENCES players(game_id, player_id)
);

-- Pending requests only; accept/decline/cancel delete the row.
CREATE TABLE friend_requests (
  game_id      TEXT NOT NULL,
  from_player  TEXT NOT NULL,
  to_player    TEXT NOT NULL,
  created_at   BIGINT NOT NULL,
  PRIMARY KEY (game_id, from_player, to_player),
  CHECK (from_player <> to_player),
  FOREIGN KEY (game_id, from_player) REFERENCES players(game_id, player_id),
  FOREIGN KEY (game_id, to_player) REFERENCES players(game_id, player_id)
);
CREATE INDEX friend_requests_to ON friend_requests(game_id, to_player, created_at);

CREATE TABLE player_blocks (
  game_id     TEXT NOT NULL,
  player_id   TEXT NOT NULL,
  blocked_id  TEXT NOT NULL,
  created_at  BIGINT NOT NULL,
  PRIMARY KEY (game_id, player_id, blocked_id),
  CHECK (player_id <> blocked_id),
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id),
  FOREIGN KEY (game_id, blocked_id) REFERENCES players(game_id, player_id)
);
CREATE INDEX player_blocks_blocked ON player_blocks(game_id, blocked_id);

CREATE TABLE invite_codes (
  game_id     TEXT NOT NULL,
  code        TEXT NOT NULL,
  inviter_id  TEXT NOT NULL,
  created_at  BIGINT NOT NULL,
  PRIMARY KEY (game_id, code),
  FOREIGN KEY (game_id, inviter_id) REFERENCES players(game_id, player_id)
);
CREATE INDEX invite_codes_inviter ON invite_codes(game_id, inviter_id);

-- A new player is attributed at most once per game (PRIMARY KEY on the invitee).
CREATE TABLE invite_attributions (
  game_id     TEXT NOT NULL,
  invitee_id  TEXT NOT NULL,
  code        TEXT NOT NULL,
  inviter_id  TEXT NOT NULL,
  created_at  BIGINT NOT NULL,
  PRIMARY KEY (game_id, invitee_id),
  FOREIGN KEY (game_id, invitee_id) REFERENCES players(game_id, player_id),
  FOREIGN KEY (game_id, inviter_id) REFERENCES players(game_id, player_id)
);
CREATE INDEX invite_attributions_inviter ON invite_attributions(game_id, inviter_id);

CREATE TABLE shares (
  game_id     TEXT NOT NULL,
  share_id    TEXT NOT NULL,
  player_id   TEXT NOT NULL,
  channel     TEXT NOT NULL,
  payload     TEXT NOT NULL,
  created_at  BIGINT NOT NULL,
  PRIMARY KEY (game_id, share_id),
  FOREIGN KEY (game_id, player_id) REFERENCES players(game_id, player_id)
);
CREATE INDEX shares_player ON shares(game_id, player_id, created_at);

-- One open per (share, player): attribution is unique per opener.
CREATE TABLE share_opens (
  game_id     TEXT NOT NULL,
  share_id    TEXT NOT NULL,
  opener_id   TEXT NOT NULL,
  created_at  BIGINT NOT NULL,
  PRIMARY KEY (game_id, share_id, opener_id),
  FOREIGN KEY (game_id, share_id) REFERENCES shares(game_id, share_id),
  FOREIGN KEY (game_id, opener_id) REFERENCES players(game_id, player_id)
);
