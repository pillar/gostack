#!/usr/bin/env bash
# Local A08 analogue (PRD §11): Litestream replication of a generated GoStack
# game app to a throwaway MinIO, write load through the HTTP API, kill -9 of
# app + Litestream, restore with deploy/litestream/restore.sh into a new path,
# backup verify, restart on the restored DB and API read-back. Prints a JSON
# summary on stdout (progress on stderr). See docs/DISASTER_RECOVERY.md.
#
# Environment (all optional):
#   DRILL_REPLICA=s3|file     s3 (default) starts MinIO; file uses a local
#                             directory replica (no object store involved)
#   MINIO_BIN=minio           LITESTREAM_BIN=litestream (v0.5.x)
#   RUNS=2 DURATION=60s WORKERS=8 THINK=0s PAYLOAD=2048 SEED_PLAYERS=0
#   SYNC_INTERVAL=10s         comma list, cycled per run (e.g. 10s,1s)
#   WAL_AUTOCHECKPOINT=1000   comma list, cycled per run (e.g. 1000,0)
#   KEEP_WORK=1               keep the work dir (logs, replicas, DBs)
#   DRILL_S3_ENDPOINT=...     with DRILL_REPLICA=s3: use this existing S3 endpoint
#                             (e.g. Cloudflare R2) instead of starting MinIO; also
#                             DRILL_S3_BUCKET (existing), DRILL_S3_REGION (auto for
#                             R2) and LITESTREAM_ACCESS_KEY_ID/_SECRET_ACCESS_KEY.
#
# Credentials: MinIO root credentials are random per invocation and passed to
# Litestream through LITESTREAM_ACCESS_KEY_ID/LITESTREAM_SECRET_ACCESS_KEY in
# the environment only; no config file contains them.
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
. "$REPO/scripts/drill/minio_lib.sh"

DRILL_REPLICA=${DRILL_REPLICA:-s3}
LITESTREAM_BIN=$(command -v "${LITESTREAM_BIN:-litestream}") || { echo "litestream not found (set LITESTREAM_BIN)" >&2; exit 1; }
WORK=$(mktemp -d "${TMPDIR:-/tmp}/gostack-lsdrill.XXXXXX")
cleanup() {
  minio_stop
  if [ "${KEEP_WORK:-0}" = 1 ]; then echo "work dir kept: $WORK" >&2; else rm -rf "$WORK"; fi
}
trap cleanup EXIT

echo "building gostack CLI, drill app and drill driver in $WORK" >&2
mkdir -p "$WORK/bin"
(cd "$REPO" && go build -o "$WORK/bin/gostack" ./cmd/gostack && go build -o "$WORK/bin/drill" ./tests/drill/litestream) >&2
(cd "$WORK" && "$WORK/bin/gostack" init drill --kit game --gostack-path "$REPO") >&2
(cd "$WORK/drill" && GOFLAGS=-mod=mod go mod tidy && go build -o "$WORK/bin/drillapp" .) >&2

args=(-app "$WORK/bin/drillapp" -litestream "$LITESTREAM_BIN" -restore "$REPO/deploy/litestream/restore.sh"
  -work "$WORK/runs" -replica "$DRILL_REPLICA" -runs "${RUNS:-2}" -duration "${DURATION:-60s}"
  -workers "${WORKERS:-8}" -think "${THINK:-0s}" -payload "${PAYLOAD:-2048}" -seed-players "${SEED_PLAYERS:-0}"
  -sync-interval "${SYNC_INTERVAL:-10s}" -wal-autocheckpoint "${WAL_AUTOCHECKPOINT:-1000}")
mkdir -p "$WORK/runs"
if [ "$DRILL_REPLICA" = s3 ] && [ -n "${DRILL_S3_ENDPOINT:-}" ]; then
  echo "external S3 endpoint (bucket ${DRILL_S3_BUCKET:?DRILL_S3_BUCKET required})" >&2
  "$WORK/bin/drill" "${args[@]}" -s3-endpoint "$DRILL_S3_ENDPOINT" -s3-bucket "$DRILL_S3_BUCKET" \
    -s3-region "${DRILL_S3_REGION:-auto}" -s3-create-bucket=false
elif [ "$DRILL_REPLICA" = s3 ]; then
  minio_start "$WORK/minio-data" "$WORK/minio.log"
  echo "MinIO $("${MINIO_BIN:-minio}" --version 2>/dev/null | head -1) at $MINIO_ENDPOINT" >&2
  LITESTREAM_ACCESS_KEY_ID="$MINIO_KEY" LITESTREAM_SECRET_ACCESS_KEY="$MINIO_SECRET" \
    "$WORK/bin/drill" "${args[@]}" -s3-endpoint "$MINIO_ENDPOINT" -s3-bucket gostack-drill -s3-region us-east-1
else
  "$WORK/bin/drill" "${args[@]}"
fi
