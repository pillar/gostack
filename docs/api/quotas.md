# Per-game resource quotas (GameKit, G-A09)

Code: `kits/game/quota.go` (hooks in `kit.go` `withGame`, `identity.go` `createPlayer`, `saves.go`, `analytics.go`), migration `0014_game_quotas`. Tests: `kits/game/quota_test.go`; callback exemption `kits/game/commerce/callback_quota_test.go`.

Purpose: in a deployment that serves several games, one noisy game must not exhaust shared resources (players table, save storage, request budget, analytics pipeline, write queue). Every game has five quotas; `0` means unlimited.

| Quota | Enforced where | Exceeded | Scope |
|---|---|---|---|
| `max_players` | new player creation (guest registration, first platform login), same write transaction as the `players` insert | `403 quota_exceeded` | exact, all instances (database counter) |
| `requests_per_minute` | every `/v1/games/{game}/...` route except CORS preflight and provider callbacks (see below) (token bucket, burst = rate) | `429 rate_limited` + `Retry-After` | global with `rate_limit.driver: valkey`, otherwise per instance |
| `save_bytes_total` | save `PUT`, `restore`, `DELETE`, same write transaction | `403 quota_exceeded` (only when a write grows the total) | exact, all instances (database counter) |
| `analytics_events_per_day` | `POST /events/batch` | excess events dropped, still `202` | per instance, UTC day (see below) |
| `write_concurrency` | mutating requests (`POST`/`PUT`/`DELETE`/`PATCH`) of the game in flight | `503 overloaded` | per instance |

**Provider callbacks are exempt.** Routes registered with `Kit.HandleCallback`
(commerce: `POST /callbacks/{provider}`, `GET|POST /callbacks/ads/{provider}`)
skip `requests_per_minute` and `write_concurrency`. They are server-to-server
calls authenticated by the provider's signature, and the requests bucket is
shared by all of the game's traffic including unauthenticated requests: junk
sent to any route of the game could otherwise make signed Google RTDN, WeChat
Pay (no client-side fallback) or ad SSV callbacks fail with 429/503. Feature
packages use `Kit.HandleCallback` only for such signed callbacks; the handler
must verify the signature before any database work.

**Decision: unauthenticated requests still count.** For all other routes the
quota is applied before session authentication, so requests that later fail
with 401 consume the game's bucket. Moving the check after authentication for
`Player` routes was considered and not done: public routes (guest login,
config, leaderboards) would still share the bucket, so it would not stop a
flood from starving the game, and every rejected token would cost a session
lookup before being limited. `requests_per_minute` protects other games from a
noisy game; it is not an abuse shield for one game. Per-IP limits (GameKit's
auth limiter, a reverse proxy/WAF) are the defence against floods.

Error details: `403 quota_exceeded` carries `details: {"quota","limit","used","requested"}` (`requested` = the bytes or players the write would add); `429` and `503` carry `details: {"quota","limit"}`.

## Configuration and admin API

Defaults for every game (restart-only; env `GAME_QUOTA_DEFAULTS_<NAME>`):

```yaml
game:
  quota_defaults:
    max_players: 0
    requests_per_minute: 0
    save_bytes_total: 0
    analytics_events_per_day: 0
    write_concurrency: 0
```

Negative values are rejected by config validation.

Per-game overrides (admin API key with scope `admin`, tenant must own the game):

- `GET /v1/admin/games/{game}/quotas` → `{"game_id","limits","overrides","defaults","usage"}`. `limits` are the effective values; `overrides` has `null` for inherited fields; `usage.players` and `usage.save_bytes` are the stored counters (exact, all instances); `usage.analytics_events_today` and `usage.writes_in_flight` are the answering instance's counters.
- `PUT /v1/admin/games/{game}/quotas` with `{"max_players":..,"requests_per_minute":..,"save_bytes_total":..,"analytics_events_per_day":..,"write_concurrency":..}` **replaces** the overrides: an omitted or `null` field inherits the default. Values within `[0, 2^50]`, `requests_per_minute` ≤ 1e9; unknown fields → `400`. Body limit 4 KiB.

