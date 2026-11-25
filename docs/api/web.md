# WebKit API (PRD §24 — P1)

Package `kits/web`, enabled with `kits: [web]`. Migration `0018_web.sql`
(SQLite and PostgreSQL twins). WebKit reuses API keys, web sessions
(`modules/webauth`), the jobs queue and billing. It is deliberately **not** a
CMS or a shop: no editor UI, media library, catalog, cart, tax or payment
provider.

## Authentication

- **Admin routes** (`/v1/web/*` except `/v1/web/public/*`): an API key with
  scope `web` (`Authorization: Bearer gsk_...`, tenant = key tenant), or —
  when `auth.web.enabled` — the web session cookie of a user listed as an
  editor of the session's tenant. Session requests with unsafe methods need
  `X-CSRF-Token` and pass the webauth Origin check. Editors are managed with an
  API key only: `POST /v1/web/editors {user_id}`, `DELETE /v1/web/editors/{user}`.
  Other tenants' sites, pages, orders and webhooks are `404`.
- **Public routes** (`/v1/web/public/{site}/...`): no credentials; published
  content only.

## Content publishing

Sites (`POST /v1/web/sites {id, name, base_url, robots_disallow}`) own a URL
space; the site id is global because it appears in public URLs. Pages
(`kind: page|post`) have a slug unique per tenant and site
(`409 slug_taken`), `/`-separated `[a-z0-9-]` segments.

Every create/update appends an immutable revision (`web_page_revisions`,
append-only triggers) with title, body and SEO metadata (`title`,
`description`, `canonical_url`, `og_title`, `og_description`, `og_image`,
`noindex`). Updates need `expected_version` (`409 revision_conflict`). A
published page keeps serving its published revision until it is published
again.

```
draft | archived ──publish──▶ published         (now)
draft | archived ──publish(at)──▶ scheduled ──job at `at`──▶ published
scheduled ──unschedule──▶ draft      scheduled ──publish──▶ published | rescheduled
published ──publish──▶ published (another revision; same revision = no-op)
draft | scheduled | published ──archive──▶ archived (idempotent)
```

Scheduling enqueues a `web.publish` job (`RunAt = at`) carrying a random
schedule token; unscheduling, rescheduling or archiving changes the token, so
stale jobs do nothing. Scheduling a published page is rejected
(`409 invalid_transition`); archive it first or publish now.

## Public read, SEO, sitemap, robots

| Route | Content |
|---|---|
| `GET /v1/web/public/{site}/pages/{slug...}` | Published page with effective SEO (`title`/`og_title` fall back to the page title, `canonical_url` to `<base_url>/<slug>`); `X-Robots-Tag: noindex` when set. |
| `GET /v1/web/public/{site}/sitemap.xml` | Published, indexable pages (≤ 50 000), `loc` = canonical URL, `lastmod` = publish time. |
| `GET /v1/web/public/{site}/robots.txt` | `User-agent: *`, `Disallow:` lines (or `Allow: /`), `Sitemap: <base_url>/sitemap.xml`. |

All three send `Cache-Control: public, max-age=<web.public_max_age>` and a
strong `ETag` (SHA-256 of the body); `If-None-Match` gets `304`. The site's
front end is expected to serve `/sitemap.xml` and `/robots.txt` by proxying
these endpoints (robots.txt announces the sitemap at `base_url`).

## Order state machine (example; requires `billing.enabled`)

```
created ──pay──▶ paid ──fulfill──▶ fulfilled
created ──cancel──▶ cancelled
paid | fulfilled ──refund──▶ refunded
```

- `POST /v1/web/orders {account_id, amount, customer_ref, items}` needs
  `Idempotency-Key`; the billing account must exist in the tenant and gives the
  currency.
- `pay`: billing `Reserve` then `Settle` with keys derived from the order id
  (`web-order:<id>:reserve|settle`), then `created → paid`. Retrying never
  charges twice; paying a paid/fulfilled order is a no-op.
- `cancel`: `created → cancelled`, then compensation of any payment in flight:
  a held reservation is released, a settled one is refunded by a billing
  credit with the order's refund key. `pay` finding the order cancelled after
  settling runs the same compensation. Repeating `cancel` re-runs it
  idempotently.
- `refund`: `paid|fulfilled → refunded`, then a billing credit with the same
  refund key (`web-order:<id>:refund`), so cancellation and refund credit at
  most once in total.
- Transitions to the current status are no-ops; others are
  `409 invalid_transition`. Every transition appends an event
  (`web_events`, append-only, ordered by order version) in the same
  transaction: `order.created|paid|fulfilled|cancelled|refunded`.
