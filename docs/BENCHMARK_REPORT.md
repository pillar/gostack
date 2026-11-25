# 基准报告 2026-11-25（local, not acceptance）

> **本报告全部数据来自开发机，不是 PRD §13/§23 验收结果。** 服务端与压测客户端在同一台机器；服务端用 `GOMAXPROCS=1`、`GOMEMLIMIT=900MiB` 限制，但这**不等于** 1 vCPU / 1 GiB Linux VPS：CPU 不同（Apple M5 Pro 单核远快于常见 VPS vCPU）、fsync 语义不同（见 §2）、没有 cgroup 内存上限、page cache 不受 1 GiB 限制、同机其他负载在运行。只能作为趋势与回归参考。目标环境验收方法见 `docs/PERFORMANCE_BUDGET.md` 与 `docs/GAME_PERFORMANCE_BUDGET.md`。

原始数据：`benchmarks/results/2026-11-25/`（每个场景一个 JSON；`driver_bench.txt` 为 `go test` 原始输出）。表格由 `benchmarks/cmd/gsreport` 从这些 JSON 生成。

## 1. 环境与方法

| 项 | 值 |
|---|---|
| CPU / 内存 | Apple M5 Pro，15 逻辑核，64 GiB（`env.json`） |
| OS / 文件系统 | macOS（Darwin 27.0.0 arm64），内置 SSD 上的 APFS 卷 |
| Go / SQLite | go1.27.4；modernc.org/sqlite v1.63.0（SQLite 3.54.1） |
| GoStack | commit `8036530`，`cmd/example`，`CGO_ENABLED=0 -trimpath` |
| 服务端 | `GOMAXPROCS=1 GOMEMLIMIT=900MiB`；配置 `benchmarks/config/bench.yaml`：WAL、`synchronous=FULL`、读连接 2、`http.max_concurrent=512`、日志 info/JSON 写文件、realtime 模块开启；`game.auth_rate_per_minute` 调高（所有请求来自同一 IP，否则埋点按 IP 限流会生效） |
| 压测端 | `gsload`，同机，GOMAXPROCS=15，开环恒定到达率；HTTP/1.1 keep-alive，无 TLS、无代理 |
| 数据集 | `gsseed` 固定种子 20261125：10 万玩家/会话，100 万榜单行（10 个周赛季），每玩家 8 KiB + 1 KiB 存档；种子库 1.62 GB，用时 57 s（`seed.json`） |
| 时长 | 目标速率：预热 30 s + 稳态 90 s；饱和搜索：每步预热 5 s + 稳态 15 s，×1.5 递增；长连接保持 120 s（HTTP 90 s）。PRD 要求的 2 min 预热 + 10 min 稳态、24 h、30 min 长连接**未执行** |
| 同机负载 | 运行期间 1 分钟 load average 3.9–32.5（均值 18.5），另一进程长期占用约 9–11 核（`host_load.txt`）。尾延迟（P99 约 4 ms）很可能部分来自这一干扰 |

延迟口径：修正延迟 = 完成时刻 − **计划**发送时刻（含客户端调度误差，已用"粗睡眠 + 自旋"压到 P99 ≈1 ms 以内，见 JSON 的 `client_schedule_lag`）；服务延迟 = 完成时刻 − 实际发送时刻。预算按修正延迟判定。错误率不含预期业务拒绝（本次为 0 次）。

## 2. fsync 探针（`fsync.json`）

| 屏障 | P50 | P99 | max |
|---|---|---|---|
| `fsync(2)`（SQLite FULL 在 macOS 上默认使用） | 0.023 ms | 0.040 ms | 0.333 ms |
| `F_FULLFSYNC`（Go `File.Sync`，真正刷新磁盘缓存） | 3.02 ms | 5.70 ms | 14.0 ms |

macOS 的 `fsync(2)` 不刷新磁盘写缓存，所以**本机所有 FULL 写场景的延迟和饱和点都严重偏乐观**。Linux 上 `fsync` 会刷新设备缓存，云盘常见 0.5–5 ms；目标机上 FULL 写的结论必须重新测量。驱动基准中开启 `fullfsync=1` 时单行写事务约 3.4–4.5 ms（§5），可作为"每事务一次真实刷盘"的量级参考：串行单写连接下上限约 220–300 事务/s，仍高于 PRD 的 100 RPS FULL 写目标，但余量远小于本机饱和数字所示。

## 3. 与 PRD 目标对比（local, not acceptance）

### 3.1 目标速率

