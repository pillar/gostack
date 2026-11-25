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

### S3 副本（MinIO）演练，2026-11-25

Homebrew 的 MinIO 在本机崩溃后，改用源码编译的 MinIO（`CGO_ENABLED=0 go install github.com/minio/minio@latest`，`minio version DEVELOPMENT.GOGET`，go1.27.4）作为 S3 兼容端点（path-style，随机 root 凭据），其余环境同上。命令：`MINIO_BIN=~/go/bin/minio LITESTREAM_BIN=~/go/bin/litestream DRILL_REPLICA=s3 RUNS=2 DURATION=60s scripts/drill/litestream_minio.sh`。

| 演练 | 负载 | sync-interval | 确认写入/s | DB@kill + WAL@kill | 恢复库 | 复制延迟 p50/p95/max | restore.sh | 就绪 | 丢失（存档/领奖/成绩/新玩家） | 丢失窗口 |
|---|---|---|---|---|---|---|---|---|---|---|
| S1 | 8 并发无间隔，60 s，2 KiB 存档 | 10s | 1776 | 308.3 MB + 133.3 MB | 265.4 MB | 6.61 / 10.55 / 11.43 s | 2.27 s | 0.025 s | 5186/5185/5184/258（共确认 34941/34937/34935/1750） | 9.36 s |
| S2 | 同上 | 10s | 1816 | 314.2 MB + 239.7 MB | 273.8 MB | 6.23 / 10.08 / 11.31 s | 2.50 s | 0.02 s | 5018/5016/5017/251（共 35728/35725/35723/1791） | 9.78 s |

两次均：`backup verify` 通过（integrity `ok`、schema 11/11、对账 0 差异），恢复库上应用就绪并通过 API 回读；恢复结果是干净前缀（无未确认但幸存的写入）；没有确认写入始终未出现在副本中。结论与 file 副本一致：丢失窗口约为一个 sync-interval。MinIO 在本机回环上，**不代表 R2 的网络延迟与带宽**。

### Cloudflare R2 演练，2026-11-25

从本机（macOS，经公网）复制到 Cloudflare R2（桶位置 APAC，专用测试桶 + 仅授权该桶读写的临时 Token，演练后删除），Litestream v0.5.18，命令：`DRILL_REPLICA=s3 DRILL_S3_ENDPOINT=https://<account>.r2.cloudflarestorage.com DRILL_S3_BUCKET=<bucket> DRILL_S3_REGION=auto LITESTREAM_ACCESS_KEY_ID=… LITESTREAM_SECRET_ACCESS_KEY=… scripts/drill/litestream_minio.sh`。

| 演练 | 负载 | sync-interval | 确认写入/s | 复制延迟 p50/p95/max | restore.sh | 丢失（存档/领奖/成绩/新玩家） | 丢失窗口 |
|---|---|---|---|---|---|---|---|
| R1 | 8 并发无间隔，60 s | 10s | 2015 | 14.8 / 22.3 / 22.9 s | 5.17 s | 12552/12554/12555/626 | 21.8 s |
| R2 | 同上 | 10s | 2053 | 16.2 / 23.9 / 24.6 s | 6.32 s | 14370/14370/14370/718 | 23.9 s |
| R3 | 4 并发、每操作间隔 20 ms，60 s | 1s | 186 | 1.35 / 2.15 / 2.73 s | 2.73 s | 60/57/60/3 | 0.96 s |
| R4 | 同上 | 1s | 184 | 1.30 / 2.35 / 3.02 s | 2.77 s | 59/58/58/4 | 0.96 s |

4 次均：`backup verify` 通过（integrity ok、schema 一致、账本对账 0 差异），恢复库上应用就绪并通过 API 回读；恢复结果是干净前缀（无未确认但幸存的写入），没有确认写入始终未出现在副本中。结论：
- 高写入（约 2000 写/s）下，经公网到 R2 的复制延迟明显高于本机 MinIO（p95 约 22–24 s 对约 10 s），丢失窗口 22–24 s，仍在 PRD 的初始目标 RPO ≤ 30 s 内但余量小；中等负载 + `sync-interval: 1s` 时丢失窗口约 1 s。
- 应用侧以「已确认写入可能丢失最近一个复制周期」为前提设计；资金类数据若要求更小 RPO，需缩短 sync-interval（R2 请求费用随之增加）或采用同步方案。
- 网络路径为开发机到 R2，不是目标服务器到 R2；目标机上的数字需另测。

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

