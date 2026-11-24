# Sourced by scripts/drill/*.sh: start a throwaway MinIO on a free localhost
# port with random root credentials (exported only to this shell, never
# written to disk except MinIO's own data dir), and stop it on exit.
#
#   minio_start DATA_DIR LOG_FILE   -> sets MINIO_ENDPOINT, MINIO_KEY, MINIO_SECRET, MINIO_PID
#   minio_stop
#
# MINIO_BIN overrides the binary (default: minio on PATH).

minio_free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

minio_start() {
  local data="$1" log="$2" bin="${MINIO_BIN:-minio}"
  command -v "$bin" >/dev/null 2>&1 || { echo "minio not found (set MINIO_BIN)" >&2; return 1; }
  local port console
  port=$(minio_free_port)
  console=$(minio_free_port)
  MINIO_KEY="drill$(openssl rand -hex 6)"
  MINIO_SECRET="$(openssl rand -hex 20)"
  mkdir -p "$data"
  MINIO_ROOT_USER="$MINIO_KEY" MINIO_ROOT_PASSWORD="$MINIO_SECRET" MINIO_BROWSER=off \
    "$bin" server "$data" --address "127.0.0.1:$port" --console-address "127.0.0.1:$console" >"$log" 2>&1 &
  MINIO_PID=$!
  MINIO_ENDPOINT="http://127.0.0.1:$port"
  local i
  for i in $(seq 1 100); do
    if ! kill -0 "$MINIO_PID" 2>/dev/null; then
      echo "minio exited during startup; first lines of $log:" >&2
      head -5 "$log" >&2
      MINIO_PID=""
      return 1
    fi
    if curl -fsS "$MINIO_ENDPOINT/minio/health/live" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  echo "minio not healthy after 10s; see $log" >&2
  return 1
}

minio_stop() {
  if [ -n "${MINIO_PID:-}" ]; then
    kill "$MINIO_PID" 2>/dev/null || true
    wait "$MINIO_PID" 2>/dev/null || true
    MINIO_PID=""
  fi
}