Propagation: each instance caches a game's limits for 5 s (`quotaTTL`). A `PUT` applies on the answering instance at once and on other instances within 5 s. A failed refresh keeps the previous limits (logged).

Lowering a quota below current usage does not delete anything: existing players keep logging in, saves can still shrink or be deleted; only growth is refused.

CLI: `gostack game quotas <game_id> [--recount] [--json]` prints limits and stored usage. `--recount` rebuilds `game_usage` from `players` and `save_slots` (use after writing those tables outside GameKit, e.g. bulk imports or `benchmarks/cmd/gsseed`, which bypass the counters).

## Counters and concurrency

`game_usage(game_id, players, save_bytes)` has one row per game (created with the game; existing games are backfilled by the migration). The counter changes in the same write transaction as the business row with a conditional update:

```sql
UPDATE game_usage SET save_bytes = save_bytes + ? WHERE game_id = ? AND save_bytes + ? <= ?
```

Zero rows affected means the quota would be exceeded and the transaction is rolled back (nothing written). Shrinking (negative delta) is unconditional. On SQLite the single writer serialises the updates; on PostgreSQL (SERIALIZABLE) concurrent writers of the same game conflict on the row and `WriteTx` retries them (bounded; exhausted retries surface as `503`). Tested with concurrent saves, restores and deletes on both: the counter equals `sum(length(payload))` and never exceeds the limit.

Cost: on PostgreSQL every player creation and save write of one game updates that game's single counter row, so these writes are serialised per game (other games are unaffected). This is the price of an exact quota; no throughput measurement exists yet.

`save_bytes_total` counts the current payload of each slot (`save_slots.payload`, the bytes of the JSON payload as sent). Retained revisions (`game.save_history`) are not counted; worst-case stored save bytes of a game are about `save_bytes_total × (1 + save_history)`.

A missing `game_usage` row (game inserted outside `CreateGame`) is created on first use from a recount that already includes the current write.

## analytics_events_per_day (approximate, bounded)

Each instance keeps a per-game count for the current UTC day. On the first event of the day for a game it seeds the count from the analytics store (`count(*)` of that game's events with `server_time` since 00:00 UTC, index `analytics_events_time`), so a restart does not grant a fresh allowance. Afterwards it counts locally. Consequences:

- with N instances at most about N × quota events are accepted per day;
- events still in an instance's buffer at another instance's seed time are not seen by that seed;
- duplicates (same `event_id`) count against the quota although the store keeps one copy.

Excess events are dropped before the buffer: the response stays `202 {"accepted","dropped"}` and `dropped` includes quota drops. Counter: `game_quota_events_dropped_total{game}`.

## write_concurrency

A per-process in-flight counter per game wraps mutating requests of the game (all of `withGame`, which every `/v1/games/{game}` route uses, including feature packages registered with `Kit.Handle`). It bounds how many entries of the shared write queue (`database.write_queue`) one game can occupy on this instance, so a burst from one game gets `503` instead of making every game's writes fail with a full queue. It counts requests, not transactions: a mutating request that does network IO before its transaction (platform login) holds its slot for that time. Reads (`GET`/`HEAD`/`OPTIONS`) are not counted. Not covered: the realtime WebSocket gateway, admin routes, jobs and provider callbacks (`Kit.HandleCallback`).

## Metrics

Games are created only by admins, so the set of `game` label values is bounded by the number of games; series are registered only for games the instance has served:

- `game_quota_limit{game,quota}` — 5 series per game (effective limits);
- `game_quota_usage{game,quota}` — 4 series per game: `max_players` (players) and `save_bytes_total` (bytes) as of the last cache refresh (≤ 5 s old while the game has traffic), `write_concurrency` (in flight), `analytics_events_per_day` (this instance, today);
- `game_quota_rejected_total{game,quota}` — rejections by `requests_per_minute` / `write_concurrency`; `game_quota_events_dropped_total{game}`;
- with the Valkey limiter, the request quota also appears as `ratelimit_global_*{limiter="game_quota_rpm_<game>"}` (local: `ratelimit_local_denied_total`).

`403 quota_exceeded` for players and saves is not counted in a metric (it is decided inside a re-runnable transaction).
