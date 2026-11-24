# Matchmaking and rooms API (GameKit G15)

Package `kits/game/match`. Three audiences:

* Players: `/v1/games/{game}/match/...` with a player session.
* Admins: `/v1/admin/games/{game}/match/...` with an `admin` API key of the game's tenant.
* Battle servers: `/v1/match/rooms/...` with an API key carrying scope `battle`. These routes are not under a game; every room is checked against the key's tenant and another tenant's room is `404`.

Battle state (ticks, positions) never touches SQLite; the module writes only on ticket/room creation, claim, heartbeat (once per lease interval), member state change and settlement. Battle servers run as separate deployments; a room is owned by at most one server through a lease and a fencing token, and SQLite is never shared between nodes.

## Queues (admin)
`PUT /v1/admin/games/{game}/match/queues/{queue}`
```json
{"size":2,"rating_window":50,"widen_per_sec":10,"ttl":60,"max_window":0,"board":"score"}
```
`size` 2-64 players per room; a ticket may match players whose rating differs by at most `rating_window + widen_per_sec * seconds_waited` (the narrowest window in the group applies; `max_window` > 0 caps it); `ttl` ticket lifetime in seconds (5-3600); optional `board` receives settlement scores (must exist in the game config). `GET /v1/admin/games/{game}/match/queues` lists them.

## Tickets (player)
| Route | Result |
|---|---|
| `POST /match/tickets` `{"queue":"duel","attributes":{"rating":1200}}` | `201 {"ticket_id","queue","status":"waiting","expires_at"}`; unknown queue `404`; `409 ticket_active` when the player already has a waiting or matched ticket; rating within +-1,000,000 |
| `GET /match/tickets/{id}` | own ticket only (others `404`) |
| `DELETE /match/tickets/{id}` | cancels a waiting ticket (repeatable); `409 already_matched`, `409 ticket_closed` |

`POST` honours `Idempotency-Key`. Ticket status: `waiting`, `matched`, `expired` (TTL elapsed), `cancelled`, `abandoned` (room lease lost), `done` (room settled). `expired` and `abandoned` are reported as soon as time passes, even before the sweep persists them, and a stale ticket never blocks creating a new one.

Status response once matched:
```json
{"ticket_id":"tkt_..","queue":"duel","status":"matched","rating":1200,"created_at":1,"expires_at":2,
 "room":{"room_id":"rm_..","status":"open|active|settled|abandoned","lease_until":3,
         "connection":null,"members":[{"player_id":"..","state":"assigned|connected|left"}]}}
```
`connection` is `null` until a battle server claims the room, then the JSON object the server supplied. Players poll this endpoint (also to rejoin: the ticket stays `matched` until settlement or abandonment).

Matching consumes tickets: `MatchOnce` runs per queue inside one write transaction that reads the waiting tickets and flips each used ticket `waiting -> matched` (checked), so a ticket is in at most one room even when several matchers run concurrently. Tickets are single use.

## Room lifecycle (battle server)
Rooms are created `open` with a claim deadline (`ClaimTimeout`, default 60s). States: `open -> active -> settled`, or `open|active -> abandoned` when the lease expires.

**Claim** `POST /v1/match/rooms/claim` `{"server_id":"bs1","game_id":"alpha"?,"queue":"duel"?,"connection":{"url":"wss://.."}}`
Claims the oldest unassigned room of the key's tenant (optionally filtered). `200`:
```json
{"room_id":"rm_..","game_id":"alpha","queue":"duel","token":7,"lease_until":1,"lease_ttl_ms":30000,"members":[{"player_id":"..","state":"assigned"}]}
```
`204` when there is no room. `token` is the fencing token: taken from a per-game counter, so it only grows. `connection` must be a JSON object up to 1024 bytes. Honours `Idempotency-Key`.

**Heartbeat** `POST /v1/match/rooms/{room}/heartbeat` `{"token":7}` -> `{"room_id","lease_until"}`; extends the lease by `LeaseTTL` (default 30s). Send it about every `lease_ttl_ms / 3`.

**Member state** `POST /v1/match/rooms/{room}/members/{player}/state` `{"token":7,"state":"left|connected"}`. Report a player disconnecting (`left`) or rejoining (`connected`). The slot stays reserved; the battle server decides how a leaver is scored in the final result. `404` for non-members.

**Fencing errors** (all `409`): `stale_token` (token is not the room's current token, was never issued, or the room was never claimed), `room_abandoned` (lease expired: the room is marked abandoned in that same call, its players' tickets become `abandoned`), `room_settled`. A server that lost its lease cannot heartbeat, change state or settle.

**Settlement** `POST /v1/match/rooms/{room}/result`
```json
{"token":7,"results":[{"player_id":"..","score":700,"grants":{"coin":10}},{"player_id":"..","grants":{"coin":3}}]}
```
Only room members (`400 not_a_member`), distinct players, at most 64 entries, up to 8 grant assets per player with integer amounts in [0, 1e9]. `score` requires a configured queue board (`400 no_board`) and follows leaderboard rules (range check `400`, quarantine, best/sum, asc/desc, current season). Grants use `game.ApplyLedger` with source key `match:<room>:<player>`. Everything commits in one transaction together with the settlement row (primary key = room), the room becoming `settled` and tickets becoming `done`; any invalid entry rolls back the whole request. `200`:
```json
{"room_id":"rm_..","status":"settled","results":[{"player_id":"..","granted":{"coin":10},"score_status":"accepted","best":700}]}
```
Idempotent per room: repeating the identical body (same bytes, current token) returns the stored response with `Idempotent-Replayed: true`; a different body is `409 already_settled`; no double grants under concurrency.

## Maintenance and options
`Run(ctx)` loops `MatchOnce` + `Sweep` every `MatchInterval` (default 1s); tests and schedulers can call `MatchOnce(ctx)` and `Sweep(ctx)` directly. `Maintain(ctx)` = `Sweep` + purge of terminal tickets and abandoned rooms older than `Retention` (default 7d; settled rooms and settlements are kept). `Sweep` expires waiting tickets past their TTL and abandons rooms past their lease.

Options (defaults): `LeaseTTL` 30s, `ClaimTimeout` 60s, `MatchInterval` 1s, `Retention` 7d, `MaxMatchBatch` 500 (waiting tickets considered per queue per pass; matching is O(n^2) in this), `TicketsPerMinute` 60 per player.

Known limits: the module trusts the battle server's results (authenticated, tenant-scoped, fenced, idempotent) but does not verify gameplay; a room whose lease expired cannot be settled even if the match finished, so choose `LeaseTTL` above the longest expected heartbeat gap; the connection object is visible to all room members, so battle servers should authenticate players themselves (for example by checking the player's GoStack session or issuing their own join token in `connection`).
