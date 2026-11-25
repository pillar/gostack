# shellcheck shell=bash
# Shared helpers for the GoStack deploy scripts (install/upgrade/rollback).
#
# Target: Linux with systemd and util-linux (runuser). Layout for app NAME:
#   /opt/NAME/releases/<version>/NAME   immutable release binaries (root-owned)
#   /opt/NAME/current  -> releases/<v>  symlink, switched atomically
#   /opt/NAME/previous -> releases/<v>  last release before the switch
#   /etc/NAME/gostack.yaml              configuration (0640 root:NAME)
#   /etc/NAME/gostack.env               service environment (EnvironmentFile)
#   /var/lib/NAME/                      data (0750 NAME:NAME), backups/ inside
#
# DEPLOY_ROOT prefixes every path and skips root/runuser; it exists only for
# the stubbed test harness (deploy/deploy_test.go in the GoStack repository).
# Leave it unset in production.
set -euo pipefail

ROOT="${DEPLOY_ROOT:-}"
SCRIPT="$(basename "$0")"

log() { printf '[%s] %s\n' "$SCRIPT" "$*" >&2; }
die() {
  log "ERROR: $*"
  exit 1
}

require_root() {
  if [ -z "$ROOT" ] && [ "$(id -u)" -ne 0 ]; then
    die "must run as root"
  fi
}

check_name() {
  [[ "$1" =~ ^[a-z][a-z0-9-]{0,30}$ ]] || die "invalid --name '$1' (want [a-z][a-z0-9-]{0,30})"
}

check_version() {
  [[ "$1" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$ ]] || die "invalid --version '$1'"
}

# set_paths NAME: defines the layout variables.
set_paths() {
  APP_DIR="$ROOT/opt/$1"
  ETC_DIR="$ROOT/etc/$1"
  DATA_DIR="$ROOT/var/lib/$1"
  UNIT_DIR="$ROOT/etc/systemd/system"
  CONFIG="$ETC_DIR/gostack.yaml"
  ENV_FILE="$ETC_DIR/gostack.env"
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# expected_sha BINARY SHA SUMS_FILE: prints the expected digest from --sha256
# or from a sha256sums file line naming the binary's basename.
expected_sha() {
  local bin="$1" sha="$2" sums="$3" base
  if [ -n "$sha" ]; then
    printf '%s\n' "$sha"
    return
  fi
  [ -n "$sums" ] || die "--sha256 or --sums is required (refusing unverified binaries)"
  base="$(basename "$bin")"
  awk -v f="$base" '$2 == f || $2 == "*"f {print $1; found=1} END {exit !found}' "$sums" ||
    die "$base not listed in $sums"
}

verify_sha() {
  local file="$1" want="$2" got
  [[ "$want" =~ ^[0-9a-f]{64}$ ]] || die "expected sha256 must be 64 lowercase hex chars"
  got="$(sha256_of "$file")"
  [ "$got" = "$want" ] || die "sha256 mismatch for $file: got $got, want $want"
}

# as_app CMD...: run CMD as the service user with the service environment.
as_app() {
  if [ -n "$ROOT" ]; then
    (set -a && . "$ENV_FILE" && set +a && exec "$@")
  else
    command -v runuser >/dev/null 2>&1 || die "runuser (util-linux) is required"
    runuser -u "$NAME" -- /bin/sh -c 'set -a; . "$0"; set +a; exec "$@"' "$ENV_FILE" "$@"
  fi
}

# stage_release VERSION BINARY SHA: copy the verified binary to
# releases/VERSION atomically (staging dir + rename). Re-running with the same
# binary is a no-op; a different binary under an existing version is refused.
stage_release() {
  local version="$1" bin="$2" sha="$3" rel tmp
  rel="$APP_DIR/releases/$version"
  if [ -e "$rel" ]; then
    [ "$(sha256_of "$rel/$NAME")" = "$sha" ] || die "release $version exists with a different binary; pick a new version"
    log "release $version already staged"
    return
  fi
  mkdir -p "$APP_DIR/releases"
  tmp="$(mktemp -d "$APP_DIR/releases/.stage.XXXXXX")"
  cp "$bin" "$tmp/$NAME"
  chmod 0755 "$tmp/$NAME" "$tmp"
  verify_sha "$tmp/$NAME" "$sha"
  printf '%s  %s\n' "$sha" "$NAME" >"$tmp/SHA256SUMS"
  mv "$tmp" "$rel"
  log "staged release $version"
}

# link_atomic TARGET LINK: point LINK at TARGET via rename (never a moment
# without LINK). GNU mv uses -T; BSD/macOS mv (stub tests) uses -h.
link_atomic() {
  local target="$1" link="$2" tmp="$2.tmp.$$"
  ln -sfn "$target" "$tmp"
  mv -T "$tmp" "$link" 2>/dev/null || mv -h "$tmp" "$link"
}

# release_of LINK: prints the version a current/previous link points at.
release_of() {
  local t
  t="$(readlink "$1" 2>/dev/null)" || return 1
  printf '%s\n' "${t#releases/}"
}

# preflight BIN: doctor + migration compatibility of BIN against the live DB.
# Pending migrations are fine (applied on start); drift or versions unknown to
# BIN are not.
preflight() {
  local bin="$1" out rc=0
  log "preflight: migrate status"
  out="$(as_app "$bin" --config "$CONFIG" --json migrate status)" || rc=$?
  if [ "$rc" -ne 0 ] && [ "$rc" -ne 3 ]; then
    die "migrate status failed (exit $rc)"
  fi
  if printf '%s\n' "$out" | grep -q '^  "drift": true'; then
    printf '%s\n' "$out" | grep -A3 '"unknown_versions"' >&2 || true
    die "database schema is newer than or drifted from $bin; refusing"
  fi
  PENDING_MIGRATIONS="$(printf '%s\n' "$out" | tr -d ' \n' | sed -n 's/.*"pending_versions":\[\([^]]*\)\].*/\1/p')"
  log "preflight: doctor"
  as_app "$bin" --config "$CONFIG" doctor >&2 || die "doctor failed for $bin"
}

# restart_service restarts the unit and never aborts the script: the caller's
# health_check decides. reset-failed first clears systemd's start-rate limit —
# a crash-looping release trips "Start request repeated too quickly", after
# which a plain restart of the previous (good) release is refused (found on a
# real systemd host during a failed-upgrade drill).
restart_service() {
  systemctl reset-failed "$NAME" >/dev/null 2>&1 || true
  if ! systemctl restart "$NAME"; then
    log "systemctl restart $NAME failed; health check decides"
  fi
}

# health_check URL TIMEOUT_SECONDS
health_check() {
  local url="$1" timeout="$2" deadline
  deadline=$(($(date +%s) + timeout))
  while :; do
    if systemctl is-active --quiet "$NAME" && curl -fsS --max-time 3 "$url" >/dev/null 2>&1; then
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      return 1
    fi
    sleep 1
  done
}