- Litestream → R2 已从开发机演练（见上）；**目标服务器 → R2** 的 RPO/RTO、R2 费用与网络中断期间的行为未测；AWS S3 未测。
- 1C1G 目标机上的吞吐、WAL 增长与恢复时间；本表的数据来自高配开发机与本地 SSD，不能代表目标环境。
- `restore.sh --timestamp`（时间点恢复）与 `--owner` 未在真实 Litestream 下演练；恢复后重新启用复制（第 6 步）未演练。

---

# PostgreSQL（profile scale，PRD §11/§12「Scale 使用 PG 备份/PITR」）

两层手段，互补而非替代：

| 手段 | 作用 | 恢复点 | 实现 |
|---|---|---|---|
| 逻辑备份 `backup snapshot/verify/restore` | 可移植的一致性快照（跨机器/跨小版本、抽查、归档） | 快照时刻 | `app/backup_pg.go`（pg_dump/pg_restore 自定义格式 + JSON 清单） |
| 物理备份 + WAL 归档（PITR） | 机器丢失、误删/误改后回到任意时间点 | 归档覆盖范围内任意时刻 | `deploy/postgres/`（服务器配置片段与脚本，PostgreSQL 自带工具） |

## 逻辑备份命令

```bash
APP --config CFG [--json] backup snapshot --to /backups/app-<ts>.dump
#   写出 app-<ts>.dump（pg_dump -Fc，主库全部 schema，含 gostack_analytics）、
#   app-<ts>.dump.json（清单）；analytics_dsn_ref 指向独立库时另写 app-<ts>.dump.analytics。
#   任一目标已存在即拒绝；先写临时文件再链接发布。
APP --config CFG [--json] backup verify [--dsn-ref env:SCRATCH] /backups/app-<ts>.dump
APP --config CFG [--json] backup restore --from /backups/app-<ts>.dump --to-dsn-ref env:NEW_DB [--analytics-to-dsn-ref env:NEW_AN]
```

- **一致性**：snapshot 在一个 `REPEATABLE READ READ ONLY` 事务中执行 `pg_export_snapshot()`，在同一快照内统计每张表行数、各 schema 最高迁移版本、`game_ledger` 按资产求和、`billing_ledger_entries` 的 `delta_available/delta_reserved` 求和，再以 `pg_dump --snapshot=<id>` 导出。清单与归档描述的是**同一份数据**，因此恢复后必须逐项相等。独立 analytics 库是另一个快照（P2 数据，不与主库严格一致）。
- **口令与其他秘密参数**：DSN 只经 `database.dsn_ref`（env:/file:）解析；传给 pg_dump/pg_restore 的 `--dbname` 去掉全部秘密参数（`password`、`sslpassword`、`oauth_client_secret`），不出现在 argv/`ps` 中；错误信息不回显 DSN。`password` 经子进程环境变量 `PGPASSWORD` 传递；libpq 没有 `sslpassword` 等参数对应的环境变量（PG 17 libpq 的环境变量表中无 `PGSSLPASSWORD`），因此写入临时 0600 服务文件（`PGSERVICEFILE` + `PGSERVICE=gostack_tool`，libpq 对连接串未给出的参数取服务文件中的值），工具退出后删除。DSN 自带 `service=` 或环境已设 `PGSERVICE` 时，若同时含 `sslpassword` 则拒绝（请把它写进自己的服务文件）；含换行或尾随空白的值无法写入服务文件，同样拒绝。`sslkey`、`passfile` 是路径而非秘密，保留在 DSN 中。单测 `TestPGDSNKeepsPasswordOutOfArgs`、`TestPGDSNKeepsAllSecretsOutOfArgs` 覆盖 URL（userinfo、`?password=`、`?sslpassword=`）与 key=value（含引号转义）两种形式；`TestPGToolsWithSSLPasswordViaServiceFile` 在真实 PostgreSQL 上用含 `sslpassword` 的 DSN 运行 pg_dump。
- **verify**：校验归档 sha256/大小与清单一致 → 在 `--dsn-ref`（默认 `database.dsn_ref`）所在服务器 `CREATE DATABASE gs_verify_<随机> TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C'`（角色需 CREATEDB）→ `pg_restore --single-transaction --exit-on-error --no-owner --no-privileges` → 主库与 analytics schema 的迁移状态对比本二进制（待处理可接受，未知/漂移失败）→ GameKit 钱包/账本对账 → 计费对账 → 与清单比较行数、账本和、schema 版本 → `DROP DATABASE … WITH (FORCE)`（失败路径同样删除）。文件以 `PGDMP` 开头时自动按 PG 处理；非 PG 配置下需 `--dsn-ref`。
- **restore**：目标库必须已创建（C 排序）且为空（无任何用户关系；独立 analytics 目标不得已有 `gostack_analytics`），否则拒绝；先校验清单与校验和，再单事务 `pg_restore`（失败则目标保持为空），然后在目标库上原地执行与 verify 相同的检查。对象属主为连接角色（`--no-owner`）。
- **退出码**与 SQLite 版一致：0 通过；3 检查失败（归档截断/篡改、缺清单、pg_restore 失败、对账不平、清单不符、schema 比本二进制新或漂移）；1 运行错误（目标非空、无 pg_dump/pg_restore、连接失败、角色无 CREATEDB）；2 用法错误（含 `--to-dsn-ref` 传字面 DSN）。
- `--json` 字段：`ok`、`file`、`bytes`、`sha256`、`database`、`restore`、`schema`、`analytics_schema`、`reconcile_mismatches`、`billing_reconcile_mismatches`、`manifest_differences`、`problems`。
- 需要 PostgreSQL 客户端工具在 PATH 中，主版本 ≥ 服务器（pg_dump 拒绝导出更新的服务器）。
- 代价：snapshot 对每张表做一次 `count(*)`；verify 需要与库同量级的临时空间与时间。大库上请在低峰执行，并为 verify 准备单独的服务器（`--dsn-ref`）。

