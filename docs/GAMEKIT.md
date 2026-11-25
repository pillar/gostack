# GameKit

GameKit（`kits/game`）是 GoStack 面向 H5 / 微信小游戏 / 轻量联网游戏的后端能力集合：游戏注册、玩家身份、云存档、排行榜与反作弊、钱包/道具/账本、远程配置与灰度、埋点、赛季结算，以及按需启用的运营、实验、社交、匹配、商业化和实时通信模块。

- HTTP 契约：`docs/openapi/gostack.yaml`（全部路由，`app/openapi_test.go` 保证与代码一致）；P0 说明 `docs/GAME_API.md`；模块说明 `docs/api/{ops,experiments,social,match,commerce,realtime}.md`。
- 客户端：`sdk/typescript`，集成方式见 `docs/SDK_INTEGRATION.md`。
- 安全模型：`docs/GAME_SECURITY.md`。整体架构：`docs/ARCHITECTURE.md`。实现与验证状态：`docs/STATUS.md`。

## 1. 模块

| PRD | 模块 | 包 | 启用 | 说明 |
|---|---|---|---|---|
| G01–G08 | 核心 P0 | `kits/game` | `kits: [game]` | 游戏注册、游客/微信登录与绑定、存档（条件更新、历史、恢复、合并钩子）、排行榜（best/sum、日/周赛季、游标、本人名次、对局令牌、规则版本、隔离审核、赛季结算）、钱包与道具目录、礼包购买、领奖、配置（ETag、回滚、灰度）、埋点、管理接口 |
| G09 | SDK | `sdk/typescript` | — | 核心 + browser / wechat / realtime 入口 |
| G10 | 运营 | `kits/game/ops` | `game.modules: [ops]` | 公告、活动、邮件（定向/广播、附件领取）、签到、任务、兑换码 |
| G11 | 商业化 | `kits/game/commerce` | `[commerce]` | Apple IAP 验单与服务器通知、退款与欠款、激励广告回调（AdMob SSV、HMAC） |
| G12 | 实验 | `kits/game/experiments` | `[experiments]` | 稳定分桶、互斥层、曝光记录、停止开关 |
| G13 | 社交 | `kits/game/social` | `[social]` | 资料与隐私、好友、屏蔽、邀请、分享、好友榜 |
| G14 | 实时 | `kits/game/realtime` | `[realtime]`（仅 lite） | WebSocket 网关、序号、ack、断线续传、背压 |
| G15 | 匹配与房间 | `kits/game/match` | `[match]` | 匹配票据、队列、战斗服租约与 fencing token、权威结算 |

未列入 `game.modules` 的模块不会挂载路由；拼写未知或未实现的模块名被配置校验拒绝（`TestModuleConfigValidation`）。

## 2. 配置

两层配置：

1. **服务配置**（`gostack.yaml`，`game.*`；重启生效，`auth_rate_per_minute`/`score_rate_per_minute` 可 SIGHUP 热更新）：

| 键 | 默认 | 含义 |
|---|---|---|
| `session_ttl` | 24h | 玩家会话有效期（无 refresh token） |
| `save_max_bytes` / `save_slots_per_player` / `save_history` | 64 KiB / 5 / 10 | 存档单槽上限、每玩家槽数、保留历史版本数 |
| `analytics_buffer` / `analytics_retain_days` | 10000 / 30 | 埋点内存缓冲条数、保留天数 |
| `auth_rate_per_minute` | 30 | 每 IP 登录速率（埋点为其 4 倍） |
| `score_rate_per_minute` | 60 | 每玩家成绩提交 + 开局速率 |
| `platforms.wechat.apps.<game>` | — | `app_id` 与 `app_secret_ref`（必须是 `env:`/`file:` 引用） |
| `modules` | [] | 启用的可选模块 |
| `ops` / `social` / `match` / `commerce` / `realtime` | 见各模块文档 | 模块选项 |
| `settlement.interval` | 5m | 赛季结算调度周期（0 关闭自动调度） |

2. **游戏配置文档**（管理员按游戏发布，版本化）：`client`（下发给客户端）、`boards`、`rewards`、`saves`、`items`、`offers`、`rollouts`。结构与校验规则见 `docs/GAME_API.md`「配置」。发布：`PUT /v1/admin/games/{game}/config` 或 CLI `game config publish <game> <file>`；回滚：`POST .../config/rollback` 或 `game config rollback`。回滚以旧内容发布**新** revision，已完成的账本不会被修改。

客户端读取 `GET /v1/games/{game}/config`：强 ETag + `If-None-Match` → 304；灰度命中时附带 `rollouts` 且 ETag 变化。SDK 在拉取失败时回退到缓存，再回退到内置默认值，不阻塞进入游戏。

## 3. 数据隔离模型

- **租户 → 游戏 → 玩家**。`games(game_id, tenant_id, status, allowed_origins)`；所有游戏数据表的主键/唯一约束都包含 `game_id`（如 `players(game_id, player_id)`、账本 `UNIQUE(game_id, source_key, asset)`）。
- **身份只来自凭据**：`game_id` 来自 URL 且必须与会话的 game 相同（不同则 403）；`player_id` 只来自会话；租户只来自 API Key。请求体中不存在 `player_id`/`game_id` 字段（管理接口除外，管理接口里的玩家 id 也必须属于该游戏，否则 404）。
- **管理接口**：API Key 需 `admin` scope，且 key 的租户拥有该游戏；其他租户的游戏表现为不存在（404），不泄露存在性。
- **跨玩家资源**（他人的票据、邮件、订单）一律表现为 404。
- **平台身份**：`(game, platform, openid)` 唯一；绑定冲突返回 `409 identity_conflict`，不覆盖存档或资产。平台身份由服务端调用平台接口验证（微信 code2session），不接受客户端上报的 openid。
- **埋点**写入独立库/schema，不参与经济事务。

