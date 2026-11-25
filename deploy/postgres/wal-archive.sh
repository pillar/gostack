#!/usr/bin/env bash
# PostgreSQL archive_command helper: copy one completed WAL segment (or
# .history/.backup file) into a local or mounted archive directory.
#
#   archive_command = '/opt/gostack/deploy/postgres/wal-archive.sh %p %f /var/lib/postgresql/wal-archive'
#
# Contract (PostgreSQL "Continuous Archiving"): exit 0 only once the file is
# durably stored; never overwrite an existing archive file with different
# content (a second cluster archiving into the same directory must fail
# loudly). Re-archiving an identical file succeeds (PostgreSQL may retry after
# a crash). The copy goes to a temporary name, is fsynced and renamed, so a
# reader (restore_command) never sees a partial segment.
#
# For off-machine archives use a tool built for it (pgBackRest, WAL-G) or point
# ARCHIVE_DIR at storage that survives loss of the database host; a directory
# on the same disk only protects against logical mistakes, not machine loss.
set -euo pipefail
[ $# -eq 3 ] || {
  echo "usage: wal-archive.sh %p %f ARCHIVE_DIR" >&2
  exit 2
}
src=$1 name=$2 dir=$3
case "$name" in */* | .* | "")
  echo "wal-archive: bad file name '$name'" >&2
  exit 2
  ;;
esac
[ -d "$dir" ] || {
  echo "wal-archive: archive directory $dir missing" >&2
  exit 1
}
dst="$dir/$name"
if [ -e "$dst" ]; then
  if cmp -s "$src" "$dst"; then
    exit 0
  fi
  echo "wal-archive: $dst exists with different content; refusing to overwrite (two clusters archiving to one directory?)" >&2
  exit 1
fi
tmp="$dir/.$name.tmp.$$"
trap 'rm -f "$tmp"' EXIT
cp "$src" "$tmp"
# fsync the file (GNU coreutils: sync FILE); fall back to a global sync.
sync "$tmp" 2>/dev/null || sync
mv "$tmp" "$dst"
trap - EXIT
sync "$dir" 2>/dev/null || sync
