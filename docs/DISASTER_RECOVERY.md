# 容灾与恢复（PRD §11）

## 现状与边界

- 已实现并测试（本机 Go 测试，真实文件）：`backup snapshot`（VACUUM INTO 在线一致性快照）、`backup verify`（integrity_check + schema 与二进制兼容 + GameKit 钱包/账本对账）、`backup restore`（先验证、临时文件 + 链接发布、拒绝覆盖）、`deploy/litestream/restore.sh`（Litestream 恢复到新路径 → verify；桩测试 + 下文真实 Litestream v0.5.18 演练）。
- **已执行（本地类比，非 A08 验收）**：真实 Litestream v0.5.18 + **本地 file 副本** 的 kill -9 → restore.sh → verify → 重启 → API 回读演练，见下文「本地演练结果」。
- **未执行**：Litestream → S3 兼容存储（MinIO 本地、Cloudflare R2）的复制与恢复；1C1G 目标机上的任何测量。RPO ≤30s、1 GiB 样本 RTO ≤10min 仍是**目标**，需在目标环境（R2 + 1C1G）实测后才能宣称；网络故障期间不保证。
- Litestream 为异步复制：断电/机器丢失时，最后一个已上传段之后的已提交事务会丢失。关键资金业务若要求跨机器零丢失，不能只依赖它（PRD §11）。
- `analytics.db` 为 P2 数据，默认不复制、不在恢复范围内。

## 备份命令

```bash
APP --config CFG backup snapshot --to /var/lib/APP/backups/manual-<ts>.db   # 在线；目标已存在则拒绝
APP --config CFG [--json] backup verify FILE                                 # 不修改 FILE（在临时副本上检查）
APP --config CFG [--json] backup restore --from FILE --to NEW_PATH           # NEW_PATH 及其 -wal/-shm 必须不存在
```
`backup verify` 退出码：0 通过；3 检查失败（损坏、截断、非 GoStack 库、schema 比本二进制新或漂移、账本不平）；1 运行错误。待处理迁移（备份来自旧版本）视为可接受并列出，服务启动时会自动迁移。

`--json` 输出字段：`ok`、`file`、`bytes`、`sha256`、`integrity`、`schema{known,applied,pending_versions,unknown_versions,drifted_versions,drift}`、`reconcile_mismatches`（无 GameKit 表时为 null）、`problems`。

## 恢复流程

1. **隔离旧实例，防止双写**：`systemctl stop APP`（若旧机器仍可达），并确保不会被自动拉起；若旧机器不可达，在入口（Cloudflare/负载均衡/DNS）切断其流量。旧实例上的 Litestream 必须停止，避免两个写者向同一副本路径写入。
2. **新机安装固定版本**：用与故障前相同（或更新）的发布物 `deploy/install.sh`（不要先启动服务写入新库；可先安装后 `systemctl stop APP`）。安装 `litestream`（固定版本）及其配置（R2 凭据独立于应用凭据、最小权限）。
3. **恢复到新路径**（默认禁止覆盖现有库）：
   ```bash
   sudo deploy/litestream/restore.sh --bin /opt/APP/current/APP --replica-config /etc/litestream.yml \
        --db /var/lib/APP/app.db --to /var/lib/APP/restore/app-<ts>.db --owner APP [--timestamp <RFC3339>]
   ```
   或从本地/异地快照：`APP backup restore --from snap.db --to /var/lib/APP/restore/app-<ts>.db`。
   脚本可重复执行：失败时只删除本次创建的不完整文件；验证失败（退出码 3）时保留文件供检查，**不得切换到该文件**。
