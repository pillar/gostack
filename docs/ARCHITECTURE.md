# GoStack 架构

本文描述代码中已经存在的结构（2026-11-25，分支 `dev/full-scope`）。未实现的能力在文末单列；实现与验证状态以 `docs/STATUS.md` 和各测试为准。HTTP 契约：`docs/openapi/gostack.yaml`（机器可读，`app/openapi_test.go` 保证与路由一致）与 `docs/GAME_API.md`、`docs/api/*.md`（说明性文档）。

## 1. 分层

```
cmd/gostack, cmd/example, 生成项目 main.go        进程入口（CLI 子命令 + serve）
        │
app/                                             装配：配置 → 存储 → Kit/模块 → HTTP Handler → 后台循环
        │
kits/game (+ commerce/experiments/match/ops/      业务能力（GameKit 与可选模块）
           realtime/social)
modules/{webauth,billing,storage,jobs,            可复用业务模块与基础设施模块
         idempotency,ratelimit}
        │
core/{config,httpx,auth,observability}            横切：配置与校验、HTTP 中间件与错误信封、API Key/哈希、日志与指标
        │
adapters/store  ← adapters/sqlite | adapters/postgres   方言无关的 *store.DB；两个打开器
migrations/{sqlite,analytics,postgres,postgres_analytics}  内嵌、只前进的迁移
```

依赖方向自上而下；`kits/*` 与 `modules/*` 不依赖 `app`。功能包（`kits/game/<module>`）只通过 `kits/game/ext.go` 的扩展 API（`Kit.Handle`、`Kit.HandleAdmin`、`CurrentGame`、`CurrentSession`、`ApplyLedger` 等）接入 GameKit，从而继承游戏解析、CORS、会话认证和管理租户检查。

## 2. 包一览

| 包 | 职责 |
|---|---|
| `adapters/store` | `*store.DB{Write, Read, Dialect}`：`WriteTx`（有界准入，满时 `ErrOverloaded` → 503；fn 在 SQLITE_BUSY / 40001 / 40P01 时整体重跑）、`ReadTx`、`IsUniqueViolation`、迁移执行器（校验和漂移拒绝、未知版本检测） |
| `adapters/sqlite` | Lite：单写连接 + 只读池、每连接 PRAGMA、`BEGIN IMMEDIATE`、实例文件锁（ADR-0001/0002） |
| `adapters/postgres` | Scale：pgx stdlib、写事务 SERIALIZABLE + 有界重试、只读池、`?`→`$n` 改写、C 排序校验（ADR-0003） |
| `core/config` | 默认值 < 文件 < 环境变量 < CLI；未知键拒绝；能力矩阵校验（未实现的 profile/驱动/模块被拒绝）；密钥只能是 `env:`/`file:` 引用；热更新 `Diff`/`WithHot` |
| `core/httpx` | 请求 ID、错误信封、body/超时上限、panic 恢复、可信代理 IP、`BearerToken` |
| `core/auth` | API Key（`gsk_`，SHA-256 摘要、scope、过期、撤销即时生效）、`Hash`、随机秘密 |
| `core/observability` | JSON 日志与敏感字段脱敏、访问日志（记录路由模式而非原始路径）、Prometheus 文本指标 |
| `modules/jobs` | 持久化任务队列（同一张 `jobs` 表，两种方言）：租约 token、重试退避、死信、重放、dedupe、同事务入队 |
| `modules/idempotency` | `idempotency.Do`：幂等键 + 请求摘要，与业务写在同一事务；保留 72h |
| `modules/ratelimit` | 进程内令牌桶，表大小有界；IPv6 按 /64 聚合；可热更新速率 |
| `modules/webauth` | 邮箱密码（Argon2id）+ Cookie 会话 + CSRF（`auth.web.enabled`） |
| `modules/billing` | 租户预付费账户、只追加账本、预留状态机（`billing.enabled`） |
| `modules/storage` | 对象存储端口：本地文件系统（带预签名 HTTP 处理器）与 S3 兼容（自实现 SigV4） |
| `kits/game` | GameKit P0：游戏注册、身份、存档、排行榜与反作弊、钱包/道具/账本、配置与灰度、埋点、赛季结算、管理接口 |
| `kits/game/{ops,experiments,social,match,commerce,realtime}` | P1/P2 可选模块，由 `game.modules` 启用 |
| `app` | 装配、`Handler`、`Serve`（优雅停机）、维护循环、热更新、CLI 子命令（migrate/backup/doctor/game/apikey/jobs/to-postgres） |
| `templates/app` | `gostack init` 生成的项目骨架（内嵌） |
| `sdk/typescript` | 引擎无关的 TS 客户端 SDK（核心 + browser/wechat/realtime 入口） |

## 3. 请求数据流

```
TCP → http.Server (ReadHeaderTimeout 5s, MaxHeaderBytes)
  → observability.CaptureRoute(mux)        记录匹配到的路由模式
  → httpx.WithRequestID                    服务端生成 req_...（不信任客户端值）
  → observability.AccessLog                日志 + 指标
  → httpx.WithRecover                      panic → 500 internal
  → App.concurrencyGate                    超过 http.max_concurrent → 503 overloaded（可热更新）
  → httpx.WithLimits                       body 上限、请求超时
  → ServeMux 路由（未匹配 → 404 not_found "route not found"）
      /v1/games/{game}/...       Kit.cors → withGame（未知 404 / 停用 403）→ authed|optionalAuth → 处理器
      /v1/admin/games/...        Kit.admin（API Key + admin scope）→ adminGame（租户拥有该游戏，否则 404）
      /v1/match/rooms/...        API Key + battle scope，按 key 的租户过滤房间
      /v1/billing/...            API Key + billing scope，租户来自 key；16 KiB body
      /v1/auth/...               webauth：Cookie 会话、CSRF、来源检查
      /v1/files/...              本地存储预签名 GET/PUT
  → 处理器：读用 DB.Read（只读池）；写用 DB.WriteTx（单写者 / SERIALIZABLE）
      写事务内：幂等记录 + 业务变更 + 账本 + outbox 任务，一次提交；事务内不做网络 IO
```

