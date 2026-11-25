# AGENTS.md — GoStack

Product baseline: `docs/GoStack_PRD_v2.0.md` and revisions in `docs/PRD_v2_REVIEW.md` (local only, not in the public repo); HTTP contract `docs/GAME_API.md`, implementation status `docs/STATUS.md` (only verified capabilities are listed as done).

## Stack (verified 2026-11-24)
- Go 1.27.1 minimum, toolchain pinned to go1.27.4 (govulncheck: stdlib fixes), module `github.com/pillar/gostack` (public GitHub repository).
- SQLite via `modernc.org/sqlite` v1.63.0 (pure Go, SQLite 3.54.1, FTS5 available, `CGO_ENABLED=0`). YAML via `go.yaml.in/yaml/v3`.
- PostgreSQL (profile scale) via `github.com/jackc/pgx/v5` v5.13.0, database/sql stdlib driver only; tested on PostgreSQL 17.11 (local) / `postgres:17` (CI). Other direct deps: `github.com/coder/websocket`, `golang.org/x/crypto`. Add no others without an ADR.
- TypeScript SDK: `sdk/typescript`, Node 24, devDeps typescript + esbuild only, npm with lockfile.

## Commands
- Full gate: `./scripts/ci.sh` (gofmt, vet, `go test -race ./...`, linux amd64/arm64 builds, SDK typecheck/test/size).
- Go only: `go vet ./... && go test -race ./...`; skip the process-kill test with `-short`.
- PostgreSQL suite: `GOSTACK_TEST_POSTGRES_DSN=postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable go test -race -count=1 ./...` (role needs CREATEDB; each test gets a fresh C-collated `gs_test_*` database, dropped afterwards). `./scripts/ci.sh` runs it in addition to SQLite when the variable is set. Throwaway local server: `initdb -D $T -U postgres --auth=trust && pg_ctl -D $T -o "-p 55432 -k ''" -l $T/log start`.
- Targeted: `go test -race ./kits/game/`, `./modules/jobs/`, `./adapters/sqlite/`, `./tests/fault/`.
- SDK: `cd sdk/typescript && npm run typecheck && npm test && npm run size`.
- Live SDK check: run a server, then `GOSTACK_URL=http://127.0.0.1:8080 GAME_ID=demo node sdk/typescript/examples/e2e.mjs` (needs `npm run build`).
- CLI: `go build -o bin/gostack ./cmd/gostack`; `bin/gostack init <name> --kit game --gostack-path .`.

## Layout
- `adapters/store` driver-agnostic `*store.DB` (WriteTx/ReadTx, bounded admission, retry, migrations runner for both dialects)
- `adapters/sqlite` Lite opener (single writer, read-only pool, PRAGMAs, instance lock); `adapters/postgres` Scale opener (pgx, SERIALIZABLE retry, `?`→`$n` rewriting); `adapters/pgtest` test helpers
- `core/{config,httpx,auth,observability}`; `modules/{jobs,idempotency,ratelimit}`; `kits/game`
- `app` wiring + application subcommands; `cmd/gostack` CLI (adds `init`); `cmd/example` reference app
- `migrations/{sqlite,analytics}` and their PostgreSQL twins `migrations/{postgres,postgres_analytics}` (same version numbers), embedded, forward-only; `templates/app` project skeleton (embedded)
- `tests/fault` process-level fault injection (SQLite); `tests/scale` two instances on one PostgreSQL + `migrate to-postgres` end-to-end; `docs/adr` decisions

## Rules
- Never edit an applied migration; add `NNNN_name.sql` to `migrations/sqlite` AND its twin to `migrations/postgres` (same name). The runner refuses checksum drift; `TestSchemaEquivalence` (PG suite) compares both schemas.
- Writes go through `store.DB.WriteTx` (bounded admission; SQLite BEGIN IMMEDIATE, PostgreSQL SERIALIZABLE). No network IO inside a transaction; WriteTx re-runs fn on SQLITE_BUSY / serialization failures, so fn must be re-runnable.
- Portable SQL only: `?` placeholders, `ON CONFLICT` (not `INSERT OR`), table-qualified columns inside `DO UPDATE SET`, no `rowid`, no scalar `min/max(a,b)`, every non-aggregated column in `GROUP BY` (or group by the primary key), explicit `ORDER BY` for any list, integers as BIGINT, bytes as BLOB/BYTEA. After a failed statement a PostgreSQL transaction is aborted: do not catch a SQL error and keep using the same tx.
- game_id/player_id come from the session, never from request bodies. Money/quantities are integers; ledger is append-only (DB triggers).
- Mutations that clients retry use `idempotency.Do` in the same transaction as the business change.
- Do not add adapters, profiles or kits that are not implemented; config validation must reject them.
- Do not fabricate benchmark or device results. Performance budgets (PRD §13/§23) are unverified until run on the target 1C1G environment.