- A `pay` interrupted between reserve and settle for longer than the billing
  reservation TTL (15 min) finds its reservation expired on retry: the expired
  reservation is released (if the sweeper has not expired it yet), the order
  moves to the next payment generation (`web_orders.pay_generation`) and the
  retry reserves and settles again with generation-suffixed keys
  (`web-order:<id>:reserve:<n>`, `...:settle:<n>`). Only a reservation of the
  current generation can be settled, so the order is still charged at most
  once. `pay` answers `409 invalid_transition` "order was cancelled during
  payment" only when the order really was cancelled.

## Webhooks (outbox)

`POST /v1/web/webhooks {url, events}` returns the endpoint with its signing
secret `whsec_...` (shown once; stored because every delivery is signed with
it). `events` empty = all. At most 20 active endpoints per tenant.
`DELETE /v1/web/webhooks/{id}` disables it; `GET .../deliveries` lists
delivery state.

Each event is inserted with one `web_webhook_deliveries` row and one
`web.webhook` job per subscribed endpoint **in the same transaction** as the
order change. The job POSTs the event JSON (`{id, type, created_at, data}`)
outside any transaction with headers:

```
GoStack-Webhook-Id: evt_...                    (same on every retry: deduplicate by it)
GoStack-Webhook-Timestamp: <unix seconds>
GoStack-Webhook-Signature: v1=<hex HMAC-SHA256(secret, timestamp + "." + body)>
```

`web.Verify(secret, header, ts, body, now, tolerance)` checks a signature.
Non-2xx responses and network errors are retried by the queue with
exponential backoff up to `web.webhook_max_attempts`; the last attempt marks
the delivery `failed`. A delivered event is not re-sent by a repeated job;
a crash between the POST and recording it can re-send it (at-least-once).
The delivery client follows no redirects and, unless
`web.webhook_allow_private_networks` (rejected in production), refuses to
connect to any special-purpose address (`web.PublicIP`), checked on the
resolved IP at dial time (DNS rebinding cannot bypass it) and, for literal IP
URLs, already at creation (`400`):

- IPv4: `0.0.0.0/8`, `10/8`, `100.64/10` (CGNAT; Alibaba Cloud metadata
  `100.100.100.200`), `127/8`, `169.254/16`, `172.16/12`, `192.0.0.0/24`,
  `192.0.2.0/24`, `192.168/16`, `198.18/15`, `198.51.100/24`, `203.0.113/24`,
  `224/4`, `240/4` (includes `255.255.255.255`).
- IPv6: anything outside `2000::/3`, plus `2001::/23`, `2001:db8::/32`,
  `64:ff9b:1::/48` (NAT64 local use), `100::/64`, `fc00::/7`, `fe80::/10`,
  `fec0::/10`, `ff00::/8`, `::/96`. IPv4-mapped (`::ffff:0:0/96`), NAT64
  well-known (`64:ff9b::/96`) and 6to4 (`2002::/16`) addresses are judged by
  the IPv4 address they embed.

The default delivery client is built once per Kit and reused (one connection
pool). Production requires https URLs.

## Configuration

```yaml
kits: [web]
web:
  public_max_age: 1m
  webhook_timeout: 10s                   # < queue.lease
  webhook_max_attempts: 8
  webhook_allow_private_networks: false
billing:
  enabled: true                          # order example
```

Env override: `GOSTACK_WEB_PUBLIC_MAX_AGE`.

## Tests (`kits/web/web_test.go`, `kits/web/review_fixes_test.go`, SQLite and PostgreSQL)

Special-purpose address classes refused at dial time and at creation; the
default client is reused (goroutines stay flat over 40 deliveries); `pay`
after the reservation expired (swept and not yet swept) completes with one
charge; `publish` re-resolves the current draft when PostgreSQL re-runs its
transaction after a concurrent edit (PostgreSQL only).
Content lifecycle with revisions, optimistic versions, slug uniqueness,
invalid transitions, ETag/304; scheduled publish via jobs including stale jobs
after unschedule/reschedule; sitemap (noindex excluded, canonical URLs) and
robots; order state machine with idempotent transitions and balances;
compensation after an interrupted pay (refund exactly once, held reservation
released); webhook signing, retry until success with the same event id, no
re-send by a duplicate job, SSRF guard and final failure; tenant isolation
across pages/orders, scope checks, editor sessions with CSRF.
`app/kits_test.go` runs a scheduled publish through the real job worker.

## Not provided

CMS editor UI, rich text processing or HTML sanitisation (the body is stored
and returned as given), media/asset management, i18n, page trees/menus,
search, comments; product catalog, carts, taxes, shipping, real payment
providers; webhook secret rotation; custom domains/TLS.