| 场景 | PRD 目标 | 施加/完成 RPS | 预算分位实测 | P50 | P99 | P99.9 | max | 服务延迟 P99 | 错误率 | 服务端 RSS 峰值 MiB | 服务端 CPU | 本机判定 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| memory | §13 内存 API 1000 RPS P99≤30ms | 1000 / 1000.0 | 4.08 ms | 0.19 | 4.08 | 6.78 | 14.4 | 3.87 | 0.0000% | 27 | 9% | 通过 |
| config_player | §23 配置与玩家读取 300 RPS P95≤30ms | 300 / 300.0 | 2.48 ms | 0.36 | 4.16 | 6.78 | 10.0 | 3.77 | 0.0000% | 37 | 7% | 通过 |
| pointread | §13 索引点查 500 RPS P99≤50ms | 500 / 500.0 | 4.32 ms | 0.34 | 4.32 | 7.55 | 12.8 | 4.00 | 0.0000% | 49 | 11% | 通过 |
| mixed9010 | §13 90/10 混合 300 RPS P99≤100ms | 300 / 300.0 | 1.12 ms | 0.28 | 1.12 | 2.83 | 6.6 | 1.09 | 0.0000% | 50 | 9% | 通过 |
| fullwrite | §13 FULL 同步写 100 RPS P99≤150ms | 100 / 100.0 | 4.51 ms | 0.83 | 4.51 | 8.32 | 13.7 | 4.32 | 0.0000% | 50 | 8% | 通过（fsync 偏乐观） |
| board | §23 Top 100 榜单 200 RPS P95≤20ms | 200 / 200.0 | 0.66 ms | 0.36 | 0.95 | 2.94 | 11.0 | 0.90 | 0.0000% | 50 | 7% | 通过 |
| save8k | §23 8KiB 存档写入 50 RPS P95≤50ms | 50 / 50.0 | 2.00 ms | 1.03 | 4.06 | 9.21 | 13.5 | 3.62 | 0.0000% | 50 | 5% | 通过（fsync 偏乐观） |
| economy | §23 经济事务 FULL 50 RPS P99≤150ms | 50 / 50.0 | 7.29 ms | 0.97 | 7.29 | 11.33 | 13.7 | 6.43 | 0.0000% | 50 | 4% | 通过（fsync 偏乐观） |
| gamemix | §23 混合负载 200 RPS P99≤150ms | 200 / 200.0 | 5.60 ms | 0.45 | 5.60 | 10.37 | 36.1 | 4.89 | 0.0000% | 62 | 8% | 通过 |

"服务端 CPU" 为稳态内服务进程 CPU 秒 / 墙钟秒（100% = 一个核）。所有场景在同一服务进程中依次运行，RSS 是累计的。

### 3.2 内存与长连接

| PRD 项 | 目标 | 本机观测 |
|---|---|---|
| 空载应用 RSS | ≤64 MiB | 启动就绪后 5 s：20.0–20.3 MiB（3 次启动，`server_idle_*.json`） |
| 稳态应用 RSS | ≤256 MiB | `gamemix` 200 RPS 期间峰值 62 MiB；饱和阶段（过载）累计峰值 214 MiB；过载诊断中 162 MiB |
| 全机内存 ≤768 MiB | — | **无法在本机测量**（64 GiB 主机，无 cgroup） |
| 3000 空闲长连接 | 保持 30 min | 3000 个已认证 WebSocket 全部建立并保持 **120 s**（非 30 min），0 断开；服务端 RSS 峰值 141 MiB，CPU 0.9% |
| 活跃长连接 | 3000 连接、每 30 s 一条消息 | 3000 连接 120 s，12,319 条 ping→pong，RTT P99 2.06 ms，0 错误；RSS 峰值 152 MiB |
| 空闲 HTTP keep-alive | — | 3000 连接空闲 90 s 后 3000 条仍可复用；RSS 峰值 176 MiB（服务端 `IdleTimeout` 为 120 s） |
| 正常负载错误率 | <0.1% | 目标速率下全部 0 |
| 过载 | 有界内存、明确 429/503、卸载后恢复 | 见 §3.3；过载时出现 503 `overloaded`，但也出现大量连接被重置（不是明确的 503）；卸载后 200 RPS 立即恢复（`diag_recovery_gamemix.json`：0 错误，P99 2.0 ms） |

经济正确性（`verify_economy.json`，全部运行结束后）：`reward_claims` = 账本奖励行 = 账本 coin 合计 = 钱包 coin 合计 = 287,111，重复领取槽位 0；`economy` 目标运行中 120 次 Idempotency-Key 重放全部返回同一 `claim_id` 且带 `Idempotent-Replayed: true`，0 次不一致。`game reconcile`（逐玩家对账）在该数据量下 8.5 min 未完成，已中止，见 §4。

