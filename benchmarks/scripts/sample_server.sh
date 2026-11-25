#!/usr/bin/env bash
# Samples a server process once per second on the TARGET host (use it when the
# load generator runs on a separate machine and cannot use gsload -pid).
# Output: CSV unix_time,rss_kib,cpu_time,vm_hwm_kib(Linux only)
#
#   benchmarks/scripts/sample_server.sh "$(systemctl show -p MainPID --value gostack-bench)" > server_samples.csv
set -euo pipefail
pid=$1
echo "unix_time,rss_kib,cpu_time,vm_hwm_kib"
while kill -0 "$pid" 2>/dev/null; do
  read -r rss cpu < <(ps -o rss=,time= -p "$pid")
  hwm=""
  [[ -r /proc/$pid/status ]] && hwm=$(awk '/VmHWM/ {print $2}' "/proc/$pid/status")
  echo "$(date +%s),$rss,$cpu,$hwm"
  sleep 1
done
