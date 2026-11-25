#!/usr/bin/env bash
# Take a physical base backup for point-in-time recovery and verify it.
#
#   PGPASSWORD=... base-backup.sh --host 127.0.0.1 --port 5432 --user replicator \
#       --to /var/backups/postgresql/base-20261125T0300
#
# The role needs the REPLICATION attribute (and a pg_hba "replication" entry).
# Pass the password through PGPASSWORD or ~/.pgpass, never on the command line.
# Runs pg_basebackup in plain format with the WAL needed for consistency
# streamed into the backup (-X stream: the backup is restorable on its own),
# a fast checkpoint and a SHA-256 backup manifest, then pg_verifybackup.
# --to must not exist; a failed run removes it. Prints a JSON line with the
# backup's start WAL position and time (from backup_label) on stdout.
#
# WAL archiving (postgresql.pitr.conf) must already be running: recovering to
# a time after the backup needs every WAL segment from the backup's start on.
set -euo pipefail
log() { printf '[base-backup] %s\n' "$*" >&2; }
die() {
  log "ERROR: $*"
  exit 1
}
HOST=127.0.0.1 PORT=5432 USER_=postgres TO="" LABEL=gostack-base
while [ $# -gt 0 ]; do
  case "$1" in
  --host) HOST="$2" ;;
  --port) PORT="$2" ;;
  --user) USER_="$2" ;;
  --to) TO="$2" ;;
  --label) LABEL="$2" ;;
  *) die "unknown argument $1" ;;
  esac
  shift 2
done
[ -n "$TO" ] || die "--to is required"
[ ! -e "$TO" ] || die "$TO exists; choose a new directory"
command -v pg_basebackup >/dev/null || die "pg_basebackup not on PATH"
created=1
cleanup() { if [ "$created" = 1 ]; then rm -rf "$TO"; fi; }
trap cleanup EXIT
start=$(date +%s)
pg_basebackup -h "$HOST" -p "$PORT" -U "$USER_" -w -D "$TO" -Fp -X stream -c fast \
  --manifest-checksums=SHA256 -l "$LABEL" >&2 || die "pg_basebackup failed"
verified=false
if command -v pg_verifybackup >/dev/null; then
  pg_verifybackup -q "$TO" >&2 || die "pg_verifybackup failed"
  verified=true
else
  log "pg_verifybackup not found; backup not verified"
fi
created=0
start_wal=$(sed -n 's/^START WAL LOCATION: \([^ ]*\).*/\1/p' "$TO/backup_label")
start_time=$(sed -n 's/^START TIME: //p' "$TO/backup_label")
printf '{"path":"%s","label":"%s","start_wal":"%s","start_time":"%s","seconds":%s,"verified":%s}\n' \
  "$TO" "$LABEL" "$start_wal" "$start_time" "$(($(date +%s) - start))" "$verified"
