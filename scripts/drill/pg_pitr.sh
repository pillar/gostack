#!/usr/bin/env bash
# Local PostgreSQL PITR drill for GoStack profile scale (PRD §11/§12): a
# throwaway primary with WAL archiving (deploy/postgres), base backup and
# logical snapshot under HTTP write load from a generated game app, SIGKILL of
# the primary, recovery of new clusters to a target time T and to the end of
# the archive, app restart, backup verify / reconcile and HTTP read-back of
# every acknowledged write. Prints a JSON summary on stdout (progress on
# stderr). See tests/drill/pgpitr and docs/DISASTER_RECOVERY.md.
#
# Environment (all optional):
#   PG_BIN=/opt/homebrew/bin   directory with initdb, postgres, pg_basebackup,
#                              pg_verifybackup, pg_dump, pg_restore (default: from PATH)
#   RUNS=2 DURATION=60s WORKERS=8 THINK=0s PAYLOAD=2048 ARCHIVE_TIMEOUT=10s
#   KEEP_WORK=1                keep the work dir (logs, clusters, archive)
#
# Clusters listen on 127.0.0.1 only (random ports, no Unix sockets) with
# scram-sha-256 auth; passwords are random per run and passed only through
# environment variables.
set -euo pipefail
REPO=$(cd "$(dirname "$0")/../.." && pwd)
PG_BIN=${PG_BIN:-$(dirname "$(command -v initdb || echo /nonexistent/initdb)")}
for b in initdb postgres pg_basebackup pg_dump pg_restore; do
  [ -x "$PG_BIN/$b" ] || {
    echo "$b not found in $PG_BIN (set PG_BIN)" >&2
    exit 1
  }
done
# Short path: the work dir holds PostgreSQL data directories.
WORK=$(mktemp -d /tmp/gspitr.XXXXXX)
cleanup() {
  # Stop any server left behind by an aborted run.
  for d in "$WORK"/r*/primary "$WORK"/r*/rec-*; do
    [ -f "$d/postmaster.pid" ] && "$PG_BIN/pg_ctl" -D "$d" stop -m immediate >/dev/null 2>&1 || true
  done
  if [ "${KEEP_WORK:-0}" = 1 ]; then echo "work dir kept: $WORK" >&2; else rm -rf "$WORK"; fi
}
trap cleanup EXIT

echo "building gostack CLI, drill app and drill driver in $WORK" >&2
mkdir -p "$WORK/bin"
(cd "$REPO" && go build -o "$WORK/bin/gostack" ./cmd/gostack && go build -o "$WORK/bin/pgpitr" ./tests/drill/pgpitr) >&2
(cd "$WORK" && "$WORK/bin/gostack" init drill --kit game --gostack-path "$REPO") >&2
(cd "$WORK/drill" && GOFLAGS=-mod=mod go mod tidy && go build -o "$WORK/bin/drillapp" .) >&2

PATH="$PG_BIN:$PATH" "$WORK/bin/pgpitr" -app "$WORK/bin/drillapp" -repo "$REPO" -pgbin "$PG_BIN" \
  -work "$WORK" -runs "${RUNS:-2}" -duration "${DURATION:-60s}" -workers "${WORKERS:-8}" \
  -think "${THINK:-0s}" -payload "${PAYLOAD:-2048}" -archive-timeout "${ARCHIVE_TIMEOUT:-10s}"
