# GameKit commerce API (G11)

Package `kits/game/commerce`, migrations `0007_game_commerce.sql` and
`0016_commerce_providers.sql` (order `amount`, `prepay_id`).
Implements: App Store in-app purchase verification (StoreKit 2 / App Store Server
API signed transactions and App Store Server Notifications V2), Google Play
one-time products (Play Developer API + Real-time developer notifications), WeChat
Pay API v3 JSAPI / mini-program payment (prepay + notifications), rewarded-ad
server-to-server callbacks (AdMob SSV and a generic HMAC-SHA256 scheme),
refunds with shortfall tracking, and an outbox hook.

**Not implemented:** WeChat mini-game virtual payment (米大师 / 虚拟支付) — a
different API; Huawei and any other IAP provider; subscriptions (Apple renewals
and Google `subscriptionNotification` are verified and ignored);
`REFUND_REVERSED`; automatic handling of partial refunds (Google quantity-based
partial voids and WeChat Pay partial refunds are verified, recorded `ignored`
and left to an operator); WeChat Pay order closing and refund initiation
(refunds are started in the merchant platform; only their notifications are
handled); Google `pendingRefundReviewNotification`. `commerce.ValidateProviders`
and config validation reject unimplemented names.

Platform policy (not verified): WeChat rules may require mini-games to sell
virtual goods through the virtual payment API rather than merchant WeChat Pay.
Whether a given app may use the `wechatpay` provider is the operator's call.

**Verification status:** all crypto and HTTP paths are tested only with locally
generated keys and httptest fakes: a locally generated Apple-like certificate
chain; a fake Google OAuth2 token endpoint, Play Developer API and JWKS with
generated RSA keys; a fake WeChat Pay JSAPI endpoint with generated merchant and
platform RSA keys and APIv3 key. No real Apple, Google or WeChat endpoint, real
credential or real signed payload was exercised. Formats follow the official
documentation read on 2026-11-25 (Google: androidpublisher v3
purchases.products, RTDN reference, Pub/Sub push authentication; WeChat Pay:
API v3 request signature, JSAPI 下单, JSAPI 调起支付签名, 支付成功回调通知,
退款结果回调通知). Real-service behaviour in edge cases (a repeated JSAPI order
with the same `out_trade_no` after a lost response, test purchases without
`orderId`) is not verified.

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

### Binding for Google Play and WeChat Pay

- **Google Play:** the client passes the order's `app_account_token` to
  `BillingFlowParams.Builder.setObfuscatedAccountId`. The Play Developer API
  returns it as `obfuscatedExternalAccountId`, which is bound exactly like
  Apple's `appAccountToken` (same 403/400 errors).
- **WeChat Pay:** the server creates the WeChat order itself with
  `out_trade_no` = the order's token without dashes (32 hex), so a payment
  notification names exactly one order and its player. The paid `amount.total`
  must equal the price fixed on the order (409 `amount_mismatch`); `mchid` and
  `appid` must match the configuration.

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

## Google Play

Configuration `game.commerce.google`: `service_account_ref` (secret reference to
the service account JSON key; never logged), `apps.<game>` with `package_name`,
`finish` (`consume` by default, or `acknowledge` for non-consumables) and
`allow_test_purchases` (rejected in production), and optional `rtdn` with the
`audience` and `service_account_email` of the Pub/Sub push subscription.
`token_url`, `api_base_url` and `rtdn.jwks_url` exist for tests only.

- **Access token:** an RS256 JWT (`iss` = client_email, `scope`
  `https://www.googleapis.com/auth/androidpublisher`, `aud` = token URL, 1h)
  exchanged at `https://oauth2.googleapis.com/token` with
  `grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer`. Cached until one
  minute before expiry, dropped on a 401. Stdlib crypto only. At most one
  token request is in flight (no lock is held during network IO); concurrent
  callers wait for it only as long as their own request context allows.
