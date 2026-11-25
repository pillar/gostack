# GameKit commerce API (G11)

Package `kits/game/commerce`, migration `0007_game_commerce.sql`.
Implements: App Store in-app purchase verification (StoreKit 2 / App Store Server
API signed transactions and App Store Server Notifications V2), rewarded-ad
server-to-server callbacks (AdMob SSV and a generic HMAC-SHA256 scheme),
refunds with shortfall tracking, and an outbox hook.

**Not implemented:** Google Play, WeChat Pay and any other IAP provider;
subscriptions (renewals, `DID_RENEW`, `SUBSCRIBED` are verified and ignored);
`REFUND_REVERSED`. `commerce.ValidateProviders` rejects unimplemented names so
configuration cannot enable them.

**Verification status:** all crypto paths are tested with locally generated
keys and a locally generated root → intermediate → leaf certificate chain. No
real Apple or Google endpoint or real signed payload was exercised; the Apple
Root CA and AdMob production keys were never fetched in tests.

All errors use the standard envelope `{"error":{"code","message"},"request_id"}`.

## Security model

- **Verify outside, apply inside.** Signatures, certificate chains and key
  fetches happen before the database transaction. One short `WriteTx` then
  applies the inbox record, the order state machine and `ApplyLedger`.
- **A store transaction is delivered at most once, to one player.**
  `commerce_orders` has `UNIQUE(provider, provider_txn_id)` across all games, and
  the ledger source key `iap:<provider>:<transactionId>` is unique per game.
- **Binding transactions to players (cross-player replay).** The client first
  calls `POST /iap/orders`; the server issues a random UUID
  `app_account_token` bound to the session player and product. The client passes
  it to StoreKit as `appAccountToken`. Apple signs it into the transaction, so a
  transaction can be delivered only to the player who received the token. If a
  different player submits the transaction, the server returns 403
  `transaction_owned_by_other_player`. Transactions without a token are
  rejected unless the module option `AllowUnboundTransactions` is set. In that
  mode the first player to verify the transaction gets it, and every later
  submission is rejected. This is weaker: anyone who obtains the signed
  transaction first can claim it.
- **Ad rewards** are credited only from a signed server-to-server callback.
  The player comes from an opaque single-use intent token issued by
  `POST /ads/{placement}/intent`, never from a player id in the callback.
  Daily caps are counted inside the crediting transaction.
- **Balances never go negative.** A refund removes what the player still holds
  and records the rest as `outstanding` (state `refund_shortfall`).

## Order states

`created` (token issued) → `delivered` → `refunded` | `refund_shortfall` → `refund_compensated`.
`paid` means the store reported the payment but no player is bound yet (unknown
or missing token). A `paid` or `created` order that is refunded becomes
`refunded` without any ledger movement.

### Out-of-order callbacks (deterministic)