### 3.3 饱和搜索

| 场景 | 最高通过速率 RPS | 首个失败步 RPS | 失败步实测 | 失败原因 | 失败步错误类型 | 失败步服务端 RSS 峰值 MiB / CPU |
|---|---|---|---|---|---|---|
| memory | ≥17086（到达 20000 上限前未失败） | - | - | - | - | - |
| config_player | 11533 | 17300 | P95 1220.6 ms | p95 > 30ms; 错误率 24.0% | transport×62356 | 197 / 92% |
| pointread | 8543 | 12814 | P99 3178.5 ms | p99 > 50ms; 错误率 21.2% | transport×40743 | 211 / 91% |
| mixed9010 | 5126 | 7689 | P99 1441.8 ms | p99 > 100ms; 错误率 0.55% | transport×631 | 212 / 87% |
| fullwrite | 2563 | 3844 | P99 4784.1 ms | p99 > 150ms; 错误率 34.5% | transport×18300, 503 overloaded×1609 | 212 / 77% |
| board | 3417 | 5126 | P95 6.1 ms | 错误率 0.13% | transport×102 | 212 / 83% |
| save8k | 1922 | 2883 | P95 1966.1 ms | p95 > 50ms; 错误率 29.1% | transport×12600 | 212 / 82% |
| economy | 1922 | 2883 | P99 3129.3 ms | p99 > 150ms; 错误率 13.2% | 503 overloaded×2382, transport×3327 | 213 / 78% |
| gamemix | 3417 | 5126 | P99 602.1 ms | p99 > 150ms; 错误率 1.27% | transport×165, 503 overloaded×808 | 214 / 85% |

步长 ×1.5，所以真实饱和点在"最高通过"与"首个失败"之间。写场景的饱和点受 macOS `fsync` 偏乐观影响最大，不能外推到 Linux。

## 4. 观察到的瓶颈与问题

1. **单核 CPU 是读场景的上限**：失败步服务端 CPU 达 77–92%（GOMAXPROCS=1）。memory 场景在 17k RPS 仍通过，说明 HTTP 栈本身不是首要瓶颈；带会话查找的读（`config_player`、`pointread`）在 8.5k–11.5k RPS 饱和。每请求 info 级访问日志（JSON 写文件）计入了这部分 CPU。
2. **过载时连接被重置，而不是全部返回 503**：失败步的 transport 错误样本为 `connect: connection reset by peer`（`diag_overload_gamemix.json` 的 `error_samples`）。开环压测在过载时不断新建连接，macOS `kern.ipc.somaxconn=128` 的 accept 队列溢出；同时 `http.max_concurrent=512` 的并发闸门返回了明确的 503。PRD 要求"明确 429/503"：应用层闸门满足，但 TCP 层在建连洪峰下仍会重置。Linux 上 `somaxconn` 默认 4096，需在目标机复测；客户端连接池复用（SDK）可减轻该现象。
3. **写场景的饱和点偏乐观**：见 §2；目标机必须重测 `fullwrite`/`save8k`/`economy`。ADR-0002（串行短事务、未实现组提交）的触发条件"FULL 写达不到 100 RPS / P99 ≤150 ms"在本机未触发，但本机数据不能作为不需要组提交的证据。
4. **磁盘增长**：运行结束时 `app.db` 从 1.62 GB 增至 6.81 GB。其中 `idempotency_records` 637,163 行、响应体合计 1.84 GB（存档 PUT 的幂等响应包含完整 8 KiB payload，TTL 72 h），`save_revisions` 350,052 行、1.76 GB（`save_history` 10）。按 PRD 的 50 RPS 8 KiB 存档写持续 72 h 估算，幂等记录约 107 GB——在 1C1G 机器的本地盘上不可接受。需要在目标机上测量并评估（例如幂等响应不保存 payload、缩短存档写的 TTL）；本报告不修改实现。
5. **`game reconcile` 在 10 万玩家规模下不可用**：`EXPLAIN QUERY PLAN` 显示 `wallet_balances` 与物化 CTE 的 FULL OUTER JOIN 为嵌套全表扫描（约 10 万 × 10 万次比较），运行 8.5 min（96% CPU）未完成后中止（`verify_reconcile_note.txt`）。全局不变式已用 `gsseed -verify` 核对，但逐玩家余额对账在本次未得到验证。
6. **榜单**：Top-100（当前赛季 10 万行，表 100 万行）在 200 RPS 时 P95 0.66 ms，3417 RPS 仍通过；"本人名次"计数（PRD 评审 D5）不在本场景中，未测。