- **Verify** (`POST /iap/google/verify`, body `{"product_id","purchase_token"}`)
  calls
  `GET /androidpublisher/v3/applications/{package}/purchases/products/{product}/tokens/{token}`
  before any transaction. `purchaseState` 0 → deliver, 1 (canceled) → 409
  `transaction_refunded`, 2 (pending) → 409 `purchase_pending` (checked before
  `orderId`, which is not set while pending). `purchaseType` 0 (test) → 400
  `test_purchase_rejected` unless allowed. A standard purchase (`purchaseType`
  absent) without `orderId` → 400 `invalid_purchase`; promo (1) and rewarded (2)
  purchases are accepted without `orderId` (identity = purchase token). The
  androidpublisher reference does not state when `orderId` is absent
  (`purchaseType` is documented as "only set if this purchase was not made using
  the standard in-app billing flow"; the Billing Library documents `orderId` as
  null only while pending), so tolerating its absence for non-standard purchase
  types is a defensive choice, not documented behaviour. A 400/404/410 from Google → 400
  `invalid_purchase`; any other failure → 503 `store_unavailable`. The provider
  transaction id is the hex SHA-256 of the purchase token; `orderId` is stored as
  `original_txn_id`. A purchase that is already consumed/acknowledged is still
  delivered at most once (the order table enforces uniqueness), without a finish
  job (`finish_state` `done`).
- **Acknowledge / consume:** the delivery transaction enqueues the job
  `commerce.google.finish` (deduplicated per order, `max_attempts` 200) and sets
  the order's `finish_state` to `pending` and stores the purchase token
  (`purchase_token`, migration 0019). The job calls `:consume` or `:acknowledge`
  outside any transaction, then records `finish_state` `done` and `finished_at`.
  Google refunds purchases that are not acknowledged within three days, so the
  jobs worker must run (`queue.workers` > 0); the app registers the handler from
  `Module.JobHandlers()`.
  - Transient failures (network, 5xx, 401, 408, 429) are retried by the queue:
    exponential backoff with full jitter capped at 1 h, so 200 attempts span
    about 4 days on average (attempts from the 12th on wait 0–1 h, mean 30 min);
    tested with a fake that answers 503 for 48 simulated hours (still
    `retry_wait`, then completes).
  - Any other 4xx (e.g. 403: the service account lacks permission, 404) is first
    checked with purchases.products.get: already consumed (or acknowledged in
    `acknowledge` mode) counts as `done`. Otherwise the order gets
    `finish_state` `needs_attention` and `finish_error` (admin order list,
    `?finish_state=needs_attention`), an error log line and
    `commerce_store_finish_attention_total{provider,status}` is incremented; the
    job keeps retrying, so the purchase is finished once the cause is fixed.
  - Reconciliation (`Module.Maintain`, every 10 min with the app's maintenance):
    delivered orders with `finish_state` `pending` or `needs_attention`,
    delivered within the last 3 days, whose finish job is `dead` are replayed
    (fresh attempt budget) and whose job is missing are re-enqueued from the
    stored purchase token (`commerce_store_finish_requeued_total{provider}`).
    At most 500 orders per run. Orders delivered before migration 0019 have
    `finish_state` backfilled from their job if it still exists (succeeded jobs
    are purged after 7 days); otherwise it stays empty and they are not reconciled.
- **RTDN** (`POST /callbacks/google`, Pub/Sub push with OIDC authentication):
  the bearer JWT must be RS256 with a `kid` from Google's JWKS
  (`https://www.googleapis.com/oauth2/v3/certs`). The JWKS is fetched before the
  transaction and cached 1h; an unknown `kid` triggers a refetch at most once a
  minute. At most one fetch is in flight and no lock is held during it: an
  expired but known key is used while the refresh runs; a caller that needs
  the fetch waits only as long as its own context allows (503 `key_fetch_failed`). The token also needs `iss` `accounts.google.com` or
  `https://accounts.google.com`, `aud` = the configured audience, an unexpired
  `exp` (60 s leeway), and `email` = the configured push service account with
  `email_verified: true`. Otherwise the answer is 401 `invalid_signature` and
  nothing is recorded. Event id = `message.messageId`; `packageName` must match
  (400 `package_mismatch`).
  - `voidedPurchaseNotification` with `productType` 2 and `refundType` 1 → refund
    (grants reversed, shortfall rules below). Partial voids (`refundType` 2) and
    subscription voids → `ignored` with a reason.
  - `oneTimeProductNotification` type 1 (PURCHASED) → re-checked with
    purchases.products.get, then delivered to the token's player (or stored as
    `paid`). Pending and test purchases → `ignored`. Type 2 (CANCELED) → refund.
  - Test, subscription and pending-refund-review notifications → `ignored`.

## WeChat Pay (API v3, JSAPI / mini-program)

Configuration `game.commerce.wechatpay`:
- `mch_id`, `merchant_serial_no`
- `merchant_private_key_ref` (PEM, secret reference)
- `apiv3_key_ref` (32 bytes, secret reference)
- `platform_keys`: `Wechatpay-Serial` → PEM public key or platform certificate
  file (WeChat Pay public keys have serials `PUB_KEY_ID_…`)
