# Web auth API (`/v1/auth/*`)

Opt-in: `auth.web.enabled: true`. Module: `modules/webauth`. Security design: `docs/SECURITY.md`.
All errors use the standard envelope `{"error":{"code","message"},"request_id"}`.

## Common rules

- Request bodies must be `Content-Type: application/json` (else `415 unsupported_media_type`); unknown fields are rejected (`400 invalid_request`).
- Unsafe requests from browsers must be same-origin or from `auth.web.allowed_origins` (`Origin`, `Sec-Fetch-Site`); otherwise `403 csrf_failed`.
- Session cookie: `gostack_session` (development) / `__Host-gostack_session` (production; Secure). `HttpOnly; SameSite=Lax; Path=/; Max-Age=session_ttl`.
- Authenticated unsafe requests need header `X-CSRF-Token: <csrf_token>` (`403 csrf_failed` when missing/wrong).
- Missing, expired (absolute or idle) or revoked session: `401 unauthorized`.
- Rate limits: `429 rate_limited` with `Retry-After`.
- Write queue full / hashing busy beyond the request deadline: `503 overloaded`.
- Responses that set or reveal session data carry `Cache-Control: no-store`.

## POST /v1/auth/signup

Body `{"email": "Alice@Example.com", "password": "8..128 bytes"}`. Creates the user in the module tenant and logs in.

`201`:
```json
{"user":{"id":"usr_…","tenant_id":"default","email":"alice@example.com","created_at":1764000000000},
 "csrf_token":"…","expires_at":1764604800000}
```
Errors: `403 signup_disabled` (`allow_signup: false`), `400 invalid_email`, `400 invalid_password`, `409 email_taken`, `429` (per IP).

## POST /v1/auth/login

Body `{"email","password"}`. Any session cookie presented is revoked and a new one issued.

`200`: same shape as signup. Errors: `401 invalid_credentials` (unknown account, wrong password, or invalid password length — identical response), `429` (per IP and per account).

## GET /v1/auth/me

`200`: `{"user":{"id","tenant_id","email"},"csrf_token","expires_at"}`. Use it to obtain the CSRF token after a page reload.

## POST /v1/auth/logout

Requires CSRF. Deletes the session. `204`, cookie cleared (`Max-Age=0`).

## POST /v1/auth/password

Requires CSRF. Body `{"current_password","new_password"}`.

`200`: `{"csrf_token","expires_at"}` plus a new session cookie. All other sessions of the user (and the old token of this browser) are revoked.
Errors: `400 invalid_password`, `401 invalid_credentials` (wrong current password), `409 password_changed` (concurrent change), `429` (per account).

## OPTIONS /v1/auth/*

CORS preflight: for origins in `allowed_origins` returns `Access-Control-Allow-Origin: <origin>`, `Access-Control-Allow-Credentials: true`, allowed methods and headers (`Content-Type, X-CSRF-Token`). Other origins get no CORS headers.

## Protecting application routes

```go
mux.Handle("POST /v1/things", a.WebAuth.Require(h))  // same 401/403 rules as above
s, _ := webauth.SessionFrom(r.Context())              // TenantID, UserID, Email
```

## Configuration (`auth.web.*`)

| key | default | validation (when enabled) | reload |
|---|---|---|---|
| `enabled` | false | — | restart |
| `session_ttl` | 168h | 5m..2160h | restart |
| `idle_timeout` | 24h | 1m..session_ttl | restart |
| `allow_signup` | true | — | restart |
| `allowed_origins` | [] | `scheme://host[:port]`, no path; https in production | restart |
| `ip_rate_per_minute` | 30 | ≥1 | hot |
| `account_rate_per_minute` | 10 | ≥1 | hot |
| `max_concurrent_hashes` | 2 | 1..64 | restart |

Storage: `web_users`, `web_sessions` (migration `0004_webauth.sql`). Expired and idle sessions are deleted by the app maintenance loop (every 10 minutes).
