# 性能预算（PRD §13）与目标环境验收方法

状态：**目标环境未验证**。下表全部是 PRD §13 的候选目标，不是实测结论。本地（开发机）结果见 `docs/BENCHMARK_REPORT.md`，它们只作趋势参考，标记为 "local, not acceptance"，不得作为 A10/发布门禁证据（PRD 评审 V1）。游戏专项预算见 `docs/GAME_PERFORMANCE_BUDGET.md`。

## 1. 预算

| 场景 | 目标 | gsload 场景 | 判定 |
|---|---|---|---|
| 空载应用 RSS | ≤64 MiB | 服务启动 + 就绪后 5s 的 RSS | `ps -o rss` / `VmRSS` |
| 稳态应用 RSS | ≤256 MiB（指定负载） | `gamemix` 目标速率稳态内 RSS 峰值 | gsload `server.rss_max_kib` 或 `sample_server.sh` |
| 全机内存 | ≤768 MiB（含备份、代理、系统；注明 page cache 口径） | 稳态期间 `free -m` 的 used（不含 buff/cache）与 `MemAvailable` | 目标机手工记录 |
| 内存 API | 1000 RPS，P99 ≤30 ms | `memory`（GET /healthz） | 修正延迟 P99 |
| 索引点查 | 500 RPS，P99 ≤50 ms | `pointread`（会话查找 + 1 KiB 存档主键读） | 修正延迟 P99 |
| 90/10 混合 | 300 RPS，P99 ≤100 ms | `mixed9010` | 修正延迟 P99 |
| FULL 同步写 | 100 RPS，P99 ≤150 ms，零已确认事务丢失 | `fullwrite`（1 KiB 存档写 + 奖励领取，各 50%） | P99 + `tests/fault` 进程级崩溃测试 + 目标机断电/kill 测试 |
| 3000 空闲长连接 | 保持 30 min，无 OOM，资源在预算内 | `ws -conns 3000 -duration 30m` | `alive_at_end == 3000`、RSS |
| 活跃长连接 | 3000 连接、每连接 30 s 一条消息，记录吞吐与延迟 | `ws -conns 3000 -ws-msg-interval 30s` | 记录 `message_rtt` |
| 正常负载错误率 | <0.1%，不含预期业务拒绝 | 所有场景 | `error_rate` |
| 过载 | 有界内存、明确 429/503，卸载后恢复 | `-sat` 阶梯直到失败，随后再跑一次目标速率 | 失败步只出现 503/429；RSS 有界；恢复步通过 |

约定：

- **延迟口径**：gsload 是开环（恒定到达率）压测，延迟从**计划发送时刻**计起（修正协调遗漏，coordinated omission），同时报告从实际发送计起的服务延迟和客户端调度滞后（`client_schedule_lag`）。预算按修正延迟判定。服务端时间不含公网 RTT；跨主机压测时修正延迟包含内网 RTT，需另行记录 `ping` RTT。
- **错误率**：`errors / (scheduled - expected_rejections)`。预期业务拒绝（`already_claimed`、`insufficient_funds`）不计入；429/503、超时、传输错误、客户端丢弃（在途上限）与未完成请求都计入。
- **吞吐判定**：实际完成速率 <95% 的施加速率即判失败（饱和）。
- 每个结果 JSON 都带 `label`；只有在 §3 的目标环境、独立压测机上运行时才可用 `-label` 改写为验收标签。

## 2. 工具

全部位于 `benchmarks/`（独立 Go module，`replace github.com/pillar/gostack => ../`，压测专用依赖不进入主 `go.mod`）：

| 工具 | 作用 |
|---|---|
| `cmd/gsseed` | 通过 `app.OpenDB`（实例锁、迁移、PRAGMA）与 GameKit API 写入 §23 数据集：10 万玩家、100 万榜单行（10 个周赛季）、每玩家 8 KiB + 1 KiB 存档与会话；分批事务、固定种子可复现。`-verify` 只读核对奖励领取、账本与余额 |
| `cmd/gsload` | 开环负载发生器，场景见 `gsload -list`；预热/稳态分段、log-linear 直方图（≤0.8% 误差）、按场景预算判定、`-sat` 饱和搜索、`ws`/`idlehttp` 长连接、JSON 输出；`-pid` 时用 `ps` 采样服务端 RSS/CPU |
| `cmd/fsyncprobe` | 测数据盘 4 KiB 写 + `fsync(2)` 与 `File.Sync`（macOS 为 F_FULLFSYNC）的延迟 |
| `driver/` | ADR-0001 驱动对比 Go 基准（modernc vs mattn/go-sqlite3 CGO） |
| `cmd/benchsum` | 汇总 `go test -bench -count N` 的 min/median/max |
| `cmd/gsreport` | 从结果目录生成报告表格（目标速率、饱和、长连接） |
| `scripts/run_local.sh` | 本机全流程（单机，结果自动标为 local） |
| `scripts/sample_server.sh` | 在目标机上每秒采样服务进程 RSS/CPU/VmHWM（压测机独立时用） |

