-- Static asset releases (PRD §22, modules/assets): content-addressed objects in
-- object storage, one immutable manifest per release, and a per-game pointer to
-- the current release that is switched only after every object is stored.

CREATE TABLE assets_releases (
  game_id          TEXT NOT NULL REFERENCES games(game_id),
  release_id       TEXT NOT NULL,
  label            TEXT NOT NULL,
  manifest_key     TEXT NOT NULL,
  manifest_size    INTEGER NOT NULL CHECK (manifest_size >= 0),
  manifest_sha256  TEXT NOT NULL,
  entry_path       TEXT NOT NULL,
  entry_key        TEXT NOT NULL,
  entry_size       INTEGER NOT NULL CHECK (entry_size >= 0),
  entry_sha256     TEXT NOT NULL,
  file_count       INTEGER NOT NULL CHECK (file_count >= 0),
  total_bytes      INTEGER NOT NULL CHECK (total_bytes >= 0),
  created_at       INTEGER NOT NULL,
  created_by       TEXT NOT NULL,
  status           TEXT NOT NULL CHECK (status IN ('published','pruned')),
  PRIMARY KEY (game_id, release_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX assets_releases_created ON assets_releases(game_id, created_at);

-- Non-entry files of a release (deleted when the release is pruned).
CREATE TABLE assets_release_files (
  game_id       TEXT NOT NULL,
  release_id    TEXT NOT NULL,
  path          TEXT NOT NULL,
  object_key    TEXT NOT NULL,
  size          INTEGER NOT NULL CHECK (size >= 0),
  sha256        TEXT NOT NULL,
  content_type  TEXT NOT NULL,
  PRIMARY KEY (game_id, release_id, path),
  FOREIGN KEY (game_id, release_id) REFERENCES assets_releases(game_id, release_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX assets_release_files_key ON assets_release_files(game_id, object_key);

-- Every object the publisher stored (assets, manifests, entries), so dedupe
-- and prune never need to list the bucket. 'deleting' = prune in progress.
CREATE TABLE assets_objects (
  game_id     TEXT NOT NULL REFERENCES games(game_id),
  object_key  TEXT NOT NULL,
  sha256      TEXT NOT NULL,
  size        INTEGER NOT NULL CHECK (size >= 0),
  state       TEXT NOT NULL CHECK (state IN ('live','deleting')),
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (game_id, object_key)
) STRICT, WITHOUT ROWID;

-- Current release pointer and the per-game publish/prune lease.
CREATE TABLE assets_state (
  game_id          TEXT PRIMARY KEY REFERENCES games(game_id),
  current_release  TEXT,
  lock_holder      TEXT,
  lock_expires_at  INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL,
  updated_by       TEXT NOT NULL,
  FOREIGN KEY (game_id, current_release) REFERENCES assets_releases(game_id, release_id)
) STRICT, WITHOUT ROWID;