测试（`GOSTACK_TEST_POSTGRES_DSN` 已设置且 PATH 中有 pg_dump/pg_restore 时运行，否则跳过）：`app/backup_pg_test.go` — 快照 → 拒绝覆盖（含只存在清单）→ verify 通过且临时库已删除 → `--dsn-ref` 无 PG 配置也可验证 → 清单行数被改、归档截断（清单同步/不同步两种）、缺清单、垃圾文件均退出 3，且不残留临时库 → restore 拒绝非空目标与字面 DSN → 截断归档的 restore 失败后目标仍为空 → 正常 restore 后应用在新库上启动、余额/计费账户正确、可继续写入 → 二次 restore 被拒 → 源库钱包被篡改且有未知迁移 9999 时 verify 报 `1 mismatches` 与 `unknown=[9999]`；独立 analytics 库的快照/验证/分别恢复与缺失 analytics 归档检测。`tests/scale` 的 SQLite→PG 迁移测试在迁移后对 PG 运行 snapshot + verify。

## PITR：WAL 归档 + 基础备份

文件（`deploy/postgres/`）：
- `postgresql.pitr.conf`：`wal_level=replica`、`archive_mode=on`、`archive_command='…/wal-archive.sh %p %f ARCHIVE_DIR'`、`archive_timeout=60s`、`max_wal_senders=10`。`archive_mode` 需重启。
- `wal-archive.sh`：复制到临时名 → fsync → 用 `ln`（硬链接）发布到最终名；`ln` 在目标已存在时原子失败（`mv` 会覆盖），因此检查之后才出现的同名文件也不会被替换：同名同内容视为成功（崩溃后重试），同名不同内容**拒绝**（防止两个集群写同一归档目录）；拒绝含路径的文件名。归档目录所在文件系统必须支持硬链接（不支持时脚本报错退出，不会静默覆盖）。`deploy/postgres_test.go` 用 PATH 中的 `cp` 垫片在检查后注入同名文件，覆盖“不同内容被拒且不被覆盖”和“相同内容成功”两种竞争。
- `wal-restore.sh`：`restore_command` 助手，文件不存在时非零退出（归档结束）。
- `base-backup.sh`：`pg_basebackup -Fp -X stream -c fast --manifest-checksums=SHA256` 到新目录，然后 `pg_verifybackup`；口令经 `PGPASSWORD`/`.pgpass`；失败时删除目标；输出 JSON（起始 LSN/时间）。角色需 REPLICATION。
- `pitr-restore.sh`：把基础备份复制到**新的**数据目录，写 `recovery.signal`，在 `postgresql.auto.conf` 追加 `restore_command`、`recovery_target_action='promote'`、可选 `recovery_target_time`（`recovery_target_inclusive=on`），并设 `archive_mode=off`（避免恢复出的新时间线写回正在使用的归档）。不启动服务器。
- `deploy/postgres_test.go` 在无服务器的情况下执行这些脚本（归档/重试/拒绝覆盖/恢复助手/恢复目录准备与拒绝）。

