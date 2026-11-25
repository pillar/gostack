# GoStack

轻量优先的 Go 后端框架：默认 Lite profile（SQLite WAL + systemd，单二进制，`CGO_ENABLED=0`），可选 Scale profile（PostgreSQL + 可选 Valkey，多实例），以及面向 H5 / 微信小游戏的 GameKit 和 TypeScript 游戏 SDK。

- API 契约：`docs/openapi/gostack.yaml`（全部路由）、`docs/GAME_API.md`（GameKit 核心）、`docs/api/`（各模块）
- 实现与验证状态：`docs/STATUS.md`（只列已验证的能力）
- 开发约定与命令：`AGENTS.md`；架构：`docs/ARCHITECTURE.md`

## 功能概览

| 领域 | 内容 | 文档 |
|---|---|---|
| GameKit 核心 | 游客/平台（微信）登录、云存档（版本冲突、历史、可选合并）、排行榜与赛季结算、钱包/道具/不可变账本、远程配置与灰度、埋点、反作弊（隔离审核、对局令牌） | `docs/GAME_API.md`、`docs/GAMEKIT.md` |
| GameKit 运营 | 公告、活动、邮件、签到、任务、兑换码 | `docs/api/ops.md` |
| GameKit 实验 | 分层 A/B 实验与一键停止 | `docs/api/experiments.md` |
| GameKit 社交 | 资料与隐私、好友、黑名单、好友榜、邀请、分享 | `docs/api/social.md` |
| GameKit 匹配 | 匹配队列、房间租约与 fencing、对战服务器结算 | `docs/api/match.md` |
| GameKit 商业化 | App Store IAP 校验与通知、激励广告回调、退款追踪 | `docs/api/commerce.md` |
| GameKit 实时 | WebSocket 网关（lite 单实例；scale 经 PostgreSQL 总线多实例） | `docs/api/realtime.md` |
| Web 认证（可选） | 密码登录、Cookie 会话、CSRF | `docs/api/webauth.md`、`docs/SECURITY.md` |
| 通用计费（可选） | 预付余额、预留与追加式账本（不是支付系统） | `docs/api/billing.md` |
| 对象存储 | 本地文件与 S3 兼容适配器（预签名 URL） | `docs/api/storage.md` |
| Scale profile | PostgreSQL 存储与任务队列、多实例、`migrate to-postgres`、Valkey 全局限流与缓存 | `docs/adr/0003-postgres-scale.md`、`docs/adr/0004-valkey.md` |
| 通用核心 | CLI、HTTP 中间件、任务队列、幂等、限流、可观测性、备份与恢复 | `docs/DEPLOY.md`、`docs/RUNBOOK.md`、`docs/DISASTER_RECOVERY.md` |

## 尚未验证或未实现

- 性能预算（PRD §13/§23）未在目标 1C1G 环境压测，Scale 性能同样未测量；本仓库不提供基准结论。
- 部署脚本只用桩在 macOS 上测试，未在真实 Linux/systemd 上运行；Litestream 到 R2 的复制与恢复演练未执行（见 `docs/DISASTER_RECOVERY.md`）。
- 对象存储 S3 适配器只对 MinIO 做过真实验证，AWS S3 与 Cloudflare R2 未验证（见 `docs/api/storage.md`）。
- Apple IAP、微信登录、广告回调只用本地生成的密钥或假服务测试，未对真实服务验证；不支持 Google Play、微信支付。
- Scale：未实现 PG HA 与备份/PITR、Valkey HA/TLS、实时模块 Valkey 总线；SDK 未在 Safari/微信真机验证。
- 配置校验会拒绝其余未实现的组合：`distributed` profile、pgmq 驱动、`web`/`agent` Kit。

## 快速开始

```bash
go install github.com/pillar/gostack/cmd/gostack@latest   # 或在仓库内 go build -o bin/gostack ./cmd/gostack
gostack init mygame --kit game        # go mod tidy 拉取 github.com/pillar/gostack 最新版本
# 开发框架本身时可加 --gostack-path "$PWD"，生成本地 replace
cd mygame && go mod tidy && go build -o bin/mygame .
./bin/mygame --config gostack.yaml migrate up
./bin/mygame --config gostack.yaml game create demo --origin http://localhost:5173
./bin/mygame --config gostack.yaml game config publish demo game-config.json
./bin/mygame --config gostack.yaml serve
```

客户端：见 `sdk/typescript/README.md`。

## 验证

```bash
./scripts/ci.sh
```

未实现的 profile（distributed）、驱动（pgmq 等）和 Kit（web/agent）会被配置校验拒绝，而不是假装可用；Scale profile 需要 PostgreSQL（可选 Valkey），见 ADR-0003/0004。

## License

MIT, see `LICENSE`.
