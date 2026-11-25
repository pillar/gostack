#!/usr/bin/env bash
# PostgreSQL restore_command helper: fetch one archived file for recovery.
#
#   restore_command = '/opt/gostack/deploy/postgres/wal-restore.sh %f %p /var/lib/postgresql/wal-archive'
#
# Exits non-zero when the file is not in the archive; PostgreSQL treats that
# as "end of archive" (normal at the end of recovery) and also asks for
# .history files that may not exist.
set -euo pipefail
[ $# -eq 3 ] || {
  echo "usage: wal-restore.sh %f %p ARCHIVE_DIR" >&2
  exit 2
}
name=$1 dst=$2 dir=$3
case "$name" in */* | .* | "") exit 1 ;; esac
[ -f "$dir/$name" ] || exit 1
cp "$dir/$name" "$dst"
