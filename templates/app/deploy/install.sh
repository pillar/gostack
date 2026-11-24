#!/usr/bin/env bash
# First-time install of a GoStack application on Linux/systemd.
#
#   install.sh --name myapp --binary ./myapp-linux-amd64 --version v1.2.3 \
#              --sums SHA256SUMS --unit deploy/systemd/myapp.service --config gostack.yaml \
#              [--health-url http://127.0.0.1:8080/readyz] [--timeout 60]
#
# Steps: verify sha256 → system user → directories → stage releases/<version>
# → config/env (never overwritten) → preflight (doctor, migrate status) →
# systemd unit → atomic `current` switch → start → /readyz health check.
# Refuses to run when `current` already exists (use upgrade.sh).
source "$(dirname "$0")/lib.sh"

NAME="" BINARY="" VERSION="" SHA="" SUMS="" UNIT="" CFG_SRC=""
HEALTH_URL="http://127.0.0.1:8080/readyz" TIMEOUT=60
while [ $# -gt 0 ]; do
  case "$1" in
  --name) NAME="$2" ;;
  --binary) BINARY="$2" ;;
  --version) VERSION="$2" ;;
  --sha256) SHA="$2" ;;
  --sums) SUMS="$2" ;;
  --unit) UNIT="$2" ;;
  --config) CFG_SRC="$2" ;;
  --health-url) HEALTH_URL="$2" ;;
  --timeout) TIMEOUT="$2" ;;
  *) die "unknown argument $1" ;;
  esac
  shift 2
done
[ -n "$NAME" ] && [ -n "$BINARY" ] && [ -n "$VERSION" ] && [ -n "$UNIT" ] || die "--name, --binary, --version and --unit are required"
check_name "$NAME"
check_version "$VERSION"
require_root
set_paths "$NAME"
[ -f "$BINARY" ] || die "binary $BINARY not found"
[ -f "$UNIT" ] || die "unit file $UNIT not found"
if [ -e "$APP_DIR/current" ]; then
  die "$NAME is already installed (current -> $(release_of "$APP_DIR/current")); use upgrade.sh"
fi

WANT="$(expected_sha "$BINARY" "$SHA" "$SUMS")"
verify_sha "$BINARY" "$WANT"

if ! id -u "$NAME" >/dev/null 2>&1; then
  log "creating system user $NAME"
  useradd --system --user-group --home-dir "/var/lib/$NAME" --no-create-home --shell /usr/sbin/nologin "$NAME"
fi

mkdir -p "$APP_DIR/releases" "$ETC_DIR" "$DATA_DIR/backups"
chmod 0755 "$APP_DIR" "$APP_DIR/releases"
chmod 0750 "$ETC_DIR" "$DATA_DIR" "$DATA_DIR/backups"
chown "root:$NAME" "$ETC_DIR"
chown "$NAME:$NAME" "$DATA_DIR" "$DATA_DIR/backups"

if [ ! -e "$CONFIG" ]; then
  [ -n "$CFG_SRC" ] || die "$CONFIG does not exist; pass --config FILE"
  cp "$CFG_SRC" "$CONFIG.tmp.$$"
  chmod 0640 "$CONFIG.tmp.$$"
  chown "root:$NAME" "$CONFIG.tmp.$$"
  mv "$CONFIG.tmp.$$" "$CONFIG"
  log "installed $CONFIG"
else
  log "keeping existing $CONFIG"
fi
if [ ! -e "$ENV_FILE" ]; then
  cat >"$ENV_FILE.tmp.$$" <<EOF
# Service environment for $NAME (EnvironmentFile of the systemd unit; also
# sourced by the deploy scripts so preflight checks see the same settings).
GOSTACK_ENVIRONMENT=production
GOSTACK_DATABASE_PATH=$DATA_DIR/app.db
GOSTACK_DATABASE_ANALYTICS_PATH=$DATA_DIR/analytics.db
EOF
  chmod 0640 "$ENV_FILE.tmp.$$"
  chown "root:$NAME" "$ENV_FILE.tmp.$$"
  mv "$ENV_FILE.tmp.$$" "$ENV_FILE"
  log "installed $ENV_FILE"
fi

stage_release "$VERSION" "$BINARY" "$WANT"
NEW_BIN="$APP_DIR/releases/$VERSION/$NAME"
preflight "$NEW_BIN"

mkdir -p "$UNIT_DIR"
cp "$UNIT" "$UNIT_DIR/$NAME.service.tmp.$$"
chmod 0644 "$UNIT_DIR/$NAME.service.tmp.$$"
mv "$UNIT_DIR/$NAME.service.tmp.$$" "$UNIT_DIR/$NAME.service"
systemctl daemon-reload
systemctl enable "$NAME"

link_atomic "releases/$VERSION" "$APP_DIR/current"
log "current -> releases/$VERSION"
systemctl restart "$NAME"
if ! health_check "$HEALTH_URL" "$TIMEOUT"; then
  die "$NAME did not become ready at $HEALTH_URL within ${TIMEOUT}s; inspect: journalctl -u $NAME -n 200"
fi
log "installed $NAME $VERSION; ready"
