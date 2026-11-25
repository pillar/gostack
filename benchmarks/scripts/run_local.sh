#!/usr/bin/env bash
# Runs the full local benchmark suite: environment capture, fsync probe, driver
# comparison (ADR-0001), dataset seeding, every PRD §13/§23 scenario at its
# target rate, long connections, then a saturation search per scenario.
#
# Server and load generator share this host. The server is pinned with
# GOMAXPROCS=1 and GOMEMLIMIT=900MiB; that does NOT emulate a 1 vCPU / 1 GiB
# Linux VPS (different CPU, fsync semantics, no cgroup limits). Results are
# labelled "local, not acceptance".
#
#   benchmarks/scripts/run_local.sh               # everything
#   PHASES="target conns" benchmarks/scripts/run_local.sh
#
# Environment: BENCH_WORK (data dir, default: a new temp dir on the system disk),
# OUT (results dir, default benchmarks/results/<date>), WARMUP, DURATION,
# SAT_WARMUP, SAT_DURATION, CONNS, CONN_HOLD, HTTP_HOLD, PLAYERS, SKIP_SEED=1 (reuse BENCH_WORK data).
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
BENCH="$ROOT/benchmarks"
DATE=$(date +%Y-%m-%d)
OUT=${OUT:-$BENCH/results/$DATE}
WORK=${BENCH_WORK:-$(mktemp -d -t gostack-bench)}
WARMUP=${WARMUP:-30s}
DURATION=${DURATION:-90s}
SAT_WARMUP=${SAT_WARMUP:-5s}
SAT_DURATION=${SAT_DURATION:-15s}
CONNS=${CONNS:-3000}
CONN_HOLD=${CONN_HOLD:-120s}
HTTP_HOLD=${HTTP_HOLD:-90s}
PLAYERS=${PLAYERS:-100000}
PHASES=${PHASES:-"env driver seed target conns sat verify"}
SCENARIOS=${SCENARIOS:-"memory config_player pointread mixed9010 fullwrite board save8k economy gamemix"}
mkdir -p "$OUT" "$WORK/bin"
echo "work dir: $WORK  results: $OUT"
has() { [[ " $PHASES " == *" $1 "* ]]; }

(cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -o "$WORK/bin/example" ./cmd/example)
(cd "$BENCH" && go build -trimpath -o "$WORK/bin/gsseed" ./cmd/gsseed \
  && go build -trimpath -o "$WORK/bin/gsload" ./cmd/gsload \
  && go build -trimpath -o "$WORK/bin/fsyncprobe" ./cmd/fsyncprobe \
  && go build -trimpath -o "$WORK/bin/benchsum" ./cmd/benchsum)
B="$WORK/bin"

if has env; then
  {
    echo "{"
    echo "  \"date\": \"$(date -u +%Y-%m-%dT%H:%M:%SZ)\","
    echo "  \"uname\": \"$(uname -srm)\","
    if [[ $(uname) == Darwin ]]; then
      echo "  \"cpu\": \"$(sysctl -n machdep.cpu.brand_string)\","
      echo "  \"logical_cpus\": $(sysctl -n hw.ncpu),"
      echo "  \"mem_bytes\": $(sysctl -n hw.memsize),"
    else
      echo "  \"cpu\": \"$(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2 | xargs)\","
      echo "  \"logical_cpus\": $(nproc),"
      echo "  \"mem_bytes\": $(( $(grep MemTotal /proc/meminfo | awk '{print $2}') * 1024 )),"
    fi
    echo "  \"go\": \"$(go env GOVERSION)\","
    echo "  \"gostack_commit\": \"$(git -C "$ROOT" rev-parse --short HEAD)\","
    echo "  \"data_dir_fs\": \"$(df -P "$WORK" | tail -1 | awk '{print $1}')\","
    echo "  \"server_env\": {\"GOMAXPROCS\": 1, \"GOMEMLIMIT\": \"900MiB\"},"
    echo "  \"warmup\": \"$WARMUP\", \"duration\": \"$DURATION\", \"sat_warmup\": \"$SAT_WARMUP\", \"sat_duration\": \"$SAT_DURATION\""
    echo "}"
  } > "$OUT/env.json"
  "$B/fsyncprobe" -dir "$WORK" -n 2000 | sed "s|\"dir\": \".*\"|\"dir\": \"<BENCH_WORK>\"|" > "$OUT/fsync.json"
fi

