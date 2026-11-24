# Billing API (F10, optional module)

Package `modules/billing`. Prepaid balances with an append-only ledger and
reservations (PRD §7). It is **not** a payment or clearing system: it never
contacts a payment provider and does no network IO.

## Model

- Money is integer minor units (`int64`), never floats. Single amounts and
  `available + reserved` are capped at `2^53-1` (exact in JavaScript).
- Tables (migration `0005_billing.sql`, prefixed `billing_` to avoid clashing
  with application tables): `billing_accounts`, `billing_ledger_entries`,
  `billing_reservations`, `billing_idempotency`.
  - `billing_accounts(tenant_id, id, currency, available, reserved, version, created_at, updated_at)`,
    `CHECK available >= 0`, `CHECK reserved >= 0`. `version` increments on every balance change.
  - `billing_ledger_entries` is append-only (UPDATE/DELETE triggers abort),
    `UNIQUE(tenant_id, idempotency_key)`. Each entry stores `delta_available`
    and `delta_reserved`, so `available = SUM(delta_available)` and
    `reserved = SUM(delta_reserved)` per account.
  - `billing_reservations.status ∈ reserved | settled | released | expired | reconciling`.
- Every balance change writes exactly one ledger entry in the same
  `WriteTx` as the conditional balance update. Overdraft is impossible: the
  update is conditional (`available + delta >= 0`) and the column has a CHECK.
- Tenant always comes from the caller (Go API argument / API key), never from a
  request body. All reads and writes are scoped by tenant.

### Reservation state machine

```
reserved ──Settle(actual ≤ amount)──▶ settled      (charge actual, release remainder)
reserved ──Release────────────────▶ released     (return all)
reserved ──sweeper (expires_at)───▶ expired      (return all)
reserved ──MarkUncertain──────────▶ reconciling  (funds stay reserved; nothing automatic)
reconciling ──Resolve(settle|release, operator note)──▶ settled | released
```

All other transitions fail with `409 invalid_state`. `Settle` on a `reserved`
reservation whose `expires_at` has passed fails with `409 reservation_expired`
(the caller should `MarkUncertain` if the external call may have happened, or
`Release`). Reconciling reservations are never expired by the sweeper.

Intended flow for paid external calls: `Reserve` (committed before returning)
→ perform the external call outside any transaction → `Settle` with the actual
cost, `Release` on definite failure, or `MarkUncertain` on timeout/unknown
outcome. No retry or charge happens automatically for reconciling reservations.

## Idempotency

Every mutation requires an idempotency key (1–128 printable ASCII, must not
start with `sys:`, which is reserved for sweeper entries). Keys are unique per
tenant across all billing operations and are stored permanently.

- Same key + same operation + same parameters → the stored result is returned
  (`Result.Replayed = true`; HTTP header `Idempotent-Replayed: true`); nothing
  is executed again. Concurrent duplicates execute once (single writer).
- Same key with a different operation or parameters → `409 idempotency_key_reused`.
- Failed operations are not recorded, so a key whose first attempt failed
  (e.g. `insufficient_funds`) can be retried.

## Go API

```go
svc := &billing.Service{DB: db, Now: time.Now}
svc.CreateAccount(ctx, tenant, key, id /* "" = generated acct_… */, currency)
svc.Credit(ctx, tenant, key, accountID, amount, note)
svc.Debit(ctx, tenant, key, accountID, amount, note)
svc.Reserve(ctx, tenant, key, accountID, amount, ttl /* 0 = 15m, max 7d */, note)
svc.Settle(ctx, tenant, key, reservationID, actual, note)
svc.Release(ctx, tenant, key, reservationID, note)
svc.MarkUncertain(ctx, tenant, key, reservationID, note)
svc.ResolveReconciliation(ctx, tenant, key, reservationID, "settle"|"release", actual, operatorNote)
svc.ExpireReservations(ctx, limit)   // sweeper, all tenants, one transaction
svc.Maintain(ctx)                    // runs the sweeper until no due reservations remain
svc.Reconcile(ctx, tenant /* "" = all */) ([]billing.Mismatch, error)
svc.GetAccount / GetReservation / Entries
```

Mutations return `billing.Result{Account, Entry?, Reservation?, Replayed}`.
Errors are `*httpx.Error` with the codes below.

