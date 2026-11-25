# GameKit live ops API (G10)

Package `kits/game/ops`, migration `0006_game_ops.sql`. Client routes live under `/v1/games/{game}`, admin routes under `/v1/admin/games/{game}` (API key with `admin` scope owning the game). Errors use the standard envelope `{"error":{"code","message"},"request_id"}`. Times in requests are epoch milliseconds or RFC3339 strings (the offset carries the zone); responses use epoch milliseconds. Grants are `{asset: positive integer}` (max 20 assets) and are always paid through the append-only ledger (`kind=reward`) with a unique source key, so no claim can pay twice. Retryable player mutations (claim, check-in, redeem) honour `Idempotency-Key` (replay returns the stored response with `Idempotent-Replayed: true`); without a key a retry gets the "already" error below.

## Grants and the items catalog

Mail attachments, check-in rewards, task rewards and redeem-code grants are
posted through `Kit.Grant`: when the game's configuration has an `items`
catalog, every granted asset must be in it (`409 unknown_asset`) and items may
not exceed `max_stack` (`409 stack_limit`, `details.asset`/`details.max_stack`).
A failing claim is rolled back entirely and can be retried after the player
spends items. Social invite grants follow the same rule.

## Announcements
| Route | Notes |
|---|---|
| `GET /announcements?platform=` | public / optional auth. Active ones (`starts_at <= now < ends_at`, 0 = open), filtered by platform when the announcement lists platforms (a client that sends no platform only sees unrestricted ones). Ordered by priority desc, starts_at desc. `{"announcements":[{id,title,body,priority,starts_at,ends_at,platforms}]}` |
| admin `GET /announcements` | all |
| admin `PUT /announcements/{id}` | create or replace `{title (required), body, priority, starts_at, ends_at, platforms[]}`; 400 on `ends_at <= starts_at` |
| admin `DELETE /announcements/{id}` | 204 / 404 |

## Events (活动)
`GET /events` (public / optional auth) returns active events `{id,title,starts_at,ends_at,timezone,metadata}`. Admin `GET /events`, `PUT /events/{id}` `{title, starts_at, ends_at (both required), timezone (IANA, default UTC), metadata (JSON object <= 8KiB)}`, `DELETE /events/{id}`. The timezone is descriptive for clients; the window itself is absolute. A task may name an `event_id` (below).

## Mail
Admin `POST /mail` `{player_id | broadcast:true, title, body, grants?, expires_at?}` -> 201 `{mail_id, broadcast}`. Exactly one of `player_id` / `broadcast`. A broadcast is stored once; per-player state (read/claimed/deleted) is created lazily on first read/claim/delete, so sending to a million players writes one row. A broadcast is visible to players registered at or before it was sent (players who register later do not receive old broadcasts). Supports `Idempotency-Key`. Admin `GET /mail` (latest 100 with `claimed_count`), `DELETE /mail/{id}` (recall for everyone; already paid grants stay).

Player (all need a session):
- `GET /mail` -> `{"mail":[{id,title,body,grants,created_at,expires_at,read,claimed,claimable}]}` newest first, max 100; expired and deleted mail are hidden.
- `POST /mail/{id}/read` -> 204.
- `POST /mail/{id}/claim` -> 200 `{mail_id, grants}`. 404 not visible/expired/deleted; 409 `no_attachment`; 409 `already_claimed`. Ledger source key `mail:<mail_id>:<player_id>`; concurrent claims pay exactly once.
- `DELETE /mail/{id}` -> 204 hides it for this player. A mail with an **unclaimed attachment** is refused with 409 `unclaimed_attachments` unless `?discard=true`, which forfeits the attachment.

`Maintain(ctx)` deletes mail (and its state) expired more than 7 days ago.

## Daily check-in
Admin `PUT /checkin` `{cycle:[{asset:amount},...] (1-366 days), timezone (IANA, default UTC), streak_reset}`; admin `GET /checkin`.
- `GET /checkin` -> `{timezone, today (local date), checked_in_today, streak, total, cycle_length, next_index, next_grants, next_available_at}`; 404 until configured.
- `POST /checkin` -> `{day, streak, index, grants}` once per local day of the configured timezone; 409 `already_checked_in`. Reward = `cycle[(streak-1) mod len]`. With `streak_reset=true` missing a local day restarts at 1; with false the streak keeps counting. Source key `checkin:<local-day>:<player>`.

## Tasks
Admin `PUT /tasks/{id}` `{title?, metric, target>0, period: once|daily|weekly, grants, timezone?, event_id?}`, `GET /tasks`, `DELETE /tasks/{id}` (also drops progress). Daily periods roll at local midnight, weekly at ISO-week boundaries in the task timezone. A task with `event_id` is only visible, counted and claimable while that event is active.

Progress is server-authoritative; clients cannot post it. Game code calls, inside its own `WriteTx`:
```go
err := mod.Progress(ctx, tx, gameID, playerID, "wins", 1) // delta 1..1e9
```
Tooling may use admin `POST /progress` `{player_id, metric, delta}` -> `{tasks_updated}` (404 unknown player).

Player: `GET /tasks` -> `{tasks:[{id,title,metric,target,period,grants,period_key,progress,completed,claimed,claimable}]}`; `POST /tasks/{id}/claim` -> `{task_id, period_key, grants}`; 404, 403 (event inactive), 409 `not_completed`, 409 `already_claimed` (once per period). Source key `task:<id>:<period_key>:<player>`.

## Redeem codes
Admin `POST /redeem-batches`:
- N single-use codes: `{count: 1..10000, grants, expires_at?, label?}`
- one shared code: `{shared:true, code? (4-32 letters/digits, generated if absent), max_redemptions>=1, grants, expires_at?}`; each player may redeem it once, total redemptions are capped.
-> 201 `{batch_id, shared, max_redemptions, codes:[...]}`. **Plaintext codes appear only in this response**; the database stores `SHA-256(game_id ":" normalized_code)`. Generated codes look like `ABCD-EFGH-JKMN` (12 symbols, about 59 bits). Matching ignores case, dashes and spaces. 409 `code_exists` for a duplicate custom code. Admin `GET /redeem-batches` (counts only), `DELETE /redeem-batches/{id}` disables a batch (204, idempotent).

Player `POST /redeem` `{code}` -> 200 `{batch_id, grants}`.
- Unknown, malformed, expired, disabled and exhausted codes all return the same 400 `invalid_code` (no enumeration oracle).
- A player who already redeemed that code gets 409 `already_redeemed` (distinct on purpose: it only reveals information to someone who holds the code).
- Rate limit: 429 with `Retry-After`, per player and per client IP (Module options below); every attempt counts, including failures.
- Capacity is enforced by a guarded `UPDATE ... WHERE redeemed < max` in the same transaction as the ledger post; concurrent redeems of a single-use code yield exactly one success.
- Source key `redeem:<code_hash>:<player>`.

## Module options
`Kit`, `DB`, `Now` (optional clock), `RedeemPerPlayerPerMinute` (default 10), `RedeemPerIPPerMinute` (default 60).