- `notify_base_url` (https origin of this server), `max_skew` (default 5m)
- `query_after` (default 10m, must be < 2h): unpaid orders with a `prepay_id`
  older than this are queried (see Order query below)
- `apps.<game>`: `app_id` (the AppID the player logged in with through
  `game.platforms.wechat`), `prices` (wechatpay provider product id → fen),
  optional `description`

Map products with `provider_product_ids: {"wechatpay": "<id in prices>"}`.

Client flow: `POST /iap/orders` → `POST /iap/wechatpay/prepay {"order_id"}` →
`wx.requestPayment(payment)` → poll `GET /iap/orders/{order}` until `delivered`.

- **Prepay** (no network IO inside a transaction):
  1. Checks: the order belongs to the player and is `created` (else 404 / 409
     `invalid_state`); it is younger than 2h − 1 min (else 409 `order_expired`);
     the player has a `wechat` identity, i.e. an openid (else 409
     `wechat_identity_required`); the product has a wechatpay mapping and price
     (else 422 `product_not_mapped`).
  2. A WriteTx fixes `provider`, `provider_product_id` and `amount` on the order.
  3. `POST https://api.mch.weixin.qq.com/v3/pay/transactions/jsapi` with
     `Authorization: WECHATPAY2-SHA256-RSA2048 mchid,nonce_str,signature,timestamp,serial_no`
     (signature = base64 RSA-SHA256 over `METHOD\nPATH\ntimestamp\nnonce\nbody\n`).
     The body has `appid, mchid, description, out_trade_no, time_expire`
     (created + 2h), `attach` (= wechatpay product id), `notify_url`
     (`notify_base_url/v1/games/{game}/callbacks/wechatpay`),
     `amount{total, CNY}` and `payer{openid}`. The response signature is
     verified with the platform key.
  4. A WriteTx stores `prepay_id`. The answer carries `wx.requestPayment`
     parameters; `paySign` = base64 RSA-SHA256 over
     `appId\ntimeStamp\nnonceStr\npackage\n`.

  Failure modes:
  - WeChat unreachable or an error answer → 503 `store_unavailable`. The order
    keeps its fixed amount, and a retry repeats step 3 with identical parameters.
  - WeChat answers `OUT_TRADE_NO_USED` → 409 `prepay_conflict`; the client
    creates a new order.
  - WeChat accepted step 3 but the response was lost: if the payer still pays
    that WeChat order, its notification (which names the out_trade_no) delivers it.
  - Step 4 fails after a successful step 3: the next prepay calls WeChat again.
    Whether WeChat returns the same `prepay_id` for an identical repeat is
    **not verified**.
  - Once `prepay_id` is stored, repeats return it without calling WeChat.
- **Notifications** (`POST /callbacks/wechatpay`): `Wechatpay-Signature` must be
  RSA-SHA256 over `Wechatpay-Timestamp\nWechatpay-Nonce\nbody\n` with the key
  named by `Wechatpay-Serial`, and the timestamp must be within `max_skew`
  (else 401 `invalid_signature`). `resource` is decrypted with AEAD_AES_256_GCM
  (APIv3 key, `nonce`, `associated_data`); a failure is 400 `invalid_signature`.
  Event id = the notification `id`.
  - `TRANSACTION.SUCCESS` with `trade_state` SUCCESS: `mchid` (400
    `merchant_mismatch`) and `appid` (400 `app_mismatch`) must match;
    `out_trade_no` → order token; `attach` → store product; `amount.total` must
    equal the order amount (409 `amount_mismatch`, recorded `failed`). The
    payment is delivered once, keyed by `transaction_id`.
  - `REFUND.SUCCESS` with `refund_status` SUCCESS and `amount.refund` =
    `amount.total` → refund (grants reversed). Partial refunds, `REFUND.ABNORMAL`
    and `REFUND.CLOSED` → `ignored`. A refund that arrives before the payment
    notification marks the order `refunded`; the later payment is ignored.
  - Answer: `204` without body on success (processed, ignored or duplicate).
    On failure: the error's HTTP status with
    `{"code":"FAIL","message":"<error code>"}`, so WeChat Pay retries.
