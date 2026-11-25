# Static asset releases (PRD §22)

Package `modules/assets`, game module `assets`, CLI `gostack assets ...`.
Static files of a game build (H5 / mini-game remote assets) are stored in the
configured object store (`storage.driver: local | s3`, see
[storage.md](storage.md)) and served by a CDN or the bucket; the Go server only
publishes, keeps the release history and serves the current manifest. The
operator procedure is [docs/ASSET_RELEASE.md](../ASSET_RELEASE.md).

## Object layout and caching

| Object | Key | Cache-Control stored with the object |
|---|---|---|
| every non-entry file | `games/<game>/assets/<sha256 of body>/<name>` | `public, max-age=31536000, immutable` |
| manifest of a release | `games/<game>/manifests/<release>.json` | same |
| entry file of a release | `games/<game>/releases/<release>/<entry name>` | same |

- `<name>` is the file's base name mapped to the key alphabet (`[A-Za-z0-9._-]`,
  other bytes → `_`, at most 128 bytes); the manifest keeps the original
  relative path. The key is derived from the content, so a key's body never
  changes; manifest and entry keys are unique per release. All three are
  therefore immutable.
- Short caching applies only to the two API responses below
  (`game.assets.manifest_max_age`, default 60 s), because they follow the
  current-release pointer.
- `Cache-Control` is stored through the optional `storage.ObjectPutter`
  interface: S3/R2/MinIO keep it as the object's `Cache-Control` header (verified
  on MinIO: presigned GET returns it). The local driver records it in the object
  trailer (`Stat`/`Get` report it) but its presign handler still serves
  `private, no-store`, so the local driver is for development, not CDN origin.
- Release id: `YYYYMMDDTHHMMSSZ-xxxxxx` (UTC start time + 24 random bits).

### Entry HTML and relative URLs

Asset keys are content-addressed, so a file is **not** reachable at
`<entry dir>/<relative path>`. The entry must load the other files through
the manifest (`files[path].url`, or `base_url + files[path].key`), e.g. a small
bootstrap script that fetches `/v1/games/<game>/assets/manifest` and then loads
the bundle by URL. Relative `<script src="js/app.js">` references in the entry
do not resolve.

## Publish (CLI)

```
gostack [--config F] [--json] assets publish --game G --dir BUILD [--entry index.html] [--version LABEL] [--actor NAME]
```

1. Walks `BUILD` (symlinks and special files are refused, ≤ 10 000 files,
   paths ≤ 1024 bytes of UTF-8 without control characters) and hashes every
   file. `--entry` (default `index.html`) must exist.
2. Takes the game's publish lease (row in `assets_state`, TTL 10 min, renewed
   after every object). A concurrent publish or prune of the same game fails
   fast (`another publish or prune of this game is in progress`); other games
   are independent. A crashed publish blocks the next one until its lease expires.
3. Uploads every non-entry file that is not already stored (dedupe):
   - key recorded in `assets_objects` as live → `Stat` (HEAD) must show the
     object with the recorded size → not re-uploaded;
   - key not recorded but present in the bucket (e.g. left by a failed
     publish) → downloaded and hashed; equal → adopted, different → error
     (`object exists with a different body`), the object is **never overwritten**;
   - missing → uploaded; the bytes actually sent are hashed and must match the
     scan (a file changed during publish is deleted again and the publish fails).
4. Uploads the manifest, then the entry.
5. One database transaction (only if the lease is still held): insert the
   release, its file list, and switch `assets_state.current_release`.

Any error before step 5 leaves the previous release current. Objects uploaded
by a failed attempt stay recorded and are reused by the next publish or
removed by `prune`.

Manifest (stored form; the API adds `base_url` and `url`):

```json
{
  "manifest_version": 1, "game_id": "demo", "release_id": "20261125T101530Z-a1b2c3",
  "label": "1.4.0", "created_at": "2026-11-25T10:15:30Z",
  "entry": {"path": "index.html", "key": "games/demo/releases/20261125T101530Z-a1b2c3/index.html",
            "size": 1043, "sha256": "…", "content_type": "text/html; charset=utf-8"},
  "files": {"js/app.js": {"key": "games/demo/assets/<sha256>/app.js", "size": 52133,
                          "sha256": "…", "content_type": "text/javascript; charset=utf-8"}}
}
```

Content types come from Go's `mime.TypeByExtension` (host MIME tables may add
types), defaulting to `application/octet-stream`.

## Other CLI commands

