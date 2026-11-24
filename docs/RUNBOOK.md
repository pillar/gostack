# GoStack 故障手册（Runbook）

适用：Lite profile，单机 systemd 部署（`deploy/install.sh` 布局）。每节按 **症状 → 诊断 → 处置** 组织；告警规则见 `deploy/prometheus/alerts.yml`，其 `runbook` 注解指向本文件的小节（`app/alerts_test.go` 校验链接与指标名）。

约定（按实际替换）：

```bash
APP=myapp                                   # 服务名 / 系统用户
BIN=/opt/$APP/current/$APP
CFG=/etc/$APP/gostack.yaml
DB=/var/lib/$APP/app.db
run() { sudo runuser -u $APP -- sh -c 'set -a; . /etc/'$APP'/gostack.env; set +a; exec "$@"' sh "$BIN" --config $CFG "$@"; }
metrics() { curl -s http://127.0.0.1:9090/metrics; }   # http.internal_listen
```

`sqlite3` 命令行仅用于只读诊断（`-readonly`），以服务用户运行；**任何时候都不要删除 `app.db-wal` / `app.db-shm`**，那会丢失已提交事务。

阈值是 1C1G 主机的起点值，不是实测预算（PRD §13 性能预算尚未在目标环境验证）。

---

## Instance down

**症状**：`GoStackDown`（`up == 0`）；`/healthz` 不通。

**诊断**
```bash
systemctl status $APP; journalctl -u $APP -n 200 --no-pager
run doctor                       # 配置、数据目录、磁盘、integrity_check、迁移兼容
run --json migrate status        # "drift": true ⇒ 当前二进制比数据库旧
```
常见原因：配置无效（启动即退出，日志 `invalid configuration`）、实例锁被占用（`database is locked by another instance`：另一个进程/旧实例仍在运行）、迁移 drift（数据库来自更新版本）、OOM（`journalctl -k | grep -i oom`）、磁盘满。

