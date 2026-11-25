-- WebKit P1 (PRD §24): content publishing with revisions and SEO metadata,
-- an order state machine example on top of the billing module, and a webhook
-- outbox delivered by the jobs queue. Not a CMS or a shop platform.

-- Sites own the public URL space; their id appears in public URLs, so it is
-- globally unique. base_url is used for canonical links and sitemap.xml.
CREATE TABLE web_sites (
  id               TEXT PRIMARY KEY,
  tenant_id        TEXT NOT NULL,
  name             TEXT NOT NULL,
  base_url         TEXT NOT NULL,
  robots_disallow  TEXT NOT NULL DEFAULT '',   -- newline separated path prefixes
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
) STRICT;
CREATE INDEX web_sites_tenant ON web_sites(tenant_id, id);

-- Web session users (modules/webauth) allowed to edit a tenant's content and orders.
CREATE TABLE web_editors (
  tenant_id   TEXT NOT NULL,
  user_id     TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (tenant_id, user_id)
) STRICT, WITHOUT ROWID;

CREATE TABLE web_pages (
  tenant_id           TEXT NOT NULL,
  id                  TEXT NOT NULL,
  site_id             TEXT NOT NULL REFERENCES web_sites(id),
  kind                TEXT NOT NULL CHECK (kind IN ('page','post')),
  slug                TEXT NOT NULL,
  status              TEXT NOT NULL CHECK (status IN ('draft','scheduled','published','archived')),
  draft_revision      INTEGER NOT NULL,
  published_revision  INTEGER NOT NULL DEFAULT 0,   -- 0 = never published
  scheduled_revision  INTEGER NOT NULL DEFAULT 0,
  publish_at          INTEGER,
  schedule_token      TEXT NOT NULL DEFAULT '',
  published_at        INTEGER,
  version             INTEGER NOT NULL DEFAULT 1,
  created_at          INTEGER NOT NULL,
  updated_at          INTEGER NOT NULL,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, site_id, slug)
) STRICT, WITHOUT ROWID;
CREATE INDEX web_pages_site ON web_pages(site_id, status, slug);

CREATE TABLE web_page_revisions (
  tenant_id        TEXT NOT NULL,
  page_id          TEXT NOT NULL,
  rev              INTEGER NOT NULL,
  title            TEXT NOT NULL,
  body             TEXT NOT NULL,
  seo_title        TEXT NOT NULL DEFAULT '',
  seo_description  TEXT NOT NULL DEFAULT '',
  canonical_url    TEXT NOT NULL DEFAULT '',
  og_title         TEXT NOT NULL DEFAULT '',
  og_description   TEXT NOT NULL DEFAULT '',
  og_image         TEXT NOT NULL DEFAULT '',
  noindex          INTEGER NOT NULL DEFAULT 0,
  author           TEXT NOT NULL,
  created_at       INTEGER NOT NULL,
  PRIMARY KEY (tenant_id, page_id, rev),
  FOREIGN KEY (tenant_id, page_id) REFERENCES web_pages(tenant_id, id)
) STRICT, WITHOUT ROWID;
CREATE TRIGGER web_page_revisions_immutable_update BEFORE UPDATE ON web_page_revisions
BEGIN SELECT RAISE(ABORT, 'web_page_revisions is append-only'); END;
CREATE TRIGGER web_page_revisions_immutable_delete BEFORE DELETE ON web_page_revisions
BEGIN SELECT RAISE(ABORT, 'web_page_revisions is append-only'); END;

-- Order state machine example: created -> paid -> fulfilled; created -> cancelled;
-- paid|fulfilled -> refunded. Payment is a billing reservation settled at once.
CREATE TABLE web_orders (
  tenant_id       TEXT NOT NULL,
  id              TEXT NOT NULL,
  status          TEXT NOT NULL CHECK (status IN ('created','paid','fulfilled','cancelled','refunded')),
  account_id      TEXT NOT NULL,
  amount          INTEGER NOT NULL CHECK (amount > 0),
  currency        TEXT NOT NULL,
  customer_ref    TEXT NOT NULL DEFAULT '',
  items           BLOB NOT NULL,
  reservation_id  TEXT NOT NULL DEFAULT '',
  version         INTEGER NOT NULL DEFAULT 1,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  PRIMARY KEY (tenant_id, id)
) STRICT, WITHOUT ROWID;
CREATE INDEX web_orders_list ON web_orders(tenant_id, created_at, id);

-- Append-only event log; every event is also the webhook payload source.
CREATE TABLE web_events (
  id           TEXT PRIMARY KEY,
  tenant_id    TEXT NOT NULL,
  type         TEXT NOT NULL,
  order_id     TEXT NOT NULL,
  order_version INTEGER NOT NULL,                -- order version after the transition (event order)
  from_status  TEXT NOT NULL,
  to_status    TEXT NOT NULL,
  payload      BLOB NOT NULL,
  created_at   INTEGER NOT NULL,
  UNIQUE (tenant_id, order_id, order_version),
  FOREIGN KEY (tenant_id, order_id) REFERENCES web_orders(tenant_id, id)
) STRICT;
CREATE TRIGGER web_events_immutable_update BEFORE UPDATE ON web_events
BEGIN SELECT RAISE(ABORT, 'web_events is append-only'); END;
CREATE TRIGGER web_events_immutable_delete BEFORE DELETE ON web_events
BEGIN SELECT RAISE(ABORT, 'web_events is append-only'); END;

-- Webhook endpoints. secret is the HMAC signing key shown once at creation;
-- it must be stored to sign deliveries.
CREATE TABLE web_webhooks (
  tenant_id   TEXT NOT NULL,
  id          TEXT NOT NULL,
  url         TEXT NOT NULL,
  secret      TEXT NOT NULL,
  events      TEXT NOT NULL DEFAULT '',     -- space separated event types; empty = all
  active      INTEGER NOT NULL DEFAULT 1,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (tenant_id, id)
) STRICT, WITHOUT ROWID;

CREATE TABLE web_webhook_deliveries (
  tenant_id     TEXT NOT NULL,
  event_id      TEXT NOT NULL REFERENCES web_events(id),
  webhook_id    TEXT NOT NULL,
  status        TEXT NOT NULL CHECK (status IN ('pending','delivered','failed')),
  attempts      INTEGER NOT NULL DEFAULT 0,
  last_status   INTEGER NOT NULL DEFAULT 0,
  last_error    TEXT NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  delivered_at  INTEGER,
  PRIMARY KEY (tenant_id, event_id, webhook_id),
  FOREIGN KEY (tenant_id, webhook_id) REFERENCES web_webhooks(tenant_id, id)
) STRICT, WITHOUT ROWID;
CREATE INDEX web_webhook_deliveries_hook ON web_webhook_deliveries(tenant_id, webhook_id, created_at);