生产建议：归档目录必须在数据库机器丢失后仍可用（另一台机器/磁盘）；更推荐 **pgBackRest** 或 **WAL-G**（推送到 S3/R2、保留策略、并行恢复、加密），GoStack 不依赖二者，替换 `archive_command`/`restore_command` 即可。需要「机器丢失零丢失」时，归档不够：使用同步备库或 `pg_receivewal --synchronous` + `synchronous_standby_names`（未在本项目中实现或演练）。

### PITR 恢复流程

1. 隔离旧主库（停止或在网络上切断），确保不会双写；停止所有应用实例。
2. 选定目标时间 T（UTC，微秒），必须晚于所用基础备份结束时间；不指定则恢复到归档末尾。
3. `deploy/postgres/pitr-restore.sh --base BASE --archive ARCHIVE --to NEWDATA [--target-time 'YYYY-MM-DD HH:MM:SS.ffffff+00']`
4. `pg_ctl -D NEWDATA -o "-p PORT" -l NEWDATA/recovery.log start`；等待 `SELECT pg_is_in_recovery()` 为 `f`。日志中 `recovery stopping before commit …`/`last completed transaction was at log time …` 给出实际恢复点。
5. 校验：`APP backup snapshot --to X && APP backup verify X`（或至少 `APP game reconcile`、`APP migrate status`）。
6. 将 `database.dsn_ref` 指向新集群，启动应用，确认 `/readyz`。
7. 为新集群配置**新的**归档目录、开启 `archive_mode`（重启）并立即做一次新的基础备份。

### PITR 演练（`scripts/drill/pg_pitr.sh` → `tests/drill/pgpitr`）

每次运行：`initdb`（scram-sha-256，随机口令，仅 127.0.0.1 TCP）→ 附加 `deploy/postgres/postgresql.pitr.conf`（路径替换、`archive_timeout` 可调）→ 创建 `gostack` 角色（CREATEDB）与 C 排序库 → `gostack init --kit game --gostack-path` 生成的应用以 profile scale（`dsn_ref: env:…`、`queue.driver: postgres`、`rate_limit.accept_per_instance: true`）运行 → N 个客户端经 HTTP 循环执行游客注册、`PUT /saves/main`、`POST /rewards/tick/claim`、`POST /boards/main/scores`，记录每个写入的**发送时刻与确认时刻** → 负载 3 s 后用 `base-backup.sh` 做基础备份，并做一次 `backup snapshot` + `backup verify`（负载中、scram 口令经环境变量）→ 负载中点记录 T → 负载结束时对 PostgreSQL **整个进程组 SIGKILL**（模拟机器丢失）并杀应用 → 分别用 `pitr-restore.sh` 恢复两个新集群：(a) 到 T，(b) 到归档末尾（latest）→ 各自启动应用，运行 `migrate status`、`game reconcile`、`backup snapshot` + `backup verify`，并用保存的游客凭据经 HTTP 回读存档 revision、coin 余额、榜单 `me.score`。

判定（客户端与服务器同一时钟；PostgreSQL 提交时间戳精度 1 µs）：
- 到 T：确认时刻 ≤ T 的写入必须存在（提交早于确认）；发送时刻 > T+1µs 的写入必须不存在（提交晚于发送）；两者之间为边界在途写入，任意结果都正确，单列统计。
- latest：以日志中 `last completed transaction was at log time`（最后重放的提交时间 L）为界，确认时刻 ≤ L 的写入必须存在；`redo done` 的 LSN 必须进入最后一个已归档段（即已归档的 WAL 全部重放）。RPO = kill 时刻 − L。
- RTO = `pitr-restore.sh`（复制基础备份）+ 启动到 promote 完成 + 应用 `/readyz`；不含人工决策、装机、切流。

### 本地演练结果（2026-11-25，**本机 macOS 数据，不代表 1C1G 或生产环境**）

环境：Apple M5 Pro（15 核）、64 GiB、macOS（Darwin 27.0.0, arm64）、本机 SSD；PostgreSQL 17.11（Homebrew）；Go 1.27.4。归档目录为**同一块本地 SSD 上的目录**（无网络/对象存储延迟），基础备份、归档、恢复集群都在同一台机器上；RTO 为下限。