- **Order query** (lost or late notifications; `Module.Maintain`, every 10 min):
  orders with provider `wechatpay`, state `created`, a stored `prepay_id`,
  created at least `query_after` ago and less than 2h (prepay expiry) + 1h ago
  are queried, oldest first, at most 100 per run, outside any transaction:
  `GET /v3/pay/transactions/out-trade-no/{out_trade_no}?mchid={mch_id}` with the
  same merchant `Authorization` (signed message
  `GET\n/v3/pay/transactions/out-trade-no/{no}?mchid={mch_id}\ntimestamp\nnonce\n\n`).
  The 200 answer's `Wechatpay-Signature` is verified with the platform key like
  every API v3 response (the official page's samples verify the response
  signature on 2xx; the parameter tables do not describe it). 404
  `ORDER_NOT_EXIST` → skipped. `trade_state` SUCCESS is checked exactly like a
  `TRANSACTION.SUCCESS` resource (mchid, appid, out_trade_no, CNY amount equal to
  the order amount) and delivered through the notification path with inbox
  event id `query:<transaction_id>`; the order/ledger uniqueness on
  `transaction_id` makes it idempotent with a notification arriving earlier,
  later or concurrently. `NOTPAY`, `CLOSED`, `REFUND`, ... → nothing (a refund
  is applied by its notification). Errors (network, bad signature, mismatch)
  are returned from `Maintain` and logged; the next run retries. Counter
  `commerce_wechatpay_query_delivered_total`. An abandoned order is queried
  about (3h − `query_after`) / 10 min times. Verified only against an httptest
  fake built from the official API page
  (https://pay.weixin.qq.com/doc/v3/merchant/4012791859, read 2026-11-25), not
  against WeChat Pay.

## Client routes (session required)

### POST /v1/games/{game}/iap/orders
Body `{"product_id":"gems100"}`. Supports `Idempotency-Key`.
201 `{"order_id","product_id","app_account_token","provider_product_ids":{"apple":"com.x.gems100"},"state":"created"}`.
Errors: 400 `invalid_request`, 404 `not_found` (product).

### GET /v1/games/{game}/iap/orders/{order}
200 `{"order":{...}}` for the session player's own order; otherwise 404 `not_found`.

### POST /v1/games/{game}/iap/wechatpay/prepay
Body `{"order_id"}`. 200 `{"order_id","out_trade_no","amount","currency":"CNY","payment":{"appId","timeStamp","nonceStr","package","signType":"RSA","paySign"}}`.
Errors are listed under WeChat Pay above.

### POST /v1/games/{game}/iap/{provider}/verify
Apple: body `{"signed_transaction":"<JWS signedTransactionInfo>"}`. Google:
`{"product_id":"<Play product id>","purchase_token":"..."}` (max 128 KiB).
`wechatpay` answers 400 because delivery comes from the payment notification.
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

Callback routes are registered with `Kit.HandleCallback` and are exempt from
the game's `requests_per_minute` and `write_concurrency` quotas
(docs/api/quotas.md): otherwise unauthenticated traffic to any route of the game
could exhaust the shared bucket and make signed Google RTDN, WeChat Pay (which
has no client-side verify fallback) or ad SSV callbacks fail with 429/503.
Each handler authenticates the provider (signature / OIDC token) before any
database work; unauthenticated callback traffic costs one signature check and
is bounded only by server-wide limits (HTTP server, reverse proxy), not by the
game's quota. Tested: with `requests_per_minute` 5 and a flood of unsigned
callbacks and public requests, a signed RTDN and a signed WeChat Pay
notification still succeed.

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
- `GET /v1/admin/games/{game}/orders?state=&finish_state=&player_id=&limit=` (limit 1–200,
  default 50), newest first: `{"orders":[order...]}`. Use
  `?state=refund_shortfall` for the review queue and
  `?finish_state=needs_attention` (`pending|done|needs_attention`) for purchases
  the store refused to acknowledge/consume. Admin orders additionally carry
  `finish_state`, `finish_error` and `finished_at` when set (not shown to players).
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
`ad_reward`. Google Play deliveries also enqueue `commerce.google.finish`, which
the module handles itself (see Google Play). The job payload is `OutboxEvent` JSON and carries a dedupe key per
order or event. The application supplies the handler, for example one that
notifies a game server. Jobs run at least once, so the handler must be
idempotent.

## Maintenance

`Module.Maintain(ctx)` deletes ad intents that expired more than a day ago, and
non-failed inbox rows older than 90 days. Duplicate protection then rests on the
order and ad-reward uniqueness constraints.
