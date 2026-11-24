-- PostgreSQL twin of migrations/sqlite/0004_webauth.sql: same tables, columns, keys,
-- constraints and indexes (INTEGER -> BIGINT, BLOB -> BYTEA). Keep in sync;
-- adapters/postgres schema tests compare both schemas.
-- Web auth (modules/webauth, opt-in via auth.web.enabled): password users and
-- cookie sessions. Tables exist even when the module is disabled (schema is
-- independent of configuration so enabling it later needs no migration).

CREATE TABLE web_users (
  id             TEXT NOT NULL PRIMARY KEY,
  tenant_id      TEXT NOT NULL,
  email          TEXT NOT NULL,          -- normalized: trimmed, lowercase
  password_hash  TEXT NOT NULL,          -- Argon2id PHC string
  created_at     BIGINT NOT NULL,
  updated_at     BIGINT NOT NULL,
  UNIQUE (tenant_id, email),
  UNIQUE (tenant_id, id),
  CHECK (email = lower(email) AND length(email) BETWEEN 3 AND 254),
  CHECK (password_hash LIKE '$argon2id$%')
);

CREATE TABLE web_sessions (
  id            TEXT NOT NULL PRIMARY KEY,
  tenant_id     TEXT NOT NULL,
  user_id       TEXT NOT NULL,
  token_hash    TEXT NOT NULL UNIQUE,    -- sha256 hex of the cookie token
  csrf_token    TEXT NOT NULL,
  created_at    BIGINT NOT NULL,
  last_seen_at  BIGINT NOT NULL,
  expires_at    BIGINT NOT NULL,
  FOREIGN KEY (tenant_id, user_id) REFERENCES web_users(tenant_id, id) ON DELETE CASCADE
);
CREATE INDEX web_sessions_user ON web_sessions(tenant_id, user_id);
CREATE INDEX web_sessions_expiry ON web_sessions(expires_at);