4. **校验**（脚本已自动执行，可复查）：`backup verify` 的 integrity_check、schema 兼容、账本对账均通过；抽查关键数据（玩家数、最近订单/账本时间）。记录恢复点时间以评估实际 RPO。
5. **切换并启动**：把旧 `app.db`、`app.db-wal`、`app.db-shm`（如存在）**移到一旁（不删除）**，将恢复文件改名为 `app.db`；或在 `/etc/APP/gostack.env` 设置 `GOSTACK_DATABASE_PATH` 指向恢复文件。`systemctl start APP`，确认 `/readyz` 200、`APP game reconcile` 0 差异。
6. **恢复复制**：更新 Litestream 配置指向当前数据库路径并启动，确认新的快照/段已上传。
7. **切流**：恢复入口流量；观察 `docs/RUNBOOK.md` 相关指标 30 分钟。
8. **记录演练/事故**：开始/结束时间（RTO）、恢复点（RPO）、遇到的问题，更新本文件与 `docs/STATUS.md` 的 A08 状态（仅在实测后）。

## 演练建议（A08 门禁，需人工执行）

在独立机器上每季度一次：生成约 1 GiB 样本 → Litestream 复制到 R2 → 销毁源机 → 按上述步骤恢复，记录 RPO/RTO 原始数据。CI 只检查脚本语法与桩测试（PRD_v2_REVIEW：A08 需外部资源，列为人工门禁）。

## Litestream v0.5 配置要点（依据 v0.5.18 源码核对，2026-11-25）

模板 `templates/app/deploy/litestream/litestream.yml.tmpl` 已按 v0.5.18 修正：
- 每个库使用单个 `replica:` 映射。旧的 `replicas:` 列表在 v0.5 中标注为 Deprecated（`cmd/litestream/main.go` `DBConfig.Replicas`），多于一个副本直接报错（"multiple replicas on a single database are no longer supported"）。
- 副本下的 `retention: 168h` 在 v0.5 中**不存在**，YAML 非严格解析会静默忽略（旧模板因此实际只保留默认 24h）。保留期改为顶层 `snapshot: {interval: 24h, retention: 168h}`（默认均为 24h，`DefaultConfig()`）。
- 凭据：`LITESTREAM_ACCESS_KEY_ID`/`LITESTREAM_SECRET_ACCESS_KEY` 环境变量在 v0.5.18 仍有效（未设置 `AWS_*` 时复制为 `AWS_*`）。
- 端点自动识别：对 MinIO、R2 等端点 Litestream 默认签名 payload；MinIO 默认 path-style，R2 不强制 path-style（`main.go` provider defaults）。
- 运维命令：v0.5 没有 `snapshots`/`generations` 子命令（`litestream snapshots` → `unknown command`），改用 `litestream ltx -config CFG [-level all] DB`（列出 LTX 文件与时间）、`litestream status -config CFG`、`litestream restore -dry-run`。

**wal_autocheckpoint 与 Litestream**（GoStack 配置：`database.wal_autocheckpoint` 默认 1000；为 0 时校验要求 `backup.enabled: true` 且 `backup.driver: litestream`，见 `core/config/config.go`）。v0.5.18 的实际行为（源码）：
- Litestream 自己的连接用 `_pragma=wal_autocheckpoint(0)` 打开（`db.go` `init`），并持有一个长读事务“to prevent checkpointing”（`acquireReadLock`），只在自己做 checkpoint 时短暂释放；
- 它自行 checkpoint：WAL 超过 `min-checkpoint-page-count`（默认 1000 页）或每 `checkpoint-interval`（默认 1m）做 PASSIVE；超过 `truncate-page-n`（默认 121359 页 ≈ 500 MB@4 KiB）做 TRUNCATE；
- 其他进程截断 WAL 时它能检测（"wal truncated by another process"）并重新同步。
- 结论：应用保持默认 1000 与 Litestream 兼容（SQLite 的 checkpoint 越不过 Litestream 的读锁，且 PASSIVE 不会重置 WAL）。设为 0 把 checkpoint 完全交给 Litestream：Litestream 停止期间 WAL 无界增长，因此仅在对 litestream 服务与 `-wal` 文件大小有告警时使用。演练中两种设置都完成了恢复且无一致性问题（各仅 1 次，吞吐差异不构成结论）。
- 观测：持续高写入下 WAL 会长到数百 MB 才被截断（见下表 `WAL@kill`，最高 338 MB），1C1G 机器需为此预留磁盘；中等负载下约 10 MB。

## 本地演练结果（A08 的本地类比，**不是** R2/1C1G 验收）