外部验证（平台 code2session、Apple JWS 证书链、AdMob 公钥拉取）都在事务**之前**完成，事务只应用结果（“外部验证、内部应用”）。

## 4. 进程与后台工作

`App.Serve` 启动：HTTP 服务器、可选内网监听（`http.internal_listen`：`/metrics`、`/debug/pprof/*`）、jobs Worker、维护循环（每 10 分钟：过期会话、幂等记录、已成功任务、webauth 会话、各模块 `Maintain`、对局令牌、埋点保留期）、埋点刷新、赛季结算调度（`game.settlement.interval`）、撮合循环（match 模块）。

停机顺序：停止接受 → 排空在途请求（`http.shutdown_timeout`，超时强制关闭）→ 关闭 WebSocket（1001）→ 停止后台工作 → 刷新埋点缓冲 → 关闭数据库（`TestServeWithModulesShutsDownRealtime`、`tests/fault`）。

配置热更新（SIGHUP）：只应用热键（日志级别、并发上限、各限流速率），重启键变化只报告不应用（`app/reload_test.go`）。

## 5. 数据

- 主库：业务表（游戏、玩家、会话、存档、榜单、账本、模块表、任务队列、幂等记录、API Key、webauth、billing）。
- 埋点库：与主库隔离（SQLite 独立文件 `analytics.db`，PG 独立 schema `gostack_analytics`），过载丢弃并计数，不与经济事务争抢写锁（`TestAnalyticsOverloadDoesNotBlockEconomy`）。
- 账本只追加：`game_ledger`、`billing_ledger_entries` 的 UPDATE/DELETE（PG 还有 TRUNCATE）被触发器拒绝（`TestLedgerAppendOnly`、`TestLedgersAppendOnly`）。
- 迁移：`migrations/sqlite` 与 `migrations/postgres` 同版本同名，一一对应；已应用迁移不可修改（校验和漂移拒绝启动）。

## 6. Profile

| | lite（默认） | scale |
|---|---|---|
| 数据库 | SQLite WAL，单写连接，实例锁（同一文件只能一个进程） | PostgreSQL（`database.dsn_ref`），SERIALIZABLE + 有界重试，可多实例 |
| 队列 | `jobs` 表（SQLite） | `jobs` 表（PostgreSQL，`FOR UPDATE SKIP LOCKED`） |
| 备份 | Litestream 配置与 `backup snapshot/verify/restore` | `backup snapshot/verify/restore`（pg_dump/pg_restore + 清单）；PITR 用 WAL 归档 + `pg_basebackup`（`deploy/postgres`，本机演练，见 `docs/DISASTER_RECOVERY.md`） |
| 限流 | 进程内 | 仍是进程内，须显式 `rate_limit.accept_per_instance: true` 承认配额乘以实例数 |
| realtime 模块 | 支持（进程内 hub） | 配置拒绝 |
| `storage.driver: local` | 支持 | 配置拒绝（用 s3） |
| 迁移工具 | `gostack migrate to-postgres` 停机迁移到 scale | — |

`distributed` profile、Valkey、pgmq 未实现，配置校验拒绝（`TestRejectsUnknownKeysAndUnimplemented`、`TestScaleProfile`）。

## 7. 模块启用方式

| 能力 | 配置 | 挂载的路由前缀 |
|---|---|---|
| GameKit P0 | `kits: [game]` | `/v1/games/{game}/...`、`/v1/admin/games...` |
| ops / experiments / social / match / commerce / realtime | `game.modules: [...]` | 同上（match 另有 `/v1/match/rooms/...`） |
| webauth | `auth.web.enabled: true` | `/v1/auth/...` |
| billing | `billing.enabled: true` | `/v1/billing/...` |
| 本地对象存储 | `storage.driver: local` | `storage.local.public_url` 的路径（如 `/v1/files/`） |
| 系统 | 总是 | `/healthz`、`/readyz` |

每个操作在 OpenAPI 中都带 `x-gostack-module` 标注，未启用模块的路由不会挂载（返回 404，`TestModulesWiredByConfig`）。

## 8. 未实现 / 未验证（不要据此宣传能力）

- 未实现：distributed profile、Valkey 缓存与全局限流、pgmq、PostgreSQL HA/故障切换、PG 备份自动化、实时模块多实例广播、在线双写迁移、组提交写调度器（ADR-0002）、Google Play / 微信支付等其他 IAP、订阅、邮箱验证/找回密码/MFA、管理后台 UI。
- 未验证：PRD §13/§23 性能预算（未在 1C1G 目标环境压测）、断电模型下的持久性、Safari iPhone 与微信小游戏真机（G-A08）、真实微信 / Apple / AdMob 端点、Linux 实机部署权限。
- 引擎集成示例（Cocos/Laya/Three.js，见 `docs/SDK_INTEGRATION.md`）未经测试。