## 5. 驱动对比（ADR-0001）

`go test -tags sqlite_fts5 ./driver -bench . -benchmem -count 5 -cpu 1,4`，汇总自 `driver_bench.txt`（min / median / max，ns/op；spread = (max−min)/median）：

| benchmark | runs | ns/op min | ns/op median | ns/op max | spread | B/op | allocs/op |
|---|---|---|---|---|---|---|---|
| WriteFULL/modernc | 5 | 98732 | 115094 | 135646 | 32.1% | 807 | 22 |
| WriteFULL/modernc-4 | 5 | 79587 | 104421 | 118178 | 37.0% | 807 | 22 |
| WriteFULL/mattn | 5 | 78830 | 99246 | 108329 | 29.7% | 943 | 28 |
| WriteFULL/mattn-4 | 5 | 76855 | 88835 | 131868 | 61.9% | 943 | 28 |
| WriteFULL/modernc/fullfsync | 5 | 3510365 | 4241329 | 4458563 | 22.4% | 776 | 20 |
| WriteFULL/modernc/fullfsync-4 | 5 | 3323316 | 3360022 | 3880323 | 16.6% | 787 | 20 |
| WriteFULL/mattn/fullfsync | 5 | 3330155 | 3450577 | 3504906 | 5.1% | 927 | 26 |
| WriteFULL/mattn/fullfsync-4 | 5 | 3251744 | 4471621 | 5061019 | 40.5% | 925 | 26 |
| BatchInsert1000/modernc | 5 | 5498897 | 5724057 | 6011413 | 9.0% | 328706 | 13010 |
| BatchInsert1000/modernc-4 | 5 | 5653423 | 10094008 | 12664325 | 69.5% | 328737 | 13007 |
| BatchInsert1000/mattn | 5 | 5796020 | 6578676 | 9147042 | 50.9% | 288879 | 11017 |
| BatchInsert1000/mattn-4 | 5 | 4774608 | 5976162 | 8324288 | 59.4% | 288949 | 11018 |
| PointRead4/modernc | 5 | 5522 | 5914 | 6228 | 11.9% | 2784 | 25 |
| PointRead4/modernc-4 | 5 | 4426 | 4472 | 4506 | 1.8% | 2784 | 25 |
| PointRead4/mattn | 5 | 3550 | 3730 | 3852 | 8.1% | 2623 | 17 |
| PointRead4/mattn-4 | 5 | 3489 | 3674 | 5396 | 51.9% | 2624 | 17 |

- 写事务与批量插入：区间大面积重叠，spread 高（同机干扰），**无可区分差异**。
- 主键点查：mattn 快约 35–40%（约 2 µs/次），allocs 17 vs 25。
- FTS5：modernc 默认可用；mattn 需 `-tags sqlite_fts5`（`driver_features.txt`）。两者都捆绑 SQLite 3.54.1。
- 二进制（`driver_size.txt`，最小程序）：darwin/arm64 strip 后 modernc 6.45 MB、mattn 3.77 MB；modernc linux/amd64 6.65 MB、linux/arm64 6.49 MB（`CGO_ENABLED=0`）；mattn 在本机交叉编译 linux **失败**（无 Linux C 工具链）。
- 许可证核对见 ADR-0001。结论：ADR-0001 **保持暂定**，缺 Linux 1C1G 上的整机对比。

## 6. 复现

```sh
BENCH_WORK=/path/on/local/ssd benchmarks/scripts/run_local.sh        # 约 60 min；结果写入 benchmarks/results/<date>/
(cd benchmarks && go run ./cmd/gsreport results/<date>)               # 生成 §3 表格
```

单个场景：`gsload -scenario <name> [-rate N] [-sat] -pid <server pid> -state <file>`；`gsload -list` 列出场景与预算。

## 7. 局限（再次强调）

- 同机压测、非 Linux、非 1C1G、无 cgroup、page cache 不受限、同机有其他高负载进程。
- macOS `fsync` 不刷盘：FULL 写数据偏乐观。
- 时长远短于 PRD（10 min 稳态、24 h、30 min 长连接均未执行）；NORMAL/FULL、读连接数、冷缓存、磁盘压力矩阵未执行；断电级"零已确认事务丢失"未测试。
- 无 TLS/代理；客户端与服务端之间无网络 RTT。
- 逐玩家对账未完成（§4.5）。