## 3. 在 1C1G 目标环境上执行（验收方法）

PRD 要求：1 vCPU、1 GiB RAM、Linux、本地 SSD、固定 Go/SQLite 版本；**压测客户端在独立机器上**；预热 2 min、稳态 10 min，另做 24 h 稳定性测试；记录 CPU 型号、fsync 延迟、数据规模、TLS/代理、并发与负载模型。

### 3.1 主机

- 目标机：真实 1 vCPU / 1 GiB 的 Linux VPS，数据目录位于本地 SSD（不是网络盘）。如只能用更大的 Linux 主机，用 cgroup 限制（见 3.3）并在报告中注明 "cgroup-emulated"，这不能替代真实 1C1G 结论。
- 压测机：同地域、内网互通，≥4 vCPU，单独记录两机间 `ping` RTT。服务只监听内网地址，安全组仅放行压测机。
- 两机记录：`uname -a`、`lscpu`、`free -m`、`df -hT <数据目录>`、`go version`、GoStack commit、内核 `vm.dirty_*` 参数。

### 3.2 构建与准备（在开发机交叉编译）

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o out/example ./cmd/example
cd benchmarks
for c in gsseed gsload fsyncprobe benchsum; do CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o ../out/$c ./cmd/$c; done
```

目标机：

```sh
sudo mkdir -p /var/lib/gostack-bench && sudo chown $USER /var/lib/gostack-bench
cp benchmarks/config/bench.yaml /var/lib/gostack-bench/bench.yaml
# 把 http.listen 改成内网地址，例如 10.0.0.5:18080；internal_listen 保持 127.0.0.1
./fsyncprobe -dir /var/lib/gostack-bench -n 2000 > fsync.json
./gsseed -config /var/lib/gostack-bench/bench.yaml > seed.json     # 约 1 GiB 数据；会话 token 为固定可推导值，只能用于压测库
```

### 3.3 启动服务（systemd，资源限制与生产单元一致）

真实 1C1G 机器上：

```sh
sudo systemd-run --unit gostack-bench --working-directory=/var/lib/gostack-bench \
  -p LimitNOFILE=65536 -p MemoryMax=768M \
  --setenv=GOMEMLIMIT=600MiB \
  /path/to/example --config /var/lib/gostack-bench/bench.yaml serve
```

`MemoryMax=768M` 与模板 `templates/app/deploy/systemd/app.service.tmpl` 一致。仅在更大主机上模拟时追加 `-p AllowedCPUs=0 -p CPUQuota=100% -p MemorySwapMax=0 --setenv=GOMAXPROCS=1`，并把整机内存口径改为该 cgroup 的 `memory.current`/`memory.peak`。

采样（目标机另开终端）：

```sh
benchmarks/scripts/sample_server.sh "$(systemctl show -p MainPID --value gostack-bench)" > server_samples.csv
```

### 3.4 压测（在压测机）

```sh
U=http://10.0.0.5:18080
for s in memory pointread mixed9010 fullwrite; do
  ./gsload -url $U -scenario $s -warmup 2m -duration 10m -pid 0 -label "1C1G target, separate load host" -out target_$s.json
done
./gsload -url $U -scenario ws -conns 3000 -duration 30m -out ws_idle_30m.json
./gsload -url $U -scenario ws -conns 3000 -duration 10m -ws-msg-interval 30s -out ws_active.json
for s in memory pointread mixed9010 fullwrite; do
  ./gsload -url $U -scenario $s -sat -sat-warmup 30s -sat-duration 2m -out sat_$s.json
done
./gsload -url $U -scenario gamemix -warmup 2m -duration 24h -out stability_24h.json   # 24 h 稳定性
```

压测机需 `ulimit -n 65536`。每次压测后在目标机执行 `gsseed -config bench.yaml -verify` 与 `example --config bench.yaml --json game reconcile bench`（停服后，需实例锁）。

### 3.5 未被本工具覆盖、仍需单独完成的项

- "零已确认事务丢失"：进程级 kill 由 `tests/fault` 覆盖；断电级（VM 强制断电）需在目标机手工执行并对账，未完成。
- NORMAL/FULL、批次大小、读连接数、冷/热缓存、慢查询、磁盘压力矩阵：用 `--set database.read_connections=N`、`GOSTACK_DATABASE_SYNCHRONOUS=NORMAL`、清 page cache（`echo 3 > /proc/sys/vm/drop_caches`）分别重跑；尚未执行。
- TLS/Caddy/Cloudflare 链路：本工具直连 HTTP；经代理的数据需单独记录。

## 4. 当前状态

| 项 | 状态 |
|---|---|
| 工具链（gsseed/gsload/fsyncprobe/driver bench） | 已实现，已在本机运行 |
| 本机趋势数据 | 见 `docs/BENCHMARK_REPORT.md`（local, not acceptance） |
| 1C1G Linux 目标机、独立压测机 | **未运行，全部预算未验证** |
| 24 h 稳定性、30 min 空闲长连接 | **未运行**（本机只做了 2 min 保持） |
