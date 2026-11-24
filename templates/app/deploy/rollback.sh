#!/usr/bin/env bash
# Switch a GoStack application back to an earlier release binary.
#
#   rollback.sh --name myapp [--to v1.2.3] [--health-url URL] [--timeout 60]
#
# Default target is `previous`. The target binary must understand the live
# database: its `migrate status --json` must report no drift (no migrations
# newer than it knows, no checksum changes). Otherwise the rollback is refused
# — a schema change cannot be undone by swapping binaries; restore the
# pre-upgrade snapshot instead (docs/RUNBOOK.md#failed-upgrade).
#
# Exit codes: 0 rolled back; 1 refused or failed (current restored).
source "$(dirname "$0")/lib.sh"

NAME="" TARGET=""
HEALTH_URL="http://127.0.0.1:8080/readyz" TIMEOUT=60
while [ $# -gt 0 ]; do
  case "$1" in
  --name) NAME="$2" ;;
  --to) TARGET="$2" ;;
  --health-url) HEALTH_URL="$2" ;;
  --timeout) TIMEOUT="$2" ;;
  *) die "unknown argument $1" ;;
  esac
  shift 2
done
[ -n "$NAME" ] || die "--name is required"
check_name "$NAME"
require_root
set_paths "$NAME"
[ -L "$APP_DIR/current" ] || die "$NAME is not installed"
CUR="$(release_of "$APP_DIR/current")"
if [ -z "$TARGET" ]; then
  TARGET="$(release_of "$APP_DIR/previous")" || die "no previous release recorded; pass --to VERSION"
fi
check_version "$TARGET"
[ "$TARGET" != "$CUR" ] || die "$TARGET is already current"
TARGET_BIN="$APP_DIR/releases/$TARGET/$NAME"
[ -x "$TARGET_BIN" ] || die "release $TARGET not found at $TARGET_BIN"
WANT="$(awk '{print $1}' "$APP_DIR/releases/$TARGET/SHA256SUMS")"
verify_sha "$TARGET_BIN" "$WANT"

PENDING_MIGRATIONS=""
preflight "$TARGET_BIN"

link_atomic "releases/$CUR" "$APP_DIR/previous"
link_atomic "releases/$TARGET" "$APP_DIR/current"
log "current -> releases/$TARGET (previous -> releases/$CUR)"
systemctl restart "$NAME"
if health_check "$HEALTH_URL" "$TIMEOUT"; then
  log "rolled back $NAME $CUR -> $TARGET; ready"
  exit 0
fi
log "$TARGET failed the health check; switching back to $CUR"
link_atomic "releases/$CUR" "$APP_DIR/current"
link_atomic "releases/$TARGET" "$APP_DIR/previous"
systemctl restart "$NAME"
die "rollback to $TARGET failed; $CUR restored. Inspect: journalctl -u $NAME"