`Reconcile` reads a snapshot from the read pool and compares, per account,
stored `available`/`reserved` with the ledger sums, and stored `reserved` with
the sum of `reserved`+`reconciling` reservations. Fields: `available`,
`reserved`, `open_reservations`. It scans the ledger; schedule it off-peak and
alert on any mismatch.

## HTTP API

Mount with `svc.Handler(mux, auth.APIKeys{DB: db})`. Every route requires
`Authorization: Bearer gsk_…` with scope `billing`; the tenant is the key's
tenant. Bodies are strict JSON (unknown fields rejected), max 16 KiB. Every
POST requires an `Idempotency-Key` header.

| Method | Path | Body | Success |
|---|---|---|---|
| POST | `/v1/billing/accounts` | `{"id"?: string, "currency": string}` | 201 Result |
| GET | `/v1/billing/accounts/{id}` | | 200 Account |
| GET | `/v1/billing/accounts/{id}/entries?limit=1..200` | | 200 `{"entries":[Entry]}` newest first |
| POST | `/v1/billing/accounts/{id}/credit` | `{"amount": int, "note"?: string}` | 200 Result |
| POST | `/v1/billing/accounts/{id}/debit` | `{"amount": int, "note"?: string}` | 200 Result |
| POST | `/v1/billing/accounts/{id}/reservations` | `{"amount": int, "ttl_seconds"?: int, "note"?: string}` | 201 Result |
| GET | `/v1/billing/reservations/{id}` | | 200 Reservation |
| POST | `/v1/billing/reservations/{id}/settle` | `{"amount": int, "note"?: string}` | 200 Result |
| POST | `/v1/billing/reservations/{id}/release` | `{"note"?: string}` | 200 Result |
| POST | `/v1/billing/reservations/{id}/uncertain` | `{"note"?: string}` | 200 Result (no entry) |
| POST | `/v1/billing/reservations/{id}/resolve` | `{"action": "settle"\|"release", "amount": int, "note": string}` | 200 Result |
| GET | `/v1/billing/reconcile` | | 200 `{"ok": bool, "mismatches": [Mismatch]}` (key's tenant only) |

Result:

```json
{
  "account": {"tenant_id":"t1","id":"u1","currency":"GEM","available":85,"reserved":0,"version":4,"created_at":0,"updated_at":0},
  "entry": {"id":"le_…","account_id":"u1","type":"settle","amount":15,"delta_available":25,"delta_reserved":-40,
            "reservation_id":"rsv_…","idempotency_key":"v1","note":"confirmed","created_at":0},
  "reservation": {"id":"rsv_…","account_id":"u1","amount":40,"settled_amount":15,"status":"settled",
                  "expires_at":0,"note":"timeout","created_at":0,"updated_at":0}
}
```

Entry types: `credit`, `debit`, `reserve`, `settle` (amount = charged; remainder
returned in `delta_available`), `release`, `expire`. Times are Unix ms.

Limits: account id 1–64 of `[A-Za-z0-9_.:-]`; currency 1–16 of `[A-Z0-9_]`
starting with a letter; amount 1..2^53-1 (settle/resolve allow 0); note ≤ 512
bytes; reservation TTL 1 s–7 d (default 15 min).

### Error codes

| Status | Code | When |
|---|---|---|
| 400 | `invalid_request` | validation, missing Idempotency-Key, unknown JSON field, settle amount > reserved |
| 401 | `unauthorized` | missing/invalid/revoked/expired API key |
| 403 | `forbidden` | key lacks `billing` scope |
| 404 | `not_found` | account/reservation not found in the key's tenant |
| 409 | `insufficient_funds` | debit/reserve exceeds available |
| 409 | `balance_limit` | credit would exceed 2^53-1 |
| 409 | `account_exists` | account id already used in the tenant |
| 409 | `invalid_state` | illegal reservation transition |
| 409 | `reservation_expired` | settle after `expires_at` |
| 409 | `idempotency_key_reused` | key reused with a different request |
| 413 | `payload_too_large` | body > 16 KiB |
| 503 | `overloaded` | write queue full (`Retry-After`) |

## Operations

- Call `Service.Maintain(ctx)` from the periodic maintenance loop to expire
  reservations.
- Schedule `Reconcile(ctx, "")` (e.g. daily) and alert on mismatches; a
  non-empty result means stored balances were changed outside the service or a
  bug exists — investigate before resolving.
- Not verified: performance under the 1C1G budget; behaviour with very large
  ledgers (Reconcile is a full scan).
