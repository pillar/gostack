# 容灾与恢复（PRD §11）

## 现状与边界

- 已实现并测试（本机 Go 测试，真实文件）：`backup snapshot`（VACUUM INTO 在线一致性快照）、`backup verify`（integrity_check + schema 与二进制兼容 + GameKit 钱包/账本对账）、`backup restore`（先验证、临时文件 + 链接发布、拒绝覆盖）、`deploy/litestream/restore.sh`（Litestream 恢复到新路径 → verify；以桩测试）。
- **未执行**：Litestream → Cloudflare R2 的真实复制与恢复演练（A08）。RPO ≤30s、1 GiB 样本 RTO ≤10min 是**目标**，需在目标环境实测后才能宣称；网络故障期间不保证。
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
