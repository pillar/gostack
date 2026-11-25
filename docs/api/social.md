# Social API (GameKit G13)

Package `kits/game/social`. All routes are under `/v1/games/{game}` and need a player session (`Authorization: Bearer <session>`). `game_id` and `player_id` always come from the session. Errors use the standard envelope `{"error":{"code","message"},"request_id"}`.

Mutations accept an optional `Idempotency-Key` header (same semantics as the rest of GameKit: same key + same request replays the stored response with `Idempotent-Replayed: true`; same key + different request is `409 idempotency_key_reused`).

Blocked, hidden (not searchable) and unknown players are indistinguishable: all return `404 not_found` "player not found".

## Profile and privacy
| Route | Notes |
|---|---|
| `GET /social/profile` | Creates the profile (and its friend code) on first use. |
| `PUT /social/profile` | Partial update. |

```json
{"player_id":"plr_..","display_name":"Ann Lee","friend_code":"K7M2QX9A",
 "privacy":{"searchable":true,"friend_requests":"everyone","show_on_friend_boards":true}}
```
`display_name`: 1-24 characters, letters/digits/space/`_`/`-`/`.`, no leading or trailing space. `friend_requests`: `everyone` or `nobody`. `searchable=false` hides the player from new requests (by player id or friend code) unless that player already sent the caller a request. Invalid input: `400 invalid_request`.

## Friends
| Route | Result |
|---|---|
| `GET /friends` | `{"friends":[{player_id,display_name,at}]}` |
| `POST /friends/requests` `{"player_id"` or `"friend_code"}` | `201 {"status":"pending","player_id"}`; `200 {"status":"accepted","player_id"}` when the target had already asked the caller (`player_id` is the resolved target, also when a `friend_code` was sent) |
| `GET /friends/requests` | `{"incoming":[..],"outgoing":[..]}` |
| `POST /friends/requests/{player_id}/accept` | `{"status":"accepted","player_id"}`; `{player_id}` is the requester |
| `POST /friends/requests/{player_id}/decline` | recipient declines: `{"status":"declined","player_id"}` |
| `DELETE /friends/requests/{player_id}` | sender cancels: `{"status":"cancelled","player_id"}` |
| `DELETE /friends/{player_id}` | removes the friendship on both sides: `{"removed":true,"player_id"}` |

Friendship is stored as two symmetric rows written in one transaction together with the deletion of any pending requests between the pair.

Errors: `400` self request or not exactly one of `player_id`/`friend_code`; `404 not_found` unknown/hidden/blocked player or missing request; `403 requests_disabled` (target set `friend_requests: nobody`); `409 already_friends`, `already_requested`, `request_limit` (too many outgoing pending), `friend_limit` (the caller's friend list is full; at accept time either side being full), `429` rate limited.

## Blocks
| Route | Result |
|---|---|
| `GET /blocks` | `{"blocks":[{player_id,at}]}` |
| `POST /blocks/{player_id}` | `{"blocked":true,"player_id"}`; idempotent. In one transaction: records the block, removes the friendship and deletes pending requests in both directions. |
| `DELETE /blocks/{player_id}` | `{"blocked":false,"player_id"}` (never fails for an absent block) |

While either side blocks the other: friend requests both ways, share opens and invite accepts return 404, and the pair never appears on each other's friend boards. Errors: `400` self block, `404` unknown player, `409 block_limit`.

## Friend leaderboard
`GET /friends/boards/{board}[?season=..]` ranks the caller and friends using accepted scores of the board's current season (board and season rules from the game config; `404` for unknown boards). Friends with `show_on_friend_boards=false` are omitted; the caller always sees their own entry.
```json
{"board":"score","season":"all","entries":[{"rank":1,"player_id":"..","display_name":"Bob","score":300,"achieved_at":1,"me":false}]}
```

## Invites
| Route | Result |
|---|---|
| `POST /invites` | `201 {"code":"K7M2QX9A"}`; `409 invite_limit` |
| `GET /invites` | `{"invites":[{code,created_at,accepted}]}` |
| `POST /invites/{code}/accept` | `{"attributed":true,"grants_pending":false,"grants":{..}}`; `grants` is what was paid to the caller now (`{}` when pending or no `InviteeGrant` is configured) |
| `POST /invites/claim-pending` | `{"paid":true,"grants":{..}}`; `404` no accepted invite, `403 identity_required` (still guest-only), `409 already_paid` |

Accepting: the caller must have been created within the invite window (default 24h) else `403 not_new_player`; own code `400 self_invite`; unknown code or blocked pair `404`; a player is attributed once per game (`409 already_attributed`, enforced by the primary key on the invitee); inviters stop earning after `MaxAcceptsPerInviter` invitees (`409 invite_full`). Optional grants go through `game.ApplyLedger` with source keys `invite:<invitee>:inviter` and `invite:<invitee>:invitee`, so a retry or race can never grant twice. Codes are per game.

Grants require a verified invitee. Guest accounts cost nothing to create, so paying on a guest accept let one person farm `InviterGrant` (up to `MaxAcceptsPerInviter`) and `InviteeGrant` with throwaway guests. The attribution is always recorded (and counts toward `MaxAcceptsPerInviter`), but grants are paid in the accept transaction only when the caller has a platform identity (a `player_identities` row with provider other than `guest`, from a platform login or `POST /auth/bind`). Otherwise the accept answers `grants_pending: true` and the attribution is stored with `rewarded_at = 0`. After binding a platform identity the invitee calls `POST /invites/claim-pending`: it re-checks the identity, flips `rewarded_at` from 0 with a conditional UPDATE and pays both grants with the same source keys, so they are paid exactly once even under concurrent calls. Payout is explicit (not on bind) so the identity code in `kits/game` stays independent of the social module; clients should call it after a successful bind. Attributions created before migration 0012 count as paid.

## Shares
| Route | Result |
|---|---|
| `POST /shares` `{"channel","payload"}` | `201 {"share_id":"shr_.."}`; `channel` matches `[a-z0-9][a-z0-9_-]{0,31}`, `payload` is any JSON up to 2048 bytes |
| `POST /shares/{id}/open` | by another player: `{"share_id","channel","payload","first_open":true|false}` — one recorded open per player; no rewards. Own share `400 own_share`; unknown share or blocked pair `404` |
| `GET /shares/{id}` | sharer only: `{share_id,channel,payload,created_at,opens}` (distinct opening players) |

## Limits and options
`Module` options (defaults): `MaxFriends` 100, `MaxPendingRequests` 50, `MaxBlocks` 200, `MaxInvites` 5, `MaxAcceptsPerInviter` 50, `InviteWindow` 24h, `InviterGrant`/`InviteeGrant` (asset → amount, empty = no grant), `RequestsPerMinute` 30 (per player; applies to friend requests, invite create/accept and share create).

Known limits: friend codes are created lazily, so a player who never called a social endpoint can only be added by player id; there is no display-name search; declined requests can be re-sent (bounded by the rate limit and `request_limit`).