## 4. 幂等与账本模型

**幂等键**（`modules/idempotency`）：会改变状态、客户端可能重试的接口接受 `Idempotency-Key`（1–128 个可见 ASCII）。键的作用域是「游戏 + 玩家」（或模块、battle key）+ 操作名（如 `reward.claim:<id>`），记录请求摘要并与业务变更**在同一事务**提交：

- 同键同载荷 → 重放首次成功响应（状态码与响应体相同，`Idempotent-Replayed: true`）；
- 同键不同载荷 → `409 idempotency_key_reused`；
- 失败的请求不记录，可用同键重试；记录保留 72h。

哪些接口接受键见 OpenAPI（参数 `IdempotencyKey`）；billing 的变更接口**必须**带键。首次游客注册、开局令牌、广告 intent 不支持幂等，SDK 不自动重试它们。

**账本**（`game_ledger`）：

- 余额只能通过 `game.ApplyLedger(ctx, tx, game, player, kind, sourceKey, deltas, now)` 变化：在调用方的业务事务中同时更新余额与追加账本行；`balance_after >= 0`（CHECK），扣减致负返回 `409 insufficient_funds`。
- 来源键唯一：`(game_id, source_key, asset)`。同一业务事件（领奖、购买、邮件附件、签到、任务、兑换码、IAP 交易、广告回调、赛季结算、房间结算、管理调整）有确定性的来源键（如 `season:<board>:<season>:<player>`、`iap:<provider>:<txn>`、`mail:<mail>:<player>`），重复执行、并发或崩溃恢复都不会重复入账。
- 账本只追加：UPDATE/DELETE 由触发器拒绝（PG 还拒绝 TRUNCATE）。
- 对账：`gostack game reconcile` 比较余额与账本汇总，0 差异为通过。
- 付费资产（余额、道具、IAP 权益）永远不从存档读取或恢复；存档只保存进度。

**外部事件**（IAP 通知、广告回调）：先在事务外验证签名，再在一个事务里写 inbox（`(game, provider, event_id)` 去重）、订单状态机与账本；需要通知外部系统时在同一事务入队 outbox 任务（`modules/jobs`，至少一次，处理器须幂等）。事务内不做网络 IO。

## 5. 运维与管理流程

| 场景 | 操作 |
|---|---|
| 新建游戏 | `gostack game create <id> --origin https://...`（或 `POST /v1/admin/games`）；`allowed_origins` 决定浏览器 CORS 白名单 |
| 管理凭据 | `gostack apikey create --name N --scope admin`（或 `battle`、`billing`，`--tenant`、`--ttl`）；明文只显示一次；`apikey revoke <key_id>` 立即生效 |
| 发布/回滚配置 | `game config publish` / `game config rollback`，或管理 API |
| 灰度停止 | 把 rollout 的 `enabled` 设为 false 重新发布，或回滚 |
| 补偿玩家 | `POST /v1/admin/games/{game}/players/{player}/adjust`（`reason` 必填，`source_key` 去重，审计表 `wallet_adjustments`） |
| 可疑成绩 | `GET .../quarantine` 列表 → `approve` / `reject`（审核人与时间记录） |
| 赛季结算 | 自动（`settlement.interval` 调度 `game.settle_season` 任务）或 `POST .../boards/{board}/seasons/{season}/settle`；可重入，不重复发奖 |
| 运营内容 | ops 管理接口（公告/活动/邮件/签到/任务/兑换码），见 `docs/api/ops.md` |
| IAP 欠款 | `GET .../orders?state=refund_shortfall` → `POST .../orders/{order}/resolve` |
| 实验停止 | `POST .../experiments/{id}/stop`（立即生效、幂等） |
| 任务队列 | `gostack jobs replay <job_id>` 把死信任务移回待执行 |
| 对账 | `gostack game reconcile` |
| 备份/恢复（lite） | `docs/DISASTER_RECOVERY.md`、`docs/RUNBOOK.md` |

## 6. 扩展

应用可在生成项目中通过 `Hooks.Routes` 增加路由，或写自己的 GameKit 功能包：使用 `Kit.Handle(mux, "POST /path", game.Player, h)` / `Kit.HandleAdmin`，在处理器中只用 `game.CurrentGame(ctx)` / `game.CurrentSession(ctx)` 获取身份，用 `ApplyLedger` 改余额、`idempotency.Do` 处理重试、`DB.WriteTx` 写入（fn 可重跑、不做网络 IO）。示例与测试：`kits/game/gametest`（`TestExtensionRoutes`）。新增路由必须同时写入 `docs/openapi/gostack.yaml`，否则 `TestOpenAPICoversRegisteredRoutes` 失败（仅扫描框架包；应用自己的路由不在检查范围）。

## 7. 限制

- 纯客户端游戏无法彻底防作弊：分数范围、隔离阈值、对局令牌与最短时长、规则版本、限流只能提高作弊成本；高价值奖励应依赖权威服务端结果（match 模块的战斗服结算）或人工审核。
- 实时模块仅单实例语义；scale profile 下被拒绝。
- 未在真实微信 / Apple / AdMob 环境验证；性能预算（PRD §23）未在目标环境压测。