**环境**：Apple M5 Pro（15 核）、64 GiB、macOS（Darwin 27.0.0, arm64）、本机 SSD；Go 1.27.4；Litestream v0.5.18（`go install`）；SQLite 3.54.1（modernc）。应用为 `gostack init drill --kit game` 生成的工程。**副本类型为本地 file 目录**：本机唯一的 MinIO（Homebrew `minio` RELEASE.2025-10-15T17-29-55Z）在本系统上启动即崩溃（`SIGSEGV: segmentation violation ... signal arrived during cgo execution`，位于 `github.com/shoenig/go-m1cpu.init`），因此 S3/MinIO 路径未能执行。file 副本没有网络与对象存储延迟，所测 RTO 与复制延迟是**下限**，不能外推到 R2。

**流程**（`scripts/drill/litestream_minio.sh` → `tests/drill/litestream`）：`migrate up` + 建游戏 + 发布配置 → 启动 `litestream replicate` 与 `serve` → N 个并发客户端经 HTTP API 循环执行游客注册、`PUT /saves/main`、`POST /rewards/tick/claim`（每次 +1 coin，写账本）、`POST /boards/main/scores`（分数递增），记录每个已确认（HTTP 200）写入的时间 → 负载进行中对 Litestream 与应用 **同时 SIGKILL** → 在空目录用 `deploy/litestream/restore.sh` 恢复到新路径（含 `backup verify`）→ 用恢复库启动应用 → 用保存的游客凭据重新登录，读取存档 revision、钱包余额、榜单 `me.score`，与确认记录比对。复制延迟：专用探针玩家每 250 ms 保存一次，`litestream restore -f -follow-interval 100ms` 跟随副本，按 50 ms 轮询该副本中出现对应 revision 的时间（上界，含轮询粒度）。

指标定义：**丢失窗口** = kill 时刻减去最早一个已确认但未恢复写入的确认时刻（即观测 RPO）；**最新幸存** = kill 时刻减去最新一个已恢复写入的确认时刻；**RTO** = `restore.sh`（litestream restore + backup verify）耗时 + 恢复库上应用 `/readyz` 就绪耗时（不含人工决策、装机、切流）。

| 演练 | 负载 | sync-interval | wal_autocheckpoint | 确认写入/s | DB@kill + WAL@kill | 恢复库 | 副本占用 | 复制延迟 p50/p95/max | restore.sh | 就绪 | 丢失（存档/领奖/成绩/新玩家） | 丢失窗口 | 最新幸存 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| A1 | 8 并发无间隔，60 s，2 KiB 存档 | 10s | 1000 | 1551 | 267.8 MB + 158.6 MB | 230.2 MB | 440 MB / 220 文件 | 7.02 / 10.76 / 11.30 s | 2.21 s | 0.026 s | 4714/4715/4716/234（共确认 30519/30516/30514/1529） | 9.79 s | 9.85 s |
| A2 | 同上 | 10s | 0 | 2571 | 446.5 MB + 338.4 MB | 391.4 MB | 818 MB / 215 | 6.97 / 10.38 / 11.26 s | 4.00 s | 0.027 s | 6656/6656/6658/332（共 50588/50586/50584/2533） | 8.97 s | 9.22 s |
| B1 | 4 并发、每操作间隔 20 ms，60 s | 1s | 1000 | 175.5 | 30.9 MB + 10.5 MB | 30.9 MB | 64 MB / 174 | 0.60 / 0.87 / 0.89 s | 0.34 s | 0.023 s | 56/57/56/4（共 3454/3452/3450/176） | 0.96 s | 0.99 s |
| B2 | 同上 | 1s | 1000 | 175.4 | 30.8 MB + 10.0 MB | 30.8 MB | 66 MB / 211 | 0.62 / 0.90 / 1.13 s | 0.33 s | 0.025 s | 58/58/57/4（共 3450/3449/3447/176） | 0.97 s | 0.99 s |
| C1 | 预置 60000 玩家×8 KiB 存档，再 8 并发 30 s | 10s | 1000 | 1460 | 1308.3 MB + 248.0 MB | 1268.4 MB | 4278 MB / 448 | 7.22 / 10.73 / 11.25 s | 9.22 s | 0.030 s | 3056/3052/3056/154（共 14361/14356/14356/722） | 6.72 s | 6.78 s |
| C2 | 同上 | 10s | 1000 | 1572 | 1332.1 MB + 240.8 MB | 1278.8 MB | 4282 MB / 417 | 6.93 / 11.94 / 12.41 s | 8.38 s | 0.023 s | 3498/3494/3496/176（共 15465/15460/15458/778） | 6.97 s | 7.03 s |

