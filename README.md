# GoStack

轻量优先的 Go 后端框架。默认 Lite profile（SQLite WAL + systemd，单二进制，`CGO_ENABLED=0`，目标 1C1G），可选 Scale profile（PostgreSQL + 可选 Valkey，多实例）。业务 Kit 与基础设施 profile 正交，按需启用：

- **GameKit**（`--kit game`）：H5 / 微信小游戏后端，配套 TypeScript 游戏 SDK。
- **WebKit**（`--kit web`）：网站、SaaS 与 API 后端的基础能力（内容、订单、webhook）。
- **AgentKit**（`--kit agent`）：可恢复、可审计的 Agent 运行（步骤 checkpoint、工具调用对账、预算）。
- **无 Kit**（`--kit` 省略）：只用通用核心写普通 API 服务。

SaaS、MaaS（模型即服务 / API 计量售卖）这类产品可以用通用核心组合：租户 API key 与 scope、Web 会话认证、预付余额 + 预留/结算计费（适合"先预留、按实际用量结算"）、全局限流、幂等、任务队列、webhook outbox、对象存储。**目前没有专门的 SaaS 或 MaaS Kit**，缺口见下文"尚未验证或未实现"。

- API 契约：`docs/openapi/gostack.yaml`（全部路由）、`docs/GAME_API.md`（GameKit 核心）、`docs/api/`（各模块与 Kit）
- 实现与验证状态：`docs/STATUS.md`（只列已验证的能力）
- 开发约定与命令：`AGENTS.md`；架构：`docs/ARCHITECTURE.md`

## 功能概览

| 领域 | 内容 | 文档 |
|---|---|---|
| 通用核心 | CLI 与项目生成器、HTTP 中间件、租户 API key（scope）、任务队列、幂等、限流、可观测性、备份与恢复、systemd 部署 | `docs/DEPLOY.md`、`docs/RUNBOOK.md`、`docs/DISASTER_RECOVERY.md` |
| Web 认证（可选） | 密码登录、Cookie 会话、CSRF | `docs/api/webauth.md`、`docs/SECURITY.md` |
| 计费（可选） | 预付余额、预留/结算、追加式账本（不是支付系统，不接支付渠道） | `docs/api/billing.md` |
| 对象存储 | 本地文件与 S3 兼容适配器（预签名 URL；MinIO、Cloudflare R2 已实测） | `docs/api/storage.md` |
| 缓存 | 进程内或 Valkey，只缓存非业务事实 | `docs/adr/0004-valkey.md` |
| WebKit（P1） | 站点/页面/文章与不可变修订、定时发布、SEO、sitemap/robots；订单状态机示例（接计费）；签名 webhook outbox（SSRF 防护） | `docs/api/web.md` |
| AgentKit（P1） | run 与步骤状态、每步 checkpoint、回放式恢复、不确定工具调用进入人工对账、取消/超时、步数与成本预算、租户与工作流并发上限、工具调用审计 | `docs/api/agent.md` |
| GameKit 核心 | 游客/微信登录、云存档、排行榜与赛季、钱包/道具/不可变账本、远程配置与灰度、埋点、反作弊 | `docs/GAME_API.md`、`docs/GAMEKIT.md` |
| GameKit 扩展 | 运营（公告/活动/邮件/签到/任务/兑换码）、A/B 实验、社交、匹配与房间、商业化（Apple/Google Play/微信支付/广告回调）、WebSocket 实时、资源发布、每游戏配额 | `docs/api/` |
| Scale profile | PostgreSQL 存储与任务队列、多实例、`migrate to-postgres`、Valkey 全局限流与缓存、PG PITR | `docs/adr/0003-postgres-scale.md`、`docs/adr/0004-valkey.md` |

## 尚未验证或未实现

- 性能：Linux 云主机上用 cgroup 模拟 1C1G，PRD §13/§23 的 9 个目标场景达标（`docs/BENCHMARK_REPORT.md` §8），但**不是正式验收**（模拟环境、压测端同机、稳态 60 s）。这些场景都走 GameKit 接口；**WebKit、AgentKit、无 Kit 应用和 Scale profile 未压测**。
- 真实平台：微信登录、Apple/Google Play、微信支付、广告回调只用本地密钥或假服务测试；SDK 未在 Safari/微信真机验证。
- SaaS 缺口：组织/成员/角色/邀请、订阅套餐与周期计费、真实支付渠道（如 Stripe）、管理后台 UI。
- MaaS 缺口：模型网关/反向代理、流式响应（SSE）转发、按 token 计量、模型目录与路由、OpenAI 兼容接口。
- Scale：未实现 PG/Valkey HA、Valkey TLS、实时模块的 Valkey 总线；`distributed` profile、pgmq 驱动未实现，配置校验会拒绝。

## 快速开始

```bash
go install github.com/pillar/gostack/cmd/gostack@latest   # 或在仓库内 go build -o bin/gostack ./cmd/gostack
gostack init mygame --kit game        # 或 --kit web / --kit agent；省略 --kit 只生成通用核心
# go mod tidy 拉取 github.com/pillar/gostack 最新版本；开发框架本身时可加 --gostack-path "$PWD"，生成本地 replace
cd mygame && go mod tidy && go build -o bin/mygame .
./bin/mygame --config gostack.yaml migrate up
./bin/mygame --config gostack.yaml game create demo --origin http://localhost:5173
./bin/mygame --config gostack.yaml game config publish demo game-config.json
./bin/mygame --config gostack.yaml serve
```

`game create` / `game config publish` 只适用于 GameKit；web、agent 项目生成后的步骤见各自 `README.md` 与 `docs/api/web.md`、`docs/api/agent.md`。游戏客户端：见 `sdk/typescript/README.md`。

## 验证

```bash
./scripts/ci.sh
```

未实现的 profile（distributed）和驱动（pgmq 等）会被配置校验拒绝，而不是假装可用；Scale profile 需要 PostgreSQL（可选 Valkey），见 ADR-0003/0004。

## License

MIT, see `LICENSE`.
