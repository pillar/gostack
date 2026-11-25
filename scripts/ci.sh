#!/usr/bin/env bash
# Local/CI gate: formatting, vet, Go tests with race detector (incl. fault tests),
# linux cross-builds, and the TypeScript SDK checks.
#
# The Go suite always runs on SQLite. When GOSTACK_TEST_POSTGRES_DSN is set
# (a role that may CREATE DATABASE, e.g.
# postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable) it runs a second
# time with storage-backed tests on PostgreSQL (profile scale, see adapters/pgtest).
#
# GOSTACK_TEST_VALKEY_ADDR (host:port of a disposable Valkey, no password)
# enables the Valkey client/cache/limiter tests in both runs and, in the
# PostgreSQL run, switches profile scale apps to rate_limit.driver valkey and
# cache.driver valkey (see adapters/valkeytest); the PostgreSQL run is then
# repeated for app/, kits/ and tests/scale/ without Valkey so the local-limiter
# path on profile scale stays covered. GOSTACK_TEST_VALKEY_SERVER (path to
# valkey-server) additionally enables the server-restart test.
set -euo pipefail
cd "$(dirname "$0")/.."

unformatted=$(gofmt -l $(find . -name '*.go' -not -path './sdk/*'))
if [ -n "$unformatted" ]; then echo "gofmt needed:"; echo "$unformatted"; exit 1; fi
go vet ./...
env -u GOSTACK_TEST_POSTGRES_DSN go test -race -count=1 ./...
if [ -n "${GOSTACK_TEST_POSTGRES_DSN:-}" ]; then
  echo "ci: running the suite on PostgreSQL${GOSTACK_TEST_VALKEY_ADDR:+ + Valkey}"
  go test -race -count=1 ./...
  if [ -n "${GOSTACK_TEST_VALKEY_ADDR:-}" ]; then
    echo "ci: PostgreSQL without Valkey (local limiters, memory cache)"
    env -u GOSTACK_TEST_VALKEY_ADDR go test -race -count=1 ./app/... ./kits/... ./tests/scale/...
  fi
fi
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -o /dev/null ./cmd/gostack ./cmd/example
done
(cd sdk/typescript && npm ci --no-audit --no-fund && npm run typecheck && npm test && npm run size)
echo "ci: all checks passed"