if has driver; then
  (cd "$BENCH" && go test -tags sqlite_fts5 ./driver -run TestFeatures -v -count=1 | grep FEATURES > "$OUT/driver_features.txt")
  (cd "$BENCH" && go test -tags sqlite_fts5 ./driver -run '^$' -bench . -benchmem -count 5 -cpu 1,4 | tee "$OUT/driver_bench.txt")
  "$B/benchsum" < "$OUT/driver_bench.txt" > "$OUT/driver_bench.md"
  "$B/benchsum" -json < "$OUT/driver_bench.txt" > "$OUT/driver_bench.json"
  {
    for v in "modernc:" "mattn:-tags drv_mattn"; do
      name=${v%%:*}; tags=${v#*:}
      (cd "$BENCH" && go build -trimpath $tags -o "$WORK/bin/drv_$name" ./cmd/drvsize)
      (cd "$BENCH" && go build -trimpath $tags -ldflags "-s -w" -o "$WORK/bin/drv_${name}_stripped" ./cmd/drvsize)
      echo "$name darwin/arm64 bytes=$(stat -f %z "$WORK/bin/drv_$name" 2>/dev/null || stat -c %s "$WORK/bin/drv_$name") stripped=$(stat -f %z "$WORK/bin/drv_${name}_stripped" 2>/dev/null || stat -c %s "$WORK/bin/drv_${name}_stripped") run=\"$("$WORK/bin/drv_$name")\""
    done
    for arch in amd64 arm64; do
      if (cd "$BENCH" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w" -o "$WORK/bin/drv_modernc_linux_$arch" ./cmd/drvsize); then
        echo "modernc linux/$arch CGO_ENABLED=0 stripped bytes=$(stat -f %z "$WORK/bin/drv_modernc_linux_$arch" 2>/dev/null || stat -c %s "$WORK/bin/drv_modernc_linux_$arch")"
      fi
      if (cd "$BENCH" && CGO_ENABLED=1 GOOS=linux GOARCH=$arch go build -trimpath -tags drv_mattn -o /dev/null ./cmd/drvsize) 2> "$WORK/cgo_cross_$arch.err"; then
        echo "mattn linux/$arch CGO cross-build: ok"
      else
        echo "mattn linux/$arch CGO cross-build: FAILED ($(head -c 200 "$WORK/cgo_cross_$arch.err" | tr '\n' ' '))"
      fi
    done
  } > "$OUT/driver_size.txt"
  cat "$OUT/driver_size.txt"
fi

if has seed && [[ "${SKIP_SEED:-0}" != 1 ]]; then
  rm -rf "$WORK/data" "$WORK/gsload-state.json"
  cp "$BENCH/config/bench.yaml" "$WORK/bench.yaml"
  "$B/gsseed" -config "$WORK/bench.yaml" -players "$PLAYERS" > "$OUT/seed.json"
fi

server_pid=""
start_server() {
  (cd "$WORK" && GOMAXPROCS=1 GOMEMLIMIT=900MiB exec "$B/example" --config bench.yaml serve >> "$WORK/server.log" 2>&1) &
  server_pid=$!
  for _ in $(seq 100); do curl -fs http://127.0.0.1:18080/readyz >/dev/null 2>&1 && break; sleep 0.1; done
  sleep 5
  echo "{\"pid\": $server_pid, \"rss_kib_idle_after_start\": $(ps -o rss= -p $server_pid | xargs)}" > "$OUT/server_idle_$1.json"
}
stop_server() {
  [[ -n "$server_pid" ]] && kill -TERM "$server_pid" && wait "$server_pid" || true
  server_pid=""
}
trap stop_server EXIT
L() { "$B/gsload" -url http://127.0.0.1:18080 -players "$PLAYERS" -pid "$server_pid" -state "$WORK/gsload-state.json" "$@"; }

if has target; then
  start_server target
  for s in $SCENARIOS; do
    L -scenario "$s" -warmup "$WARMUP" -duration "$DURATION" -out "$OUT/target_$s.json"
  done
  stop_server
fi

if has conns; then
  start_server conns
  L -scenario ws -conns "$CONNS" -duration "$CONN_HOLD" -out "$OUT/conns_ws_idle.json"
  L -scenario ws -conns "$CONNS" -duration "$CONN_HOLD" -ws-msg-interval 30s -out "$OUT/conns_ws_active.json"
  L -scenario idlehttp -conns "$CONNS" -duration "$HTTP_HOLD" -out "$OUT/conns_http_idle.json"
  stop_server
fi

if has sat; then
  start_server sat
  for s in $SCENARIOS; do
    L -scenario "$s" -sat -sat-warmup "$SAT_WARMUP" -sat-duration "$SAT_DURATION" -out "$OUT/sat_$s.json"
  done
  stop_server
fi

if has verify; then
  "$B/gsseed" -config "$WORK/bench.yaml" -verify > "$OUT/verify_economy.json"
  # Note: on the 100k-player dataset `game reconcile` did not finish in 8.5 min (see results/2026-11-25/verify_reconcile_note.txt).
  if [[ "${RECONCILE:-0}" == 1 ]]; then
    (cd "$WORK" && "$B/example" --config bench.yaml --json game reconcile bench) > "$OUT/verify_reconcile.json" || echo "reconcile failed or reported mismatches" >&2
  fi
  (cd "$WORK/data" && for f in *.db *.db-wal; do [[ -e $f ]] && echo "$(wc -c < "$f" | xargs) $f" || true; done) > "$OUT/data_files.txt"
fi
echo "done: $OUT"
