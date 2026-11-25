#!/usr/bin/env bash
# GoStack light benchmark on a shared Linux host, emulating 1C1G with cgroups.
# Server: systemd transient unit, CPUQuota=100%, MemoryMax=1G, GOMAXPROCS=1.
# Load generator: CPUQuota=${CLIENT_CPU_QUOTA:-100%} scope (at 70% it was throttled at 1000 RPS
# and its schedule lag dominated p99). Short runs at PRD target rates only.
# Run as root from the directory holding example, gsseed, gsload, fsyncprobe and
# bench.yaml (cross-compiled linux binaries); results land in ./out.
set -euo pipefail
B=${BENCH_DIR:-/var/lib/gsbench}
OUT=$B/out
cd "$B"
ulimit -n 65536
mkdir -p "$OUT"
log() { echo "$(date +%H:%M:%S) $*" | tee -a "$OUT/progress.log"; }
hostinfo() { { echo "--- $1"; uptime; free -m | sed -n 2p; docker ps --format '{{.Names}} {{.Status}}'; } >> "$OUT/host_health.log"; }

hostinfo before
log "env"
{ uname -srm; . /etc/os-release; echo "$PRETTY_NAME"; nproc; grep -m1 'model name' /proc/cpuinfo; free -m | sed -n 2p; findmnt -no SOURCE,FSTYPE,OPTIONS -T "$B"; } > "$OUT/env.txt"

log "fsync probe"
./fsyncprobe -dir "$B" -n 2000 > "$OUT/fsync.json"

log "seed (cgroup 1 CPU / 1G)"
rm -rf data gsload-state.json
systemd-run --quiet --wait --pipe --collect --unit gsbench-seed -p CPUQuota=100% -p MemoryMax=1G \
  --working-directory="$B" "$B/gsseed" -config bench.yaml -players 100000 > "$OUT/seed.json"
du -b data/* > "$OUT/data_after_seed.txt"

start_server() {
  systemd-run --quiet --unit gsbench-srv -p CPUQuota=100% -p MemoryMax=1G -p LimitNOFILE=65536 \
    -E GOMAXPROCS=1 -E GOMEMLIMIT=900MiB --working-directory="$B" "$B/example" --config bench.yaml serve
  for _ in $(seq 100); do curl -fs http://127.0.0.1:18080/readyz >/dev/null 2>&1 && break; sleep 0.2; done
  PID=$(systemctl show -p MainPID --value gsbench-srv)
  echo "{\"pid\": $PID, \"rss_kib_idle_after_start\": $(ps -o rss= -p "$PID" | xargs)}" > "$OUT/server_idle_$1.json"
}
stop_server() {
  systemctl stop gsbench-srv 2>/dev/null || true
  systemctl reset-failed gsbench-srv 2>/dev/null || true
}
trap stop_server EXIT
L() { systemd-run --quiet --scope -p CPUQuota=${CLIENT_CPU_QUOTA:-100%} "$B/gsload" -url http://127.0.0.1:18080 -players 100000 -pid "$PID" -state "$B/gsload-state.json" "$@"; }

start_server target
for s in memory config_player pointread mixed9010 fullwrite board save8k economy gamemix; do
  log "scenario $s"
  L -scenario "$s" -warmup 15s -duration 60s -out "$OUT/target_$s.json" || log "scenario $s failed"
  hostinfo "after $s"
done
stop_server

start_server conns
log "ws idle 3000 x 120s"
L -scenario ws -conns 3000 -duration 120s -out "$OUT/conns_ws_idle.json" || log "ws idle failed"
log "ws active 3000, msg every 30s, 120s"
L -scenario ws -conns 3000 -duration 120s -ws-msg-interval 30s -out "$OUT/conns_ws_active.json" || log "ws active failed"
log "http idle 3000 x 60s"
L -scenario idlehttp -conns 3000 -duration 60s -out "$OUT/conns_http_idle.json" || log "http idle failed"
hostinfo "after conns"
stop_server

log "verify economy + reconcile"
systemd-run --quiet --wait --pipe --collect --unit gsbench-verify -p CPUQuota=100% -p MemoryMax=1G \
  --working-directory="$B" "$B/gsseed" -config bench.yaml -verify > "$OUT/verify_economy.json" || log "verify failed"
"$B/example" --config bench.yaml --json game reconcile bench > "$OUT/verify_reconcile.json" 2>&1 || log "reconcile reported problems"
du -b data/* > "$OUT/data_after_run.txt"
hostinfo after
log "done"