命令：
```bash
PG_BIN=/opt/homebrew/bin RUNS=2 DURATION=60s WORKERS=8 scripts/drill/pg_pitr.sh > drillA.json             # A1/A2
PG_BIN=/opt/homebrew/bin RUNS=2 DURATION=60s WORKERS=4 THINK=20ms scripts/drill/pg_pitr.sh > drillB.json  # B1/B2
```
两条命令均退出 0（JSON `"ok": true`）。`archive_timeout=10s`。

| 演练 | 负载 | 确认写入/s（总数） | 基础备份 | 负载中逻辑快照 + verify | 库目录@kill | 归档段（大小） | kill 时未归档 WAL（最后采样） | 到 T：RTO（复制/恢复/应用） | 到 T：必须存在/缺失 · 必须不存在/出现 · 在途 | latest：RTO | latest：必须存在/缺失 · 丢失确认写入 | latest：RPO |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| A1 | 8 并发无间隔，60 s | 3089（185368） | 0.65 s，92 MB | 4.51 s + 0.63 s，OK | 547 MB | 40（671 MB） | ≈13 MB | 2.38 s（0.40/1.87/0.11） | 102907/0 · 82453/0 · 8 | 3.66 s（0.41/3.14/0.11） | 180794/0 · 4572 | 1.41 s |
| A2 | 同上 | 3933（235967） | 0.71 s，87 MB | 3.27 s + 0.66 s，OK | 683 MB | 52（872 MB） | ≈7 MB | 2.23 s（0.34/1.79/0.11） | 121419/0 · 114540/0 · 8 | 3.72 s（0.32/3.36/0.04） | 233156/0 · 2809 | 0.77 s |
| B1 | 4 并发、每操作间隔 20 ms，60 s | 177（10595） | 0.52 s，53 MB | 0.24 s + 0.31 s，OK | 63 MB | 7（117 MB） | ≈3 MB | 1.71 s（0.78/0.82/0.11） | 5377/0 · 5215/0 · 3 | 2.27 s（1.18/0.99/0.10） | 9566/0 · 1028 | 5.87 s |
| B2 | 同上 | 177（10636） | 0.52 s，53 MB | 0.25 s + 0.26 s，OK | 63 MB | 7（117 MB） | ≈4 MB | 0.87 s（0.42/0.40/0.05） | 5411/0 · 5222/0 · 3 | 1.02 s（0.41/0.55/0.07） | 9402/0 · 1233 | 6.90 s |

所有 8 次恢复：`migrate status` 12/12 无漂移、`game reconcile` 0 差异、`backup verify` 通过（清单 0 差异）；到 T 的恢复日志为 `recovery stopping before commit of transaction …`，**0 个 T 前确认的写入缺失、0 个 T 后发送的写入出现**；latest 恢复全部重放到最后一个已归档段，T/L 之前确认的写入 0 缺失。

结论（仅限本环境）：
- 时间点恢复按提交时间精确截断，可用于“回到误操作之前”。恢复后客户端在 T 之后收到成功的写入（含领奖账本、新注册玩家）全部回退，需要业务上的补偿/公告流程。
- 机器丢失时（只剩基础备份 + 归档）**会丢数据**：丢失的是尚未归档的当前 WAL 段。高写入下段约每 1 s 写满，RPO 0.8–1.4 s；低写入下由 `archive_timeout` 决定，10 s 时观测 5.9–6.9 s（上界约为 archive_timeout）。生产默认 60 s 意味着低峰期最多约 1 分钟的已确认写入可能丢失；更小的 RPO 需要更小的 `archive_timeout`（每次切换归档整段 16 MB）或流式复制/`pg_receivewal`。
- RTO 主要是 WAL 重放；本次 36–48 个段重放 3.1–3.4 s。实际 RTO 取决于基础备份大小与基础备份后的 WAL 量（应定期做基础备份）以及归档所在存储的读取速度。

### 仍未验证（PostgreSQL）

- 远程/对象存储归档（pgBackRest、WAL-G、NFS）及其网络中断行为；1C1G 目标机上的备份/恢复时间；大库（GB 级以上）的 pg_dump/verify 与基础备份耗时。
- tar 格式/压缩基础备份（脚本只做 plain 格式）；跨主版本恢复；`backup restore` 到托管 PG（权限受限、无 CREATEDB 时 verify 需 `--dsn-ref` 指向其他服务器）。
- 恢复后重新开启归档并做新基础备份（流程第 7 步）未在演练中执行。
- HA/同步复制、`pg_receivewal` 零丢失方案未实现。