**处置**
- 配置错误：修正后 `run config validate` 再 `systemctl restart $APP`。
- 实例锁：找出持锁进程（`fuser $DB.lock` / `lsof $DB.lock`），确认是重复实例后停止它；不要删除 `.lock` 文件来“解锁”运行中的实例。
- drift：说明数据库已被更新版本迁移，切回该版本（`deploy/upgrade.sh` / `rollback.sh --to <新版本>`），而不是降级二进制。
- 升级后无法启动：见 [Failed upgrade](#failed-upgrade)。

## Write queue 503s

**症状**：`GoStackWriteQueueRejecting` / `GoStackWriteQueueBacklog` / `GoStackHigh5xxRatio`；客户端收到 `503 overloaded`（`write queue full` / `server busy`），`/readyz` 返回 503（写队列 ≥90%）。

**诊断**
```bash
metrics | grep -E '^db_write_(waiting|rejected_total)|^db_busy_retries_total|^http_in_flight|^http_max_concurrent'
metrics | grep 'http_requests_total{.*class="5xx"'      # 哪些路由在报 5xx
curl -s http://127.0.0.1:9090/debug/pprof/goroutine?debug=1 | head -100   # 卡在 WriteTx 的调用栈
journalctl -u $APP --since -15m | grep -E '"status":5' | head
```
区分：
- `db_write_rejected_total` 上升：单写连接处理不过来（写事务慢或写入量超出单机能力）。
- `db_busy_retries_total` 上升：有**其他进程**在写同一数据库（管理 CLI、手工 sqlite3、Litestream checkpoint）。
- 只有 `server busy` 而无写拒绝：`http.max_concurrent` 并发上限触发（`http_in_flight` 接近 `http_max_concurrent`）。
- P99 升高且磁盘延迟高：`iostat -x 1`，检查 `synchronous=FULL` 下的 fsync 延迟。

**处置**
- 突发流量：503 带 `Retry-After`，客户端会退避；确认 SDK 重试使用幂等键。
- 外部写者：停止并发的手工写操作 / 管理命令，避免在高峰期运行 `game config publish` 等。
- 慢事务：用 pprof 定位持有写连接的调用路径；写事务内禁止网络 IO（AGENTS.md 规则）。
- 并发上限：`http.max_concurrent` 可热更新——修改配置后 `systemctl reload $APP`（见 [Config reload](#config-reload)）；`database.write_queue` 需重启。
- 持续超出单机写能力：属于容量问题，记录指标后评估 Scale（PRD §12），不要通过 `synchronous=NORMAL` 换吞吐（生产需显式 `accept_normal_sync_risk`，会丢失断电前已提交事务）。

## WAL growth / checkpoint starvation

**症状**：`GoStackWALLarge` / `GoStackWALCritical`：`db_wal_bytes` 持续增长不回落。

**诊断**
```bash
metrics | grep db_wal_bytes; ls -l $DB $DB-wal
grep -E 'wal_autocheckpoint|backup' $CFG            # 0 表示由 Litestream 管理 checkpoint
curl -s http://127.0.0.1:9090/debug/pprof/goroutine?debug=1 | grep -B2 -A10 'database/sql.(\*Rows)' | head -60   # 长时间未关闭的读
systemctl status litestream; journalctl -u litestream -n 100
```
原因：长时间持有的读事务（未关闭的 rows、超长报表查询）阻止 checkpoint 回绕；`wal_autocheckpoint=0` 而 Litestream 停止/故障；磁盘慢导致 checkpoint 跟不上。

**处置**
- 修复/重启 Litestream（若它负责 checkpoint）。
- 重启应用会关闭所有读连接，下一次 checkpoint 即可回绕：`systemctl restart $APP`（优雅停机，进行中的请求会完成）。
- 手工被动 checkpoint（安全，不阻塞）：`sudo -u $APP sqlite3 $DB 'PRAGMA wal_checkpoint(PASSIVE);'`。若 Litestream 正在复制，`TRUNCATE`/`RESTART` 模式请先按所固定 Litestream 版本的文档确认，避免打断其 WAL 跟踪。
- **禁止**删除或截断 `-wal` 文件。

## Disk full

**症状**：`GoStackDiskLow` / `GoStackDiskCritical`（`disk_free_bytes`）；写入报 `database or disk is full`，checkpoint 失败，WAL 增长。

**诊断**
```bash
df -h /var/lib/$APP; du -sh /var/lib/$APP/* | sort -h
ls -lh /var/lib/$APP/backups/            # 升级前快照
journalctl --disk-usage
```

**处置**（按风险从低到高）
1. 清理旧的升级前快照：`/var/lib/$APP/backups/pre-upgrade-*.db`（保留最近一次且已有异地备份时）。
2. `journalctl --vacuum-size=200M`。
3. 埋点库 `analytics.db` 是 P2 数据：缩短 `game.analytics_retain_days`（需重启），保留期清理每 10 分钟运行一次。
4. 扩容磁盘。**不要**删除 `app.db`、`app.db-wal`、`app.db-shm`。
5. 磁盘恢复后：`run doctor`（integrity_check）并确认 `db_wal_bytes` 回落。

## Jobs dead letters

**症状**：`GoStackJobsDeadLetters`（`jobs_dead > 0`）或 `GoStackJobsBacklogOld`（`jobs_oldest_age_seconds` 大）。

**诊断**
```bash
metrics | grep -E '^jobs_(dead|runnable|oldest_age_seconds)'
sudo -u $APP sqlite3 -readonly $DB "SELECT id,type,attempts,max_attempts,datetime(updated_at/1000,'unixepoch'),substr(last_error,1,200) FROM jobs WHERE status='dead' ORDER BY updated_at DESC LIMIT 20;"
journalctl -u $APP --since -1h | grep -i job | tail -50
```
积压但无死信：worker 卡住或 `queue.workers` 太少（pprof goroutine 查看 handler 是否阻塞在外部调用上）。

**处置**
- 先修复根因（下游服务、数据问题、代码缺陷并发布）。
- 逐个重放：`run jobs replay <job_id>`（任务处理器须幂等；重放会重置尝试次数）。
- 积压：调整 `queue.workers`（需重启）；处理器内的外部调用必须带超时。

## Analytics drops

**症状**：`GoStackAnalyticsDropping`（`analytics_dropped_total` 增长）。按设计，埋点缓冲满时丢弃，不阻塞经济/存档事务（G-A06）。

**诊断**
```bash
metrics | grep -E '^analytics_(accepted_total|dropped_total|buffer_depth)'
ls -l /var/lib/$APP/analytics.db*; df -h /var/lib/$APP
```
原因：突发事件量超过 `game.analytics_buffer`；analytics.db 写入慢（磁盘）；客户端异常重发。

**处置**
- 短时突发可接受（P2 数据）；持续丢弃时增大 `game.analytics_buffer`（需重启，内存随之增加）或降低客户端采样率。
- 检查磁盘与 [Disk full](#disk-full)。

## Memory pressure

**症状**：`GoStackMemoryHigh`（`go_sys_bytes`）；systemd `MemoryMax=768M` 触发 OOM kill（`journalctl -k`）。

**诊断**
```bash
metrics | grep -E '^go_(heap_alloc_bytes|sys_bytes|goroutines)'
curl -s http://127.0.0.1:9090/debug/pprof/heap > heap.pb.gz && go tool pprof -top heap.pb.gz | head -30
```
启用 Web 认证时，每个并发 Argon2id 计算约 19 MiB（`auth.web.max_concurrent_hashes` 限制并发，默认 2）。

**处置**：降低 `http.max_concurrent`（热更新）；检查 goroutine 泄漏；`game.analytics_buffer`、`database.cache_size_kib`、`read_connections` 调小（需重启）。

## Config reload

**症状**：`GoStackConfigReloadRejected`（重载被拒，旧配置继续运行）或 `GoStackConfigRestartPending`（已修改的键需要重启）。

**诊断**
```bash
run config validate                              # 校验 + 列出热更新键
run config diff /path/to/new.yaml                # 与新文件比较：hot / restart
journalctl -u $APP --since -1h | grep -E 'config reload|requires restart'
```
热更新键（SIGHUP / `systemctl reload`）：`log.level`、`http.max_concurrent`、`game.auth_rate_per_minute`、`game.score_rate_per_minute`、`auth.web.ip_rate_per_minute`、`auth.web.account_rate_per_minute`。其余键只在重启后生效；重载时会在日志中逐项报告 `config change requires restart; not applied`。重载使用与启动相同的来源（`--config` 文件、进程环境、原始 `--set`）；systemd `EnvironmentFile` 的修改需要重启才会进入进程环境。

**处置**：修正配置（`config validate` 通过）后再次 `systemctl reload $APP`；需要重启的改动安排维护窗口 `systemctl restart $APP`。

## Backup replication lag

**症状**：Litestream 复制停滞/报错（告警需按所固定的 Litestream 版本的指标自行添加，见 `alerts.yml` 顶部说明）；WAL 增长（若 Litestream 管理 checkpoint）。

**诊断**
```bash
systemctl status litestream; journalctl -u litestream -n 200 --no-pager
litestream status -config /etc/litestream.yml                # Litestream v0.5.x
litestream ltx -config /etc/litestream.yml -level all $DB    # 列出副本中的 LTX 文件
```
检查：R2 凭据是否过期/权限不足（与应用凭据独立）、网络、对象存储错误、本机时钟。

**处置**
- 修复后重启 Litestream，确认最新快照/WAL 段时间推进。
- 复制中断期间 RPO 不受保证；中断较久时额外执行一次本地快照：`run backup snapshot --to /var/lib/$APP/backups/manual-$(date -u +%Y%m%dT%H%M%SZ).db` 并转存异地。
- 定期演练恢复（[Restore](#restore)）。**A08 恢复演练尚未在真实 R2 上执行**（docs/STATUS.md）。

## Restore

完整流程见 `docs/DISASTER_RECOVERY.md`。要点：

```bash
# 从本地快照
run backup verify /path/snap.db
run backup restore --from /path/snap.db --to /var/lib/$APP/restore/app.db
# 从 Litestream 副本
sudo deploy/litestream/restore.sh --bin $BIN --replica-config /etc/litestream.yml \
     --db $DB --to /var/lib/$APP/restore/app-$(date -u +%Y%m%dT%H%M%SZ).db --owner $APP
```
两者都**拒绝覆盖**已有文件，并在写入前/后执行 `backup verify`（integrity_check、schema 与二进制兼容、钱包余额与账本对账）。切换前先隔离旧实例，防止双写。

## Failed upgrade

**症状**：`deploy/upgrade.sh` 退出码非 0。

- **退出码 1，日志 `rolled back to <old> (healthy)`**：新版本健康检查失败，`current` 已切回旧版本并重启成功。查看 `journalctl -u $APP`，修复后重新发布新版本号。
- **退出码 1，切换前失败**（sha256 不符、preflight 拒绝、快照失败）：生产未受影响。
- **退出码 3**：回滚后旧版本也不健康。最常见原因是新版本已执行迁移，旧二进制拒绝在更新的 schema 上运行（drift）。处置：
  1. 优先**向前修复**：修复新版本问题后用新版本号重新 `upgrade.sh`（schema 已是新版本）。
  2. 必须回到旧版本时，用升级前快照恢复（快照之后写入的数据会丢失，需评估）：
     ```bash
     systemctl stop $APP
     run backup verify /var/lib/$APP/backups/pre-upgrade-<old>-to-<new>-<ts>.db
     run backup restore --from /var/lib/$APP/backups/pre-upgrade-...db --to /var/lib/$APP/app-restored.db
     # 将旧库及 -wal/-shm 移到一旁（不要删除），再把恢复的文件改名为 app.db 或修改 GOSTACK_DATABASE_PATH
     systemctl start $APP
     ```
- `rollback.sh` 拒绝（`database schema is newer than or drifted from ...`）：同上，二进制回滚不能撤销 schema 变更（PRD §11）。

升级前快照保存在 `/var/lib/$APP/backups/`，需定期清理（见 [Disk full](#disk-full)）。
