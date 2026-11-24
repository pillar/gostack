#!/usr/bin/env bash
# Local/CI gate: formatting, vet, Go tests with race detector (incl. fault tests),
# linux cross-builds, and the TypeScript SDK checks.
set -euo pipefail
cd "$(dirname "$0")/.."

unformatted=$(gofmt -l $(find . -name '*.go' -not -path './sdk/*'))
if [ -n "$unformatted" ]; then echo "gofmt needed:"; echo "$unformatted"; exit 1; fi
go vet ./...
go test -race -count=1 ./...
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -o /dev/null ./cmd/gostack ./cmd/example
done
(cd sdk/typescript && npm ci --no-audit --no-fund && npm run typecheck && npm test && npm run size)
echo "ci: all checks passed"
