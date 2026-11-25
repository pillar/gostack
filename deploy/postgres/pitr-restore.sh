#!/usr/bin/env bash
# Prepare a NEW PostgreSQL data directory for point-in-time recovery from a
# base backup (base-backup.sh) plus the WAL archive (wal-archive.sh).
#
#   pitr-restore.sh --base /var/backups/postgresql/base-20261125T0300 \
#       --archive /var/lib/postgresql/wal-archive --to /var/lib/postgresql/17/restore \
#       [--target-time '2026-11-25 10:15:00.000000+00'] [--restore-helper PATH]
#
# Without --target-time, recovery replays every archived segment (the latest
# state the archive holds). With it, replay stops before the first transaction
# that committed after the target time (recovery_target_inclusive=on keeps a
# commit at exactly the target) and the server promotes. The target must be
# after the end of the base backup, or the server refuses to start.
#
# The script copies the base backup into --to (must not exist), writes
# recovery.signal and the recovery settings to postgresql.auto.conf, and turns
# archive_mode off in the restored cluster so it cannot push a new timeline
# into the archive you are recovering from. It does not start the server:
#
#   pg_ctl -D TO -o "-p PORT" -l TO/recovery.log start
#   psql -c 'SELECT pg_is_in_recovery()'   # f once recovery finished and promoted
#
# Then run `APP backup snapshot --to X` + `APP backup verify X` (or `APP game
# reconcile`) against it, point database.dsn_ref at it and start the app.
# After switching, re-enable archive_mode with a NEW archive directory and take
# a fresh base backup: the old archive belongs to the previous timeline.
set -euo pipefail
log() { printf '[pitr-restore] %s\n' "$*" >&2; }
die() {
  log "ERROR: $*"
  exit 1
}
BASE="" ARCHIVE="" TO="" TARGET="" HELPER="$(cd "$(dirname "$0")" && pwd)/wal-restore.sh"
while [ $# -gt 0 ]; do
  case "$1" in
  --base) BASE="$2" ;;
  --archive) ARCHIVE="$2" ;;
  --to) TO="$2" ;;
  --target-time) TARGET="$2" ;;
  --restore-helper) HELPER="$2" ;;
  *) die "unknown argument $1" ;;
  esac
  shift 2
done
[ -n "$BASE" ] && [ -n "$ARCHIVE" ] && [ -n "$TO" ] || die "--base, --archive and --to are required"
[ -f "$BASE/backup_label" ] || die "$BASE is not a plain-format base backup (no backup_label)"
[ -d "$ARCHIVE" ] || die "archive $ARCHIVE missing"
[ -x "$HELPER" ] || die "restore helper $HELPER not executable"
[ ! -e "$TO" ] || die "$TO exists; recovery always goes to a new data directory"
case "$TARGET" in *"'"* | *"\\"*) die "bad --target-time" ;; esac
case "$ARCHIVE$HELPER" in *"'"* | *" "*) die "archive and helper paths must not contain quotes or spaces" ;; esac
created=1
cleanup() { if [ "$created" = 1 ]; then rm -rf "$TO"; fi; }
trap cleanup EXIT
mkdir -p "$(dirname "$TO")"
cp -R "$BASE" "$TO"
chmod 0700 "$TO"
rm -f "$TO/postmaster.pid"
touch "$TO/recovery.signal"
{
  echo ""
  echo "# --- added by pitr-restore.sh $(date -u +%Y-%m-%dT%H:%M:%SZ) ---"
  echo "restore_command = '$HELPER %f %p $ARCHIVE'"
  echo "archive_mode = off"
  echo "recovery_target_action = 'promote'"
  if [ -n "$TARGET" ]; then
    echo "recovery_target_time = '$TARGET'"
    echo "recovery_target_inclusive = on"
  fi
} >>"$TO/postgresql.auto.conf"
created=0
log "prepared $TO (target: ${TARGET:-latest archived WAL}); start the server on it"