| Arrival order | Result |
|---|---|
| purchase notification, then client verify | notification delivers when the token is known; verify replays (`replayed: true`) |
| client verify, then purchase notification | notification `ignored` ("transaction already delivered") |
| refund, then purchase notification / verify | refund records the order `refunded` (bound to the token's player if known); the purchase notification is `ignored` and verify returns 409 `transaction_refunded`. Nothing is ever granted. |
| refund twice (same or different notification UUID) | the second is `duplicate` (same UUID) or `ignored` ("order already refunded") |

## Inbox (callbacks)

Every verified callback is recorded in `commerce_inbox` under
(game, provider, event_id). The event_id is Apple's `notificationUUID`, AdMob's
`transaction_id` or the HMAC `event_id`. Ad providers are stored as `ad:<name>`.

- `processed` / `ignored` are final. A re-delivery returns
  `{"status": <recorded>, "duplicate": true, ...}` with 200.
- `failed` is recorded when a verified event hits a business error that may clear
  later, for example `product_not_mapped`. The event's writes are rolled back
  (SAVEPOINT) and the error is returned (e.g. 422), so the provider retries. A
  re-delivery processes the event again. Any business error below 500 (including
  a 409 from `ApplyLedger`) is handled this way. Grants for IAP, ad rewards and
  match results are intentionally uncapped (`Kit.Grant` in `kits/game/ext.go`
  documents this), so `stack_limit` and `unknown_asset` cannot occur here; the
  business errors that can still occur are listed per route below.
- Callbacks that fail verification are not recorded.

Callback success body: `{"status":"processed|ignored","duplicate":false,"reason":"...","order_id":"..."}`.

## Client routes (session required)

### POST /v1/games/{game}/iap/orders
Body `{"product_id":"gems100"}`. Supports `Idempotency-Key`.
201 `{"order_id","product_id","app_account_token","provider_product_ids":{"apple":"com.x.gems100"},"state":"created"}`.
Errors: 400 `invalid_request`, 404 `not_found` (product).

### POST /v1/games/{game}/iap/{provider}/verify
Body `{"signed_transaction":"<JWS signedTransactionInfo>"}` (max 128 KiB).
The request is idempotent by transaction: a repeat by the same player returns the
same order with `replayed: true` and grants nothing more.
200 `{"order":{order_id,provider,product_id,provider_product_id,provider_txn_id,state,quantity,grants,outstanding,player_id,created_at,updated_at,delivered_at?,refunded_at?},"replayed":false}`.
Grants = product grants × transaction `quantity` (1–1000).

| Status | code | cause |
|---|---|---|
| 400 | `invalid_signature` | malformed JWS, non-ES256, chain not ending at the configured root, missing Apple marker OIDs (leaf 1.2.840.113635.100.6.11.1, intermediate 1.2.840.113635.100.6.2.1), bad signature |
| 400 | `bundle_mismatch` / `environment_mismatch` | transaction is for another app or an environment not accepted by this game |
| 400 | `invalid_request` | no `appAccountToken` and unbound transactions are disabled |
| 403 | `unknown_app_account_token` | token was not issued in this game |
| 403 | `transaction_owned_by_other_player` | token or earlier delivery belongs to another player |
| 404 | `provider_not_configured` | provider unknown, or not configured for this game |
| 409 | `transaction_refunded` | refunded/revoked before delivery |
| 409 | `transaction_already_used` | transaction belongs to another game |
| 409 | `product_mismatch` | store product differs from the order's product |
| 422 | `product_not_mapped` | store product id is not mapped in this game |

### POST /v1/games/{game}/ads/{placement}/intent
201 `{"token","placement_id","expires_at"}`. Pass `token` to the ad SDK as
server-side verification custom data (AdMob `custom_data`; `user_id` is used
only when `custom_data` is empty) or as the HMAC `token`. Each token is
single-use and valid for `AdIntentTTL` (default 24h). At most 20 unused
tokens can be open per player.
Errors: 404 `not_found` (placement), 409 `ad_cap_reached`, 409 `too_many_intents`.

## Public callback routes

### POST /v1/games/{game}/callbacks/{provider}
App Store Server Notifications V2 for the game (`provider` = `apple`). Body
`{"signedPayload":"..."}` (max 256 KiB; larger is 413 `payload_too_large`). Both the outer payload and the embedded
`signedTransactionInfo` are verified, along with bundle id and environment.
Errors: 400 `invalid_request` (body is not `{"signedPayload": ...}`), 400
`invalid_signature`/`bundle_mismatch`/`environment_mismatch`, 404
`provider_not_configured`. Business errors are recorded as `failed` and returned
so Apple retries: 422 `product_not_mapped` (store product not mapped) and 409
`product_mismatch` (store product differs from the order's product).
- `ONE_TIME_CHARGE`: the purchase is delivered to the token's player if the
  token is known. Otherwise the order is stored as `paid` and waits for verify.
- `REFUND`, `REVOKE`: grants are reversed (see Refunds).
- Any other type, including `TEST`, is verified and then `ignored`.
- A transaction whose `revocationDate` is set is handled as a refund.

### GET|POST /v1/games/{game}/callbacks/ads/{provider}
`provider` is the network name configured in `Module.Ads`.
- **AdMob SSV** (`AdMobVerifier`): ECDSA P-256/SHA-256 over the raw query
  string up to `&signature=`; `signature` is web-safe base64 DER; `key_id`
  selects the key. Only the signed parameters are trusted: `transaction_id`
  is the event id and `custom_data` (or `user_id`) is the intent token.
  `reward_amount`/`reward_item` are ignored; grants come from the placement.
  Keys come from `AdMobKeyFetcher`, which fetches `AdMobKeyURL` before any
  transaction. Keys are cached for 24h, an unknown key_id triggers a refetch at
  most once a minute, and a stale cached key is kept if a refetch fails.
- **HMAC** (`HMACAdVerifier`, secret per game): parameters `event_id`, `token`,
  `ts` (unix seconds, ±1h by default) and `sig`. They come from the query
  string, or from a form-encoded POST body when the query is empty.
  `sig = hex(HMAC-SHA256(secret, canonical))`, where `canonical` is every
  parameter except `sig`, sorted by name and joined as `name=value` with `&`
  (decoded values). Repeated parameters are rejected.

Errors: 401 `invalid_signature`, 400 `invalid_request`, 404 `provider_not_configured`,
413 `payload_too_large` (POST body or query string over 16 KiB), 503 `key_fetch_failed`. A verified callback that cannot be credited returns 200
with `status: "ignored"` and one of these reasons: `unknown_token`,
`token_used`, `token_expired`, `placement_removed`, `daily_cap_reached`.

Daily caps count credited rewards per (player, placement, UTC day).
`UNIQUE(game, player, placement, day, seq)` makes it impossible to exceed the cap.

## Refunds

On `REFUND`/`REVOKE` for a delivered order, each granted asset is deducted up to
the player's current balance (ledger kind `refund`, source
`refund:<provider>:<txn>`). If everything is recovered the state becomes
`refunded`. Otherwise the remainder goes into `outstanding` and the state
becomes `refund_shortfall`, for manual review.

## Admin routes (admin API key of the game's tenant)

- `PUT /v1/admin/games/{game}/products` replaces the catalog:
  `{"gems100":{"grants":{"gems":100},"provider_product_ids":{"apple":"com.x.gems100"}}}`.
  Product and asset ids must match `[a-z0-9][a-z0-9_-]{0,31}`. Grants must have
  1–32 positive integers. Providers must be configured. A store product id can
  map to only one product. Limit: 500 products. Delivered orders keep their
  grants snapshot. `GET` returns `{"products":{...}}`.
- `PUT /v1/admin/games/{game}/ad-placements` replaces the placements:
  `{"bonus":{"grants":{"coins":10},"daily_cap":3}}` (`daily_cap` 1–1000).
  `GET` returns `{"placements":{...}}`.
- `GET /v1/admin/games/{game}/orders?state=&player_id=&limit=` (limit 1–200,
  default 50), newest first: `{"orders":[order...]}`. Use
  `?state=refund_shortfall` for the review queue.
- `POST /v1/admin/games/{game}/orders/{order}/resolve` with body
  `{"waive":false}` retries recovering `outstanding` from current balances.
  `{"waive":true}` also writes off the remainder. The order becomes
  `refund_compensated` when nothing is outstanding or the remainder was waived.
  The remaining amount stays visible in `outstanding`. Returns 409
  `invalid_state` unless the order is in `refund_shortfall`.

## Outbox

When `Module.Outbox` (a `*jobs.Queue`) is set, a job of type `OutboxJobType`
(default `commerce.notify`) is enqueued in the same transaction for each of
`delivered`, `refunded`, `refund_shortfall`, `refund_compensated` and
`ad_reward`. The job payload is `OutboxEvent` JSON and carries a dedupe key per
order or event. The application supplies the handler, for example one that
notifies a game server. Jobs run at least once, so the handler must be
idempotent.

## Maintenance

`Module.Maintain(ctx)` deletes ad intents that expired more than a day ago, and
non-failed inbox rows older than 90 days. Duplicate protection then rests on the
order and ad-reward uniqueness constraints.
