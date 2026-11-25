#!/usr/bin/env bash
# Upgrade a GoStack application installed by install.sh.
#
#   upgrade.sh --name myapp --binary ./myapp-linux-amd64 --version v1.3.0 --sums SHA256SUMS \
#              [--health-url http://127.0.0.1:8080/readyz] [--timeout 60]
#
# Steps: verify sha256 → stage releases/<version> → preflight of the NEW binary
# (doctor; refuse if the database is newer than it) → online snapshot of the
# database (backup snapshot + backup verify) → atomic switch of `current`
# (`previous` keeps the old release) → restart (the new binary applies pending
# migrations on start) → /readyz health check.
#
# On a failed health check the `current` link is switched back and the old
# release restarted. If the new release already applied migrations the old
# binary refuses to start (schema newer than it knows); the script then exits 3
# and prints the pre-upgrade snapshot to restore (docs/RUNBOOK.md#failed-upgrade).
#
# Exit codes: 0 upgraded; 1 refused/failed before switching, or rolled back to
# the old release which is healthy; 3 rolled back but the old release is NOT
# healthy (manual action required).
source "$(dirname "$0")/lib.sh"

NAME="" BINARY="" VERSION="" SHA="" SUMS=""
HEALTH_URL="http://127.0.0.1:8080/readyz" TIMEOUT=60
while [ $# -gt 0 ]; do
  case "$1" in
  --name) NAME="$2" ;;
  --binary) BINARY="$2" ;;
  --version) VERSION="$2" ;;
  --sha256) SHA="$2" ;;
  --sums) SUMS="$2" ;;
  --health-url) HEALTH_URL="$2" ;;
  --timeout) TIMEOUT="$2" ;;
  *) die "unknown argument $1" ;;
  esac
  shift 2
done
[ -n "$NAME" ] && [ -n "$BINARY" ] && [ -n "$VERSION" ] || die "--name, --binary and --version are required"
check_name "$NAME"
check_version "$VERSION"
require_root
set_paths "$NAME"
[ -L "$APP_DIR/current" ] || die "$NAME is not installed (no $APP_DIR/current); use install.sh"
OLD="$(release_of "$APP_DIR/current")"
OLD_PREV="$(release_of "$APP_DIR/previous" || true)"
[ "$OLD" != "$VERSION" ] || die "$VERSION is already current"

WANT="$(expected_sha "$BINARY" "$SHA" "$SUMS")"
verify_sha "$BINARY" "$WANT"
stage_release "$VERSION" "$BINARY" "$WANT"
NEW_BIN="$APP_DIR/releases/$VERSION/$NAME"

PENDING_MIGRATIONS=""
preflight "$NEW_BIN"
if [ -n "$PENDING_MIGRATIONS" ]; then
  log "this upgrade applies migrations [$PENDING_MIGRATIONS]; after that, binary rollback to $OLD is refused"
fi

SNAP=""
DB_PATH="$(set -a && . "$ENV_FILE" && printf '%s' "${GOSTACK_DATABASE_PATH:-}")"
if [ -n "$DB_PATH" ] && [ -e "$DB_PATH" ]; then
  SNAP="$DATA_DIR/backups/pre-upgrade-$OLD-to-$VERSION-$(date -u +%Y%m%dT%H%M%SZ).db"
  log "snapshot -> $SNAP"
  as_app "$NEW_BIN" --config "$CONFIG" backup snapshot --to "$SNAP" >&2 || die "pre-upgrade snapshot failed; not upgrading"
  as_app "$NEW_BIN" --config "$CONFIG" backup verify "$SNAP" >&2 || die "pre-upgrade snapshot failed verification; not upgrading"
else
  log "no database yet; skipping snapshot"
fi

link_atomic "releases/$OLD" "$APP_DIR/previous"
link_atomic "releases/$VERSION" "$APP_DIR/current"
log "current -> releases/$VERSION (previous -> releases/$OLD)"
restart_service
if health_check "$HEALTH_URL" "$TIMEOUT"; then
  log "upgraded $NAME $OLD -> $VERSION; ready"
  exit 0
fi

log "$VERSION failed the health check; rolling back to $OLD"
link_atomic "releases/$OLD" "$APP_DIR/current"
if [ -n "$OLD_PREV" ]; then
  link_atomic "releases/$OLD_PREV" "$APP_DIR/previous"
else
  rm -f "$APP_DIR/previous"
fi
restart_service
if health_check "$HEALTH_URL" "$TIMEOUT"; then
  die "upgrade to $VERSION failed; rolled back to $OLD (healthy). Inspect: journalctl -u $NAME"
fi
log "ERROR: rolled back to $OLD but it is not healthy either."
if [ -n "$PENDING_MIGRATIONS" ] && [ -n "$SNAP" ]; then
  log "migrations [$PENDING_MIGRATIONS] were probably applied by $VERSION; $OLD cannot serve that schema."
  log "restore the pre-upgrade snapshot (data written after it is lost): $SNAP"
  log "see docs/RUNBOOK.md#failed-upgrade"
fi
exit 3
