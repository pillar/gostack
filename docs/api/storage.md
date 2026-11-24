# Object storage (F11)

Package `modules/storage`: the ObjectStorage port (PRD §4.2) with a local
filesystem adapter and an S3-compatible adapter (AWS S3, Cloudflare R2, MinIO).
No AWS SDK is used; SigV4 is implemented in `sigv4.go`.

```go
type Storage interface {
    Put(ctx, key string, r io.Reader, size int64, contentType string) error // size -1 = unknown
    Get(ctx, key string) (io.ReadCloser, storage.Info, error)               // caller must Close
    Delete(ctx, key string) error                                           // missing key = success
    Presign(ctx, method, key string, ttl time.Duration) (string, error)     // GET or PUT, 1s..7d
}
```

Errors: `ErrNotFound`, `ErrTooLarge`, `ErrInvalidKey`, `ErrSizeMismatch`,
`ErrContentType` (use `errors.Is`); S3 HTTP failures return an error carrying
status and S3 error code (never credentials).

## Common behaviour

- **Keys**: 1–512 bytes, segments of `[A-Za-z0-9._-]` separated by single `/`;
  no leading/trailing `/`, no empty, `.` or `..` segment. Such keys never need
  URL escaping. Map user-supplied names to keys yourself (e.g. IDs).
- **Size limit**: `MaxObjectBytes` (default 100 MiB). A declared size above the
  limit is rejected before any IO; unknown sizes are counted while streaming
  and fail with `ErrTooLarge` as soon as the limit is exceeded. A body shorter
  or longer than the declared size fails with `ErrSizeMismatch`.
- **Timeouts**: `Timeout` (default 60 s) per operation. For `Get` it covers the
  body transfer and is released on `Close`.
- **Streaming**: bodies are never buffered in memory.
- **Content type**: printable ASCII, ≤ 255 bytes; empty → `application/octet-stream`.

## Local adapter

```go
l, err := storage.NewLocal(storage.LocalConfig{
    Root: "/var/lib/app/objects", PublicURL: "https://api.example.com/v1/files",
    Secret: secret32Bytes, MaxObjectBytes: 100 << 20, Timeout: time.Minute})
mux.Handle("/v1/files/", l.Handler())
```

- Each object is one file `Root/objects/<hh>/<sha256(key)>` containing the body,
  a JSON metadata trailer (key, size, content type, sha256) and its length.
  Hashed names avoid path traversal and case-insensitive filesystem aliasing.
- **Atomic writes**: temp file in `Root/tmp` → fsync → rename → fsync dir.
  A failed or partial upload never replaces the existing object.
- **Presigned URLs**: `PublicURL/<key>?X-GS-Expires=<unix>&X-GS-Signature=<hex>`,
  signature `HMAC-SHA256(secret, "GOSTACK-LOCAL-V1\n" + METHOD + "\n" + key + "\n" + expires)`.
  `Handler` verifies method, key, expiry (and that expiry ≤ now + 7 d) with a
  constant-time comparison, then:
  - `GET` → 200 with `Content-Type`, `Content-Length`, `ETag`,
    `X-Content-Type-Options: nosniff`, `Content-Security-Policy: sandbox`,
    `Cache-Control: private, no-store`.
  - `PUT` → stores the body with the request `Content-Type`; 200.
  - Errors (JSON envelope): 403 `forbidden` (bad/expired signature, wrong
    method), 404 `not_found` (missing object or invalid key path),
    405 `method_not_allowed`, 413 `payload_too_large`, 400 `invalid_request`
    (length mismatch, bad content type).
  - A presigned PUT URL does not bind content type or size; size is bounded by
    `MaxObjectBytes` (and by the server's global body limit, which must be at
    least `MaxObjectBytes` on this route for large uploads).
  - URLs are bearer capabilities until they expire; there is no revocation
    other than rotating the secret.

## S3-compatible adapter

```go
s, err := storage.NewS3(storage.S3Config{
    Endpoint: "https://<account>.r2.cloudflarestorage.com", Region: "auto",
    Bucket: "assets", AccessKeyID: id, SecretAccessKey: secret,
    PathStyle: true, SignPayload: false, MaxObjectBytes: 100 << 20, Timeout: time.Minute})
```

- `PathStyle`: `endpoint/bucket/key`; otherwise virtual-host `bucket.endpoint/key`.
- `SignPayload=false` sends `x-amz-content-sha256: UNSIGNED-PAYLOAD` and streams
  directly when the size is known. `SignPayload=true` (or size −1) spools the
  body to a temp file in `TempDir` (bounded by `MaxObjectBytes`) to compute
  the SHA-256 / Content-Length, then uploads it with a single PUT.
- `SessionToken` (temporary credentials) is sent and signed as
  `X-Amz-Security-Token`.
- Header signing covers `host`, all `x-amz-*`, and `content-type`,
  `content-md5`, `range`, `date` when present. `GET`/`DELETE` use the
  empty-body hash.
- Presign: SigV4 query authentication (`X-Amz-Algorithm`, `X-Amz-Credential`,
  `X-Amz-Date`, `X-Amz-Expires`, `X-Amz-SignedHeaders=host`, `X-Amz-Signature`),
  `UNSIGNED-PAYLOAD`, GET or PUT, max 7 days.
- No multipart upload: one PUT per object (S3 limits a single PUT to 5 GiB).

## Verification status

- SigV4 is checked against three AWS documentation examples (S3 presigned GET
  `examplebucket/test.txt`, header-signed GET with `Range`, header-signed PUT of
  `test$file.text` with a signed payload).
- Put/Get/Delete/Presign run against an in-process fake S3 (`httptest`) that
  re-verifies every signature with an independent implementation (itself
  checked against the AWS examples), including path-style and virtual-host,
  signed/unsigned payload, session token, expiry and tampering.
- **Not verified against real AWS S3, Cloudflare R2 or MinIO** (no network
  access during development). Run a smoke test against the target provider
  before relying on it; R2 uses region `auto`.
