# 部署（Lite / systemd）

目标环境：Linux + systemd + util-linux（`runuser`）。脚本位于 `deploy/`，`gostack init` 生成的项目同样包含 `deploy/` 与 `scripts/release.sh`（与本仓库副本逐字节一致，`templates/templates_test.go` 校验）。

**验证状态**：脚本已在 macOS 上以桩（stub）方式实跑测试（`deploy/deploy_test.go`：临时 `DEPLOY_ROOT`，`systemctl`/`useradd`/`chown`/`curl`/`litestream` 为桩，二进制、SQLite、符号链接切换、sha256、快照与迁移兼容检查为真实执行）。**尚未在真实 Linux/systemd 主机上运行**（root、`useradd`、`runuser`、`systemctl` 的真实行为未验证）。

## 1. 构建发布物

```bash
scripts/release.sh --version v1.2.3 --out dist            # GoStack 仓库：构建 ./cmd/gostack
scripts/release.sh --version v1.2.3 --out dist            # 生成的项目：构建 . （应用 main 包）
```
产物：`<name>-<version>-linux-{amd64,arm64}`（`CGO_ENABLED=0`、`-trimpath`、`-X github.com/pillar/gostack/app.Version=<version>`）、对应 `.sbom.json`、`SHA256SUMS`。

SBOM 说明：`.sbom.json` 是 `go version -m -json <binary>` 的输出（Go `runtime/debug.BuildInfo`：主模块、所有链接进二进制的依赖模块版本与 go.sum 哈希、构建参数）。**它不是 SPDX/CycloneDX 格式**，需要这些格式时请用专门工具转换。注意 `-trimpath` 下 BuildInfo 不记录 `-ldflags`，版本号以二进制内嵌字符串与文件名为准。

## 2. 目录布局

```
/opt/APP/releases/<version>/APP     只读发布物（root 所有）+ SHA256SUMS
/opt/APP/current  -> releases/<v>   原子切换（ln -sfn + rename）
/opt/APP/previous -> releases/<v>   上一个版本（rollback 默认目标）
/etc/APP/gostack.yaml               配置 0640 root:APP（脚本不会覆盖已有文件）
/etc/APP/gostack.env                服务环境 0640 root:APP（GOSTACK_ENVIRONMENT=production、数据库路径）
/var/lib/APP/                       数据 0750 APP:APP；backups/ 存放升级前快照
/etc/systemd/system/APP.service     由 install.sh 安装（模板：deploy/systemd/APP.service）
```
单元文件使用 `EnvironmentFile=/etc/APP/gostack.env`，部署脚本以相同环境运行预检，保证预检与服务看到同一数据库。

## 3. 首次安装

```bash
sudo deploy/install.sh --name APP --binary dist/APP-v1.2.3-linux-amd64 --version v1.2.3 \
  --sums dist/SHA256SUMS --unit deploy/systemd/APP.service --config gostack.yaml \
  --health-url http://127.0.0.1:8080/readyz --timeout 60
```
步骤：sha256 校验（必须提供 `--sha256` 或 `--sums`）→ 创建系统用户（nologin）→ 目录与权限 → 暂存 `releases/<version>`（已存在且内容不同则拒绝）→ 安装配置/环境文件（不覆盖）→ 预检（`migrate status --json` 拒绝 drift；`doctor`）→ 安装并 enable 单元 → 原子切换 `current` → 重启 → 轮询 `/readyz`。若 `current` 已存在则拒绝（请用 upgrade）。

## 4. 升级

```bash
sudo deploy/upgrade.sh --name APP --binary dist/APP-v1.3.0-linux-amd64 --version v1.3.0 --sums dist/SHA256SUMS
```
步骤：sha256 → 暂存 → 用**新二进制**预检（数据库 schema 若比新二进制新或校验和漂移则拒绝；`doctor`）→ 在线快照 `backup snapshot`（VACUUM INTO，只读连接，不停服）+ `backup verify` → `previous` 指向旧版本、`current` 指向新版本 → `systemctl restart`（新版本启动时自动执行待处理迁移）→ 健康检查。

失败处理：健康检查失败时自动把 `current` 切回旧版本并重启。退出码 0 成功；1 失败但旧版本健康（或在切换前失败）；3 回滚后旧版本也不健康——通常是新版本已执行迁移，旧二进制拒绝运行，脚本会打印升级前快照路径，见 `docs/RUNBOOK.md#failed-upgrade`。

注意：快照在旧版本运行时生成，快照之后到重启之间的写入不在快照中。破坏性 schema 变更不能靠二进制回滚撤销（PRD §11）；迁移应保持向前兼容（先加列、后切换、再清理）。

## 5. 回滚

```bash
sudo deploy/rollback.sh --name APP                 # 回到 previous
sudo deploy/rollback.sh --name APP --to v1.2.3     # 指定版本（须仍在 releases/ 中）
```
目标二进制先对生产数据库执行 `migrate status --json`：若存在它不认识的迁移版本或校验和漂移（顶层 `"drift": true`），**拒绝回滚**且不改动 `current`。之后同样执行 `doctor`、切换、重启、健康检查；失败则切回原版本。

## 6. 配置热更新

`systemctl reload APP`（发送 SIGHUP）重新读取同一来源（配置文件、进程环境、原始 `--set`），完整校验后原子替换快照。只有热更新键生效（`APP config validate` 列出）；其他键的变化写日志 `config change requires restart; not applied` 并计入 `config_restart_pending_keys`。配置无效时保留旧快照、记录错误并计入 `config_reloads_total{result="rejected"}`。修改 `/etc/APP/gostack.env` 需要 `restart`。

## 7. 监控

`http.internal_listen: 127.0.0.1:9090` 暴露 `/metrics` 与 pprof（不要公网暴露）。告警规则：`deploy/prometheus/alerts.yml`（Prometheus job 名 `gostack`）；处置手册：`docs/RUNBOOK.md`。Litestream 复制延迟告警需按所固定的 Litestream 版本指标自行补充。