| Command | Behaviour | Exit codes |
|---|---|---|
| `assets list --game G` | releases newest first, `*` = current | 0 |
| `assets rollback --game G --to R` | `Stat`s every object of R (exists, recorded size), then switches the pointer; rolling forward works the same | 0; 1 when refused (pruned, incomplete, unknown) |
| `assets verify --game G [--release R]` | downloads every object of R (default current): manifest, entry and files; checks size and SHA-256 against the database and that the stored manifest lists exactly the recorded files | 0 ok; 3 problems (`missing`, `size_mismatch`, `sha256_mismatch`, `manifest_mismatch`, `read_error`) |
| `assets prune --game G --keep N [--yes]` | keeps the newest N published releases **plus the current one**; marks the others `pruned` and deletes every recorded object that no kept release references (including leftovers of failed publishes). Dry run unless `--yes`. Holds the publish lease while deleting. | 0; 3 when some deletes failed (rows stay `deleting`, rerun) |

`--json` prints machine-readable results for all of them. The CLI needs
`storage.*` and the database; the `assets` game module only mounts the HTTP routes.

## HTTP API (`game.modules: [assets]`, requires `storage.driver`)

| Route | Auth | Response |
|---|---|---|
| `GET /v1/games/{game}/assets/manifest` | public | 200 manifest JSON; `ETag: "<release id>"`; `Cache-Control: public, max-age=<manifest_max_age>`; `If-None-Match` → 304; 404 `not_found` before the first publish |
| `GET /v1/games/{game}/assets/entry` | public | 302 to `public_base_url + entry key` (`Cache-Control: public, max-age=…`), or without `public_base_url` to a presigned GET URL valid `entry_url_ttl` (default 10 min, `Cache-Control: no-store`); 404 before the first publish |
| `GET /v1/admin/games/{game}/assets/releases` | admin key | `{"current_release_id", "releases": [...]}` (max 200) |
| `POST /v1/admin/games/{game}/assets/rollback` | admin key | body `{"release_id"}` → `{"current_release_id","previous_release_id"}`; 404 unknown release, 409 `release_pruned`, 409 `release_incomplete`, 400 invalid body |

The manifest response is read from the object store once per release and
cached in memory (immutable per key, at most 32 renderings). With
`public_base_url` every file has `url = base_url + key` and the response has
`base_url`; without it no URLs are included (clients need a CDN/bucket URL to
load files).

## Configuration

```yaml
kits: [game]
game:
  modules: [assets]
  assets:
    public_base_url: https://cdn.example.com/   # optional; http(s), no query/fragment; https in production
    manifest_max_age: 60s                       # >= 0
    entry_url_ttl: 10m                          # 0..168h, presigned entry only
storage:
  driver: s3                                    # required by the assets module
  s3: {...}
```

## Data model (migration 0013_assets)

- `assets_releases(game_id, release_id, label, manifest_key/size/sha256,
  entry_path, entry_key/size/sha256, file_count, total_bytes, created_at,
  created_by, status published|pruned)`
- `assets_release_files(game_id, release_id, path, object_key, size, sha256, content_type)` — deleted when the release is pruned
- `assets_objects(game_id, object_key, sha256, size, state live|deleting, created_at)` — every object the publisher stored; dedupe and prune never list the bucket
- `assets_state(game_id, current_release, lock_holder, lock_expires_at, updated_at, updated_by)`

## Verification status (2026-11-25)

- `go test -race ./modules/assets/` (SQLite and PostgreSQL 17): publish →
  verify → second publish with changed files (unchanged files not re-uploaded,
  counted at the storage wrapper) → rollback → verify → prune keeps objects of
  the kept and current releases and deletes the rest; injected storage failure
  on an asset, on the manifest and on the entry leaves the previous release
  current and the retry reuses uploaded objects; concurrent publish → `ErrBusy`;
  a publish whose lease expired and was taken over cannot switch the pointer;
  immutable keys are never overwritten; verify reports missing and corrupt
  objects and rollback refuses an incomplete release; HTTP manifest/ETag/304,
  entry redirect (CDN and presigned, presigned URL fetched), admin list/rollback,
  errors. `app`: CLI end to end and config validation.
- **Real MinIO** (`scripts/drill/assets_minio.sh`, local MinIO built from
  source): publish (1 MiB + small files), Cache-Control stored and returned on
  presigned GET, HEAD-based dedupe, rollback, verify and prune all pass.
- **Verified on Cloudflare R2** (2026-11-25, `TestLiveAssetsS3`: publish → verify → second publish with dedupe → rollback → prune). **Not verified**: AWS S3, a real CDN in front of the bucket
  (cache behaviour, purge), large builds (thousands of files, upload time),
  client-side loading from the manifest.
