#!/usr/bin/env bash
# Restore a GoStack database from its Litestream replica to a NEW path and
# verify it. Never overwrites anything; the live database is not touched.
#
#   restore.sh --bin /opt/myapp/current/myapp --replica-config /etc/litestream.yml \
#              --db /var/lib/myapp/app.db --to /var/lib/myapp/restore/app-20261125.db \
#              [--timestamp 2026-11-25T03:00:00Z] [--owner myapp]
#
# --db is the database path as written in the Litestream config (it selects
# the replica); --to is where the restored copy is written. The script runs
# `litestream restore -config CFG -o TO [-timestamp T] DB` and then
# `BIN backup verify TO` (integrity_check, schema vs binary, ledger reconcile).
# Exercised with Litestream v0.5.18 and a local file replica by
# scripts/drill/litestream_minio.sh (see docs/DISASTER_RECOVERY.md for which
# replica types were verified); re-check the restore flags when you pin a
# different Litestream version.
#
# Re-runnable: a failed run removes only the partial file it created at --to.
# Exit codes: 0 restored and verified; 1 refused or restore failed; 3 restored
# but verification failed (file kept at --to for inspection, do NOT switch to it).
set -euo pipefail

log() { printf '[restore] %s\n' "$*" >&2; }
die() {
  log "ERROR: $*"
  exit 1
}

BIN="" CFG="" DB="" TO="" TS="" OWNER=""
while [ $# -gt 0 ]; do
  case "$1" in
  --bin) BIN="$2" ;;
  --replica-config) CFG="$2" ;;
  --db) DB="$2" ;;
  --to) TO="$2" ;;
  --timestamp) TS="$2" ;;
  --owner) OWNER="$2" ;;
  *) die "unknown argument $1" ;;
  esac
  shift 2
done
[ -n "$BIN" ] && [ -n "$CFG" ] && [ -n "$DB" ] && [ -n "$TO" ] || die "--bin, --replica-config, --db and --to are required"
[ -x "$BIN" ] || die "$BIN is not executable"
command -v litestream >/dev/null 2>&1 || die "litestream not found on PATH"
[ "$TO" != "$DB" ] || die "--to must differ from --db (restore never overwrites)"
for p in "$TO" "$TO-wal" "$TO-shm"; do
  [ ! -e "$p" ] || die "$p exists; choose a new --to path"
done
mkdir -p "$(dirname "$TO")"

created=0
cleanup() {
  if [ "$created" = 1 ]; then
    rm -f "$TO" "$TO-wal" "$TO-shm"
  fi
}
trap cleanup EXIT

args=(restore -config "$CFG" -o "$TO")
if [ -n "$TS" ]; then
  args+=(-timestamp "$TS")
fi
log "litestream ${args[*]} $DB"
created=1
litestream "${args[@]}" "$DB" || die "litestream restore failed"
[ -s "$TO" ] || die "litestream produced no file at $TO"
created=0 # keep the file from here on, even if verification fails

if [ -n "$OWNER" ]; then
  chown "$OWNER:$OWNER" "$TO"
fi
chmod 0600 "$TO"

log "verifying $TO"
if ! "$BIN" --json backup verify "$TO"; then
  log "VERIFICATION FAILED: $TO is kept for inspection; do not switch to it"
  exit 3
fi

cat >&2 <<EOF
[restore] OK: $TO restored and verified.
Next steps (docs/DISASTER_RECOVERY.md):
  1. Make sure the old instance is stopped/isolated (no double writes).
  2. Point the service at the restored file: set GOSTACK_DATABASE_PATH=$TO in
     the service environment, or stop the service and rename it into place
     after moving (not deleting) the old database and its -wal/-shm aside.
  3. Start the service, check /readyz, run \`game reconcile\` and spot checks.
  4. Re-enable replication for the new database path before taking traffic.
EOF
