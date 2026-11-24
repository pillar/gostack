# AGENTS.md — GoStack

Product baseline: `docs/GoStack_PRD_v2.0.md` and revisions in `docs/PRD_v2_REVIEW.md` (local only, not in the public repo); HTTP contract `docs/GAME_API.md`, implementation status `docs/STATUS.md` (only verified capabilities are listed as done).

## Stack (verified 2026-11-24)
- Go 1.27.1 minimum, toolchain pinned to go1.27.4 (govulncheck: stdlib fixes), module `github.com/pillar/gostack` (public GitHub repository).
- SQLite via `modernc.org/sqlite` v1.63.0 (pure Go, SQLite 3.54.1, FTS5 available, `CGO_ENABLED=0`). YAML via `go.yaml.in/yaml/v3`. No other Go deps.
- TypeScript SDK: `sdk/typescript`, Node 24, devDeps typescript + esbuild only, npm with lockfile.

## Commands
- Full gate: `./scripts/ci.sh` (gofmt, vet, `go test -race ./...`, linux amd64/arm64 builds, SDK typecheck/test/size).
- Go only: `go vet ./... && go test -race ./...`; skip the process-kill test with `-short`.
- Targeted: `go test -race ./kits/game/`, `./modules/jobs/`, `./adapters/sqlite/`, `./tests/fault/`.
- SDK: `cd sdk/typescript && npm run typecheck && npm test && npm run size`.
- Live SDK check: run a server, then `GOSTACK_URL=http://127.0.0.1:8080 GAME_ID=demo node sdk/typescript/examples/e2e.mjs` (needs `npm run build`).
- CLI: `go build -o bin/gostack ./cmd/gostack`; `bin/gostack init <name> --kit game --gostack-path .`.

## Layout
- `adapters/sqlite` storage (single writer, read-only pool, PRAGMAs, lock, migrations runner)
- `core/{config,httpx,auth,observability}`; `modules/{jobs,idempotency,ratelimit}`; `kits/game`
- `app` wiring + application subcommands; `cmd/gostack` CLI (adds `init`); `cmd/example` reference app
- `migrations/{sqlite,analytics}` embedded, forward-only; `templates/app` project skeleton (embedded)
- `tests/fault` process-level fault injection; `docs/adr` decisions

## Rules
- Never edit an applied migration; add `NNNN_name.sql`. The runner refuses checksum drift.
- Writes go through `sqlite.DB.WriteTx` (bounded admission, BEGIN IMMEDIATE). No network IO inside a transaction; WriteTx may re-run fn on SQLITE_BUSY, so fn must be re-runnable.
- game_id/player_id come from the session, never from request bodies. Money/quantities are integers; ledger is append-only (DB triggers).
- Mutations that clients retry use `idempotency.Do` in the same transaction as the business change.
- Do not add adapters, profiles or kits that are not implemented; config validation must reject them.
- Do not fabricate benchmark or device results. Performance budgets (PRD §13/§23) are unverified until run on the target 1C1G environment.