所有 6 次：`backup verify` 通过（integrity `ok`，schema 10/10 无漂移，账本对账 0 差异）；恢复库上应用启动并通过 API 回读，无读取错误；恢复结果是按确认时间的**干净前缀**（所有幸存写入都早于所有丢失写入，重叠 0 ms；无“未确认但幸存”的写入）；跟随副本中没有超过 3×sync-interval+2s 仍未出现的确认写入。

结论（仅限本环境）：
- 丢失是预期的异步复制窗口，且**每次都发生**：kill 前约一个 sync-interval 内确认的写入全部丢失（10s 时 6.7–9.8 s，1s 时约 1 s），包括已确认的领奖（账本）与新注册玩家（其游客凭据在恢复后返回 401）。客户端若在 kill 前收到成功，恢复后会看到回退。**这与 PRD §11 的边界一致，不能对资金类业务宣称零丢失。**
- 1.27 GB 库从本地 file 副本恢复 + 校验 8.4–9.2 s。R2 上的 RTO 受下载带宽主导（4.3 GB 副本中恢复只需读取快照 + 之后的 LTX），需另测。
- 副本占用约为库大小的 1.6–3.3 倍（L0 保留 5 min、多级压缩与快照）；R2 存储与 Class A 请求成本需按 sync-interval 估算。

## 演练命令

```bash
# Litestream 演练（默认 DRILL_REPLICA=s3，会启动临时 MinIO；本机 MinIO 不可用时用 file）
DRILL_REPLICA=file LITESTREAM_BIN=$(command -v litestream) RUNS=2 DURATION=60s WORKERS=8 \
  SYNC_INTERVAL=10s WAL_AUTOCHECKPOINT=1000,0 scripts/drill/litestream_minio.sh > drillA.json      # A1/A2
DRILL_REPLICA=file RUNS=2 DURATION=60s WORKERS=4 THINK=20ms SYNC_INTERVAL=1s \
  scripts/drill/litestream_minio.sh > drillB.json                                                   # B1/B2
DRILL_REPLICA=file RUNS=2 DURATION=30s WORKERS=8 PAYLOAD=8192 SEED_PLAYERS=60000 SYNC_INTERVAL=10s \
  scripts/drill/litestream_minio.sh > drillC.json                                                   # C1/C2（约 1.3 GB 库）
# S3/MinIO：MINIO_BIN=/path/to/working/minio scripts/drill/litestream_minio.sh
# S3 适配器真实服务测试：scripts/drill/s3_minio.sh（见 docs/api/storage.md）
```
脚本在临时目录构建 CLI、生成并构建应用，标准输出为 JSON 汇总（每次运行的确认数、DB/WAL/副本大小、复制延迟、restore 耗时、verify 结果、丢失明细与窗口），进度输出到 stderr；`KEEP_WORK=1` 保留日志与数据库。MinIO 模式下 root 凭据每次随机生成，仅通过环境变量传给 Litestream，不写入任何配置文件。

## 仍未验证

- Litestream → MinIO / S3 / **Cloudflare R2** 的复制与恢复（本机 MinIO 崩溃，未执行）；R2 上的 RPO/RTO、费用与网络中断行为。
- 1C1G 目标机上的吞吐、WAL 增长与恢复时间；本表的数据来自高配开发机与本地 SSD，不能代表目标环境。
- `restore.sh --timestamp`（时间点恢复）与 `--owner` 未在真实 Litestream 下演练；恢复后重新启用复制（第 6 步）未演练。
