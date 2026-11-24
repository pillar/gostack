#!/usr/bin/env bash
# Build release artifacts: static linux binaries, SHA256SUMS and an SBOM per binary.
#
#   scripts/release.sh [--version v1.2.3] [--pkg PKG] [--name NAME] [--out dist] [--arches "amd64 arm64"]
#
# Run from the module root. PKG defaults to ./cmd/gostack in the GoStack repo
# and to . (the generated application's main package) elsewhere.
#
# Version defaults to `git describe --tags --always --dirty`. It is embedded via
# -X github.com/pillar/gostack/app.Version (also correct for generated apps,
# which import that package).
#
# SBOM format: NOT SPDX/CycloneDX. Each <binary>.sbom.json is the output of
# `go version -m -json <binary>`, i.e. Go's runtime/debug.BuildInfo as embedded
# in the binary (main module, every linked dependency module with version and
# go.sum hash, build settings incl. GOOS/GOARCH/CGO_ENABLED/-trimpath). It lists
# exactly what was linked, but needs conversion for tools that require SPDX.
set -euo pipefail

VERSION="" PKG="" NAME="" OUT="dist" ARCHES="amd64 arm64"
while [ $# -gt 0 ]; do
  case "$1" in
  --version) VERSION="$2" ;;
  --pkg) PKG="$2" ;;
  --name) NAME="$2" ;;
  --out) OUT="$2" ;;
  --arches) ARCHES="$2" ;;
  *)
    echo "unknown argument $1" >&2
    exit 2
    ;;
  esac
  shift 2
done
if [ -z "$PKG" ]; then
  if [ -d cmd/gostack ]; then PKG=./cmd/gostack; else PKG=.; fi
fi
if [ -z "$VERSION" ]; then
  VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
fi
[[ "$VERSION" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$ ]] || {
  echo "invalid version $VERSION" >&2
  exit 2
}
if [ -z "$NAME" ]; then
  NAME="$(basename "$PKG")"
  [ "$NAME" != "." ] || NAME="$(basename "$(pwd)")"
fi
mkdir -p "$OUT"
[ ! -e "$OUT/SHA256SUMS" ] || {
  echo "$OUT/SHA256SUMS exists; use a clean --out directory" >&2
  exit 1
}

sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi; }

files=()
for arch in $ARCHES; do
  bin="$NAME-$VERSION-linux-$arch"
  echo "building $OUT/$bin" >&2
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X github.com/pillar/gostack/app.Version=$VERSION" \
    -o "$OUT/$bin" "$PKG"
  go version -m -json "$OUT/$bin" >"$OUT/$bin.sbom.json"
  files+=("$bin" "$bin.sbom.json")
done
(cd "$OUT" && sha "${files[@]}" >SHA256SUMS)
echo "release $VERSION written to $OUT:" >&2
cat "$OUT/SHA256SUMS" >&2
