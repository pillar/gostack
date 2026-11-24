#!/usr/bin/env bash
# Local/CI gate: formatting, vet, Go tests with race detector (incl. fault tests),
# linux cross-builds, and the TypeScript SDK checks.
#
# The Go suite always runs on SQLite. When GOSTACK_TEST_POSTGRES_DSN is set
# (a role that may CREATE DATABASE, e.g.
# postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable) it runs a second
# time with storage-backed tests on PostgreSQL (profile scale, see adapters/pgtest).
set -euo pipefail
cd "$(dirname "$0")/.."

unformatted=$(gofmt -l $(find . -name '*.go' -not -path './sdk/*'))
if [ -n "$unformatted" ]; then echo "gofmt needed:"; echo "$unformatted"; exit 1; fi
go vet ./...
env -u GOSTACK_TEST_POSTGRES_DSN go test -race -count=1 ./...
if [ -n "${GOSTACK_TEST_POSTGRES_DSN:-}" ]; then
  echo "ci: running the suite on PostgreSQL"
  go test -race -count=1 ./...
fi
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -o /dev/null ./cmd/gostack ./cmd/example
done
(cd sdk/typescript && npm ci --no-audit --no-fund && npm run typecheck && npm test && npm run size)
echo "ci: all checks passed"
