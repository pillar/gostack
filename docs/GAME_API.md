# GameKit HTTP API（v1 首期契约）

状态：首期实现契约。变更须同步更新本文件、`kits/game` 测试与 `sdk/typescript`。

## 通用约定

- 基础路径：`/v1/games/{game}`，`{game}` 为公开 game_id（`[a-z0-9][a-z0-9_-]{0,31}`）。
- 认证：`Authorization: Bearer <access_token>`。token 不放 URL。
- 请求/响应体均为 JSON（`Content-Type: application/json`），默认请求体上限 1 MiB（存档、埋点另有更小上限）；各接口上限见对应小节，超限返回 `413 payload_too_large`。
- 每个响应带 `X-Request-ID` 头；错误响应体：

```json
{ "error": { "code": "revision_conflict", "message": "...", "details": {"current_revision": 3} }, "request_id": "req_..." }
```

- 时间：UTC Unix 毫秒整数。金额与数量：整数。
- 幂等：会改变状态的接口支持 `Idempotency-Key` 头（1–128 个可见 ASCII 字符）。同一玩家、同一操作、同一键：载荷相同则重放首次成功响应；载荷不同返回 `409 idempotency_key_reused`。失败请求不记录，可用同键重试。
- 限流：`429 rate_limited`，带 `Retry-After`（整数秒）。过载：`503 overloaded`，带 `Retry-After`。

### 错误码

| HTTP | code | 含义 |
|---|---|---|
| 400 | invalid_request | 参数/载荷不合法 |
| 401 | unauthorized | 缺少或无效凭据、会话过期或撤销 |
| 403 | forbidden | 无权访问（游戏停用、跨游戏 token 等） |
| 404 | not_found | 资源不存在 |
| 409 | revision_conflict | 存档版本冲突，`details.current_revision` |
| 409 | idempotency_key_reused | 同键不同载荷 |
| 409 | already_claimed | 奖励已达领取上限 |
| 409 | identity_conflict | 平台身份已绑定其他玩家 |
| 409 | slot_limit | 已达每玩家存档槽上限 |
| 403 | quota_exceeded | 游戏配额已满（`max_players` 新建玩家、`save_bytes_total` 存档增长），`details.quota/limit/used/requested`，见 `docs/api/quotas.md` |
| 409 | insufficient_funds | 扣减会导致余额为负（购买、管理调整） |
| 409 | schema_outdated | 存档 `schema_version` 低于配置 `saves.min_schema_version`，`details.min_schema_version` |
| 409 | merge_base_unavailable | 合并所需的基准版本已不在历史中，需重新拉取 |
| 409 | stack_limit | 道具超过目录 `max_stack`，`details.asset/max_stack` |
| 409 | purchase_limit | 礼包已达购买次数上限 |
| 409 | rule_version_mismatch | 提交的 `rule_version` 与排行榜规则不一致，`details.rule_version` |
| 409 | run_token_invalid / run_token_used / run_token_expired | 对局令牌不属于该玩家与榜单 / 已使用 / 已过期 |
| 409 | source_key_reused | 管理调整的 `source_key` 已用于不同内容 |
| 409 | already_reviewed / season_settled / board_removed | 隔离审核：已作出相反决定 / 赛季已结算 / 榜单已删除 |
| 409 | settlement_not_configured / season_not_ended | 赛季结算：榜单无结算配置 / 赛季未结束（`details.settle_after`） |
| 409 | conflict | 资源已存在（管理接口） |
| 413 | payload_too_large | 载荷超限 |
| 429 | rate_limited | 限流 |
| 501 | not_implemented | 能力未启用/未实现 |
| 503 | overloaded | 写入队列满或服务未就绪 |
| 500 | internal | 内部错误 |

## 认证

会话：`access_token` 为不透明随机值，默认 24h，无 refresh token。到期后游客用保存的 `guest_id/guest_secret` 重新调用 `/auth/guest` 换新会话；平台登录到期需重新获取平台 code。已保存的游客凭据被拒绝（401）时客户端不得自动新建游客，以免丢失原账号。

### POST /auth/guest

首次注册请求不支持幂等键，客户端不应自动重试（超时重试可能产生第二个游客）；带凭据的再次登录可安全重试。

首次注册：空 body `{}`，返回 `201`（新玩家与一次性展示的 `guest_secret`，客户端须持久保存）。

再次登录：`{"guest_id": "gst_...", "guest_secret": "..."}`，返回 `200`、同一玩家与新会话；`"rotate": true` 时返回新的 `guest_secret`，旧值立即失效。

```json
{ "player_id": "plr_...", "guest_id": "gst_...", "guest_secret": "... 或 null",
  "access_token": "...", "expires_at": 1764000000000, "created": true }
```

### POST /auth/platform

`{"platform": "wechat", "code": "..."}`。服务端调用平台接口验证（微信 code2session），按 (game, platform, openid) 找到或创建玩家，首次创建玩家返回 `201`（`created: true`），已有身份再次登录返回 `200`；结构同上（无 guest 字段）。未配置平台时返回 `501 not_implemented`。

### POST /auth/bind（需认证）

`{"platform": "wechat", "code": "..."}`，把平台身份绑定到当前玩家。身份已属于其他玩家时 `409 identity_conflict`，不覆盖任何数据。返回 `{"player_id": "...", "platform": "wechat"}`。该接口不做 IP 限流（`/auth/guest`、`/auth/platform` 按 IP 限流）。

### POST /auth/logout（需认证）

撤销当前会话，返回 `204`。

### GET /me（需认证）

`{"player_id": "...", "game_id": "...", "created_at": 0, "identities": ["guest", "wechat"]}`

## 配置

### GET /config?client_version=1.2.0&platform=wechat&bucket_key=...

返回该游戏当前发布配置中 `client` 段：`{"revision": 3, "data": {...}}`，带 `ETag: "3"`（带引号的强 ETag）；请求带 `If-None-Match` 且未变化时返回 `304`。未发布任何配置时 `{"revision": 0, "data": {}}`。不需要认证。

灰度（`rollouts`）：按文档顺序把命中的 rollout 的 `client` 浅合并（顶层键覆盖）到 `client` 上，后者覆盖前者。命中条件：`enabled` 为 true；`platforms` 非空时须包含请求的 `platform`；设置了 `min_client_version`/`max_client_version` 时请求须带 `client_version` 且满足 `min ≤ v ≤ max`（两端包含）；`percent < 100` 时须有分桶键且 `sha256(game_id, rollout id, key) % 100 < percent`。分桶键：带有效 token 时为 `player_id`（此时忽略 `bucket_key`）；否则为 `bucket_key` 查询参数（≤128 字符，建议用安装 ID）；都没有时只命中 `percent = 100` 的 rollout。无效或过期 token 在此接口被忽略（不返回 401）；这与 `GET /boards/{board}` 不同（后者对无效 token 返回 401）。
- 有命中时响应多一个 `"rollouts": ["r1"]`（命中的 id），ETag 变为 `"<revision>-<命中 id 集合的哈希>"`；命中集合变化或发布新版本都会改变 ETag，304 行为不变。无命中时响应与 ETag 与旧契约完全相同。
- `client_version` 为 1–4 段点分非负整数（如 `1`、`1.2`、`1.2.3`），缺省段按 0 比较；格式错误返回 400。
- 停止开关：把该 rollout 的 `enabled` 改为 false（或删除）并重新发布配置，下一次拉取即生效；也可用 rollback 回到旧 revision。

配置文档（管理员发布）结构（`client`、`boards`、`rewards` 及其余各段均可选；缺省的 `client` 视为 `{}`）：

```json
{
  "client": { "任意客户端可见键": "值" },
  "boards": { "main": { "order": "desc", "mode": "best", "period": "none|daily|weekly", "min_score": 0, "max_score": 1000000, "quarantine_above": 500000,
                        "rule_version": 3, "require_run": true, "min_run_ms": 30000, "run_ttl_ms": 3600000,
                        "settlement": { "rewards": [ {"from_rank": 1, "to_rank": 1, "grants": {"coin": 500}}, {"from_rank": 2, "to_rank": 10, "grants": {"coin": 100}} ] } } },
  "rewards": { "daily_login": { "grants": {"coin": 100}, "period": "once|daily", "per_period": 1, "starts_at": 0, "ends_at": 0 } },
  "saves": { "min_schema_version": 2 },
  "items": { "coin": {"kind": "currency", "name": "金币"}, "sword": {"kind": "item", "max_stack": 1} },
  "offers": { "potion_pack": { "cost": {"coin": 50}, "grants": {"potion": 3}, "period": "once|daily|unlimited", "per_period": 2 } },
  "rollouts": [ { "id": "r1", "enabled": true, "percent": 10, "platforms": ["wechat"], "min_client_version": "1.2.0", "max_client_version": "", "client": {"new_shop": true} } ]
}
```

校验规则（发布时，违反返回 400）：
- `items`：`kind` 为 `currency` 或 `item`；`max_stack ≥ 0`（0 = 不限，仅 item 可设）。存在目录时，奖励、礼包成本/产出、赛季结算奖励引用的资产必须在目录中。
- `settlement`：仅 `daily`/`weekly` 榜可配置；区间 `1 ≤ from_rank ≤ to_rank ≤ 10000`，按升序且不重叠；`grants` 为正整数。
- `offers`：`cost`、`grants` 非空正整数且资产不重叠；`once`/`daily` 需 `per_period` 1–1000，`unlimited` 不得设置 `per_period`。
- `rule_version ≥ 0`（0 = 不校验）；`min_run_ms`/`run_ttl_ms` 仅在 `require_run` 时允许，`run_ttl_ms` 默认 1 小时、最大 7 天，`min_run_ms < run_ttl_ms`。
- `rollouts`：`id` 唯一，`percent` 0–100，版本号格式同上且 `min ≤ max`，`client` 非空。

## 存档（需认证）

槽位名 `[a-z0-9_-]{1,32}`；默认单槽 64 KiB、每玩家 5 槽。

### GET /saves/{slot}

`{"slot": "main", "revision": 3, "schema_version": 1, "checksum": "sha256hex", "payload": <JSON>, "updated_at": 0}`；无存档时 `404 not_found`。

### PUT /saves/{slot}

```json
{ "expected_revision": 3, "schema_version": 1, "payload": <JSON>, "checksum": "可选，sha256hex(payload 原始字节)" }
```

`checksum` 若提供，必须是请求 JSON 中 `payload` 值原始字节（不重新序列化）的 SHA-256 十六进制；服务端返回的 checksum 按同一规则计算。`expected_revision` 为 0 表示首次创建。成功返回新存档的元数据（revision+1、schema_version、checksum、updated_at）；**响应不回传 payload**（存储的就是请求中 payload 的原始字节，checksum 覆盖它），只有服务端合并时（`merged: true`）才带合并后的 `payload`。这样幂等记录不随存档体积增长（本机基准中，回传 payload 时 72 h 幂等保留使数据库从 1.6 GB 涨到 6.8 GB）。版本不匹配返回 `409 revision_conflict` 与 `details.current_revision`。支持幂等键。

- 兼容性：配置 `saves.min_schema_version` 存在且 `schema_version` 低于它时返回 `409 schema_outdated`（`details.min_schema_version`），客户端需升级后迁移存档。
- 合并（可选）：请求带 `"merge": true`，且应用在服务端设置了 `Kit.SaveMerge` 时，版本冲突不返回 409，而是以 `base`（客户端 `expected_revision` 对应的历史版本 payload；`expected_revision` 为 0 时为空）、`server`（当前 payload）、`client`（本次 payload）调用合并函数，并把结果提交为下一 revision，响应带 `"merged": true`，`checksum` 为合并结果的哈希。基准版本已不在历史中时返回 `409 merge_base_unavailable`；未设置合并函数或存档不存在时仍返回 `409 revision_conflict`。合并函数在写事务内执行，必须快速、确定、可重入且不做网络 IO。
- 付费资产（钱包余额、道具、IAP 权益）只来自服务端账本，永远不从存档读取或恢复；存档只保存游戏进度。

### GET /saves/{slot}/revisions?limit=20

保留的历史版本元数据，最新在前，`limit` 1–100（默认 20）。保留数量由服务配置 `game.save_history` 决定（默认 10，0 表示不保留）。

```json
{ "slot": "main", "current_revision": 4, "revisions": [ {"revision": 4, "schema_version": 1, "checksum": "...", "size": 123, "created_at": 0} ] }
```

槽位不存在且无历史时 `404 not_found`。

### POST /saves/{slot}/restore（支持幂等键）

请求体 ≤1 KiB。

`{"revision": 2, "expected_revision": 4}`：当前版本须等于 `expected_revision`（否则 `409 revision_conflict`），把历史版本 2 的 payload/schema_version/checksum 作为新 revision（5）提交，返回同 PUT 的存档结构。历史版本不存在 `404 not_found`；其 schema 低于 `min_schema_version` 时 `409 schema_outdated`。

### DELETE /saves/{slot}?expected_revision=4（支持幂等键）

条件删除：当前版本须等于 `expected_revision`（否则 `409 revision_conflict`，不存在 `404`）。删除槽位**及其全部历史版本**（删除即删除，且避免重建后 revision 从 1 重新计数时与旧历史冲突）；返回 `{"slot": "main", "deleted": true, "revision": 4}`。之后用 `expected_revision: 0` 重新创建。

## 排行榜

### GET /boards/{board}?limit=50&cursor=...&season=...

不需要认证；带有效 token 时附带 `me`，带无效/过期 token 返回 `401`（客户端应去掉 token 重试）；玩家在该榜/赛季没有已接受成绩时省略 `me`。

```json
{ "board": "main", "season": "2026-11-24", "entries": [ {"rank": 1, "player_id": "plr_...", "score": 900, "achieved_at": 0} ],
  "next_cursor": "opaque 或 null", "me": {"rank": 12, "score": 300} }
```

排序：分数（按 order）→ 达成时间早者优先 → player_id。`limit` 1–100，默认 50。

### POST /boards/{board}/scores（需认证，支持幂等键）

请求体 ≤4 KiB。`{"score": 900, "rule_version": 3, "run_token": "run_..."}` → `{"status": "accepted|quarantined", "best": 900, "season": "..."}`。`rule_version`、`run_token` 可选，仅在榜单配置要求时必须。超出 `[min_score,max_score]` 返回 400；超过 `quarantine_above` 的成绩进入隔离，不上榜，等待人工审核。

- 规则版本：榜单 `rule_version > 0` 时，提交的 `rule_version` 必须相等，否则 `409 rule_version_mismatch`（`details.rule_version` 为当前版本）。
- 对局令牌：榜单 `require_run: true` 时必须携带 `run_token`（缺失 400）。令牌一次性：已用 `409 run_token_used`，过期 `409 run_token_expired`，不属于本玩家/本榜/本游戏 `409 run_token_invalid`。从领取令牌到提交的时长小于 `min_run_ms` 时成绩进入隔离（原因 `run shorter than min_run_ms`）。未要求令牌的榜单若携带令牌也会校验并消耗。带同一 `Idempotency-Key` 的重试返回首次结果，不会报令牌已用。
- 赛季：`daily`/`weekly` 以 UTC 自然日 / ISO 周划分，赛季 id 为 `2026-11-24` / `2026-W48`；`none` 榜赛季为 `all`。

### POST /boards/{board}/runs（需认证）

开始一局，返回 `201 {"run_token": "run_...", "board": "main", "started_at": 0, "expires_at": 0}`。服务端只保存令牌的 SHA-256；有效期为榜单 `run_ttl_ms`（默认 1 小时）。与成绩提交共用限流。每次调用都会签发新令牌（不支持幂等键）。

### 赛季结算

榜单配置 `settlement` 后，`daily`/`weekly` 赛季结束（结束时刻 + 1 分钟宽限）后可结算：按与 `GET /boards` 相同的稳定排序（分数 → 达成时间 → player_id）取已接受成绩的前 `max(to_rank)` 名，名次为位置序号（同分不并列），按区间发放 `grants`。隔离中的成绩不参与。每个 (game, board, season) 只有一条结算记录（记录发起时的配置 revision，后续恢复按该 revision 发放）；每位玩家一条发放记录，账本来源键 `season:<board>:<season>:<player>`，因此并发、重复执行或崩溃后恢复都不会重复发奖。道具超过 `max_stack` 时结算按上限截断（记录实际发放），不报错。结算开始后该赛季的隔离成绩不能再批准（`409 season_settled`）。自动结算：应用周期调用 `Kit.ScheduleSettlements(ctx, queue)`，为刚结束的上一赛季入队 `game.settle_season` 任务（dedupe 键 `<game>:<board>:<season>`，`run_at` = 赛季结束 + 宽限），并把 `Kit.JobHandlers()` 注册到 jobs Worker；管理员也可手动触发（见管理接口）。

## 经济（需认证）

### GET /wallet

`{"balances": {"coin": 100, "gem": 0}}`（资产代码 → 整数余额，道具同样以资产代码表示）。

### POST /rewards/{reward}/claim（需认证，支持幂等键）

请求体为 `{}`（幂等摘要包含请求体，重试须发送相同内容）。

服务端按当前配置判定资格。成功：`{"claim_id": "rcl_...", "grants": {"coin": 100}, "balances": {...}, "config_revision": 3}`。超出领取上限：`409 already_claimed`；不在时间窗或未定义：`404 not_found`/`403 forbidden`；领取后道具超过 `max_stack`：`409 stack_limit`（整笔回滚）。

### GET /inventory

按配置 `items` 目录拆分余额：

```json
{ "currencies": [ {"id": "coin", "name": "金币", "balance": 100} ],
  "items": [ {"id": "sword", "balance": 1, "max_stack": 1} ],
  "uncataloged": { "legacy_asset": 5 } }
```

目录中的货币总是列出（余额可为 0）；道具只列出余额 > 0 的；不在目录中的非零资产放在 `uncataloged`。各列表按 id 排序。`GET /wallet` 不变。

### POST /offers/{offer}/purchase（需认证，支持幂等键）

请求体为 `{}`。在同一事务中扣除 `cost`、发放 `grants`（一笔账本来源 `offer:<purchase_id>`，kind `purchase`）并记录购买次数。成功：`{"purchase_id": "pur_...", "cost": {...}, "grants": {...}, "balances": {...}, "config_revision": 3}`。余额不足 `409 insufficient_funds`；次数上限（`once` 终身、`daily` 按 UTC 日、`unlimited` 不限）`409 purchase_limit`；道具超过 `max_stack` `409 stack_limit`；未定义 `404`。失败时不扣费。

## 埋点

### POST /events/batch（可匿名；带 token 时关联玩家）

```json
{ "events": [ { "event_id": "唯一", "event_name": "level_end", "session_id": "...", "client_time": 0,
  "client_version": "1.0.0", "platform": "web|wechat", "schema_version": 1, "properties": {} } ] }
```

单批 ≤100 条、请求体 ≤64 KiB、单事件 properties ≤4 KiB。`event_id`/`event_name`/`session_id` 为 1–64 个可见 ASCII 字符；`client_version`（≤32 字节）与 `platform`（≤16 字节）须为合法 UTF-8 且不含控制字符（U+0000–U+001F、U+007F–U+009F）；`properties` 须为 JSON 对象且为合法 UTF-8（字符串内的 `\u0000` 转义按原文保存）。任一事件不合规则整批返回 `400 invalid_request`（消息指明事件下标），不接收其中任何事件。返回 `202 {"accepted": n, "dropped": m}`。服务端缓冲满时丢弃并计数（可丢失语义）；写库时若某条事件被数据库拒绝，只丢弃该条并计数，同批其他事件照常写入，`event_id` 去重，因此不使用 `Idempotency-Key`，重发安全。携带无效/过期 token 返回 401，客户端可去掉 token 匿名重发。

## 管理接口（API Key，`Authorization: Bearer gsk_...`，需 `admin` 权限）

- `POST /v1/admin/games` `{"game_id": "demo", "name": "Demo", "allowed_origins": ["https://example.com"]}` → `201 {"game_id": "demo"}`；`game_id` 已存在（game_id 全局唯一，包括其他租户创建的）返回 `409 conflict`
- `PUT /v1/admin/games/{game}/config` body 为配置文档（≤1 MiB）→ `{"revision": n}`
- `POST /v1/admin/games/{game}/config/rollback` `{"revision": n}` → 以旧内容发布新 revision

以下接口要求 API Key 的租户拥有该游戏（否则 404）。玩家 token 调用返回 401。

### POST /v1/admin/games/{game}/players/{player}/adjust

`{"deltas": {"coin": 500, "gem": -3}, "reason": "工单 42 补偿", "source_key": "ticket-42"}`：运营调整余额（账本 kind `adjust`，来源 `adjust:<source_key>`）。`reason` 1–500 字符必填；`source_key` 1–128 个可见 ASCII 字符，在游戏内唯一：同键同玩家同 deltas 重复调用返回 `"replayed": true` 且不重复入账，内容不同返回 `409 source_key_reused`。请求体 ≤16 KiB。原因与操作的 API Key id 记录在审计表 `wallet_adjustments`。扣减致负 `409 insufficient_funds`；超过 `max_stack` `409 stack_limit`；存在目录时资产须在目录中（400）；玩家不属于该游戏 `404`。返回 `{"source_key": "...", "replayed": false, "balances": {...}}`。

### GET /v1/admin/games/{game}/quarantine?status=pending&limit=50&after=0

按 id 升序分页列出隔离成绩，`status` 为 `pending`（默认）/`approved`/`rejected`，`limit` 1–200，`after` 为上页 `next_after`：

```json
{ "items": [ {"id": 7, "board": "main", "season": "all", "player_id": "plr_...", "score": 600000, "reason": "above quarantine threshold",
               "created_at": 0, "review": "pending", "reviewed_by": null, "reviewed_at": null} ], "next_after": 7 }
```

### POST /v1/admin/games/{game}/quarantine/{id}/approve | /reject

批准：把成绩按当前榜单规则（best 取更优、sum 累加）写入其原赛季，达成时间为原提交时间；拒绝：仅标记。记录审核人 API Key id 与时间。重复同一决定是幂等的（`"replayed": true`，不重复计分）；与已有决定相反 `409 already_reviewed`；赛季已结算 `409 season_settled`；榜单已从配置删除 `409 board_removed`。返回 `{"entry": {...同上...}, "best": 200 或 null, "replayed": false}`。

### POST /v1/admin/games/{game}/boards/{board}/seasons/{season}/settle

执行（或继续未完成的）赛季结算，返回结算记录：

```json
{ "game_id": "demo", "board": "main", "season": "2026-11-24", "status": "completed", "config_revision": 3, "paid_players": 10, "created_at": 0, "completed_at": 0 }
```

已完成时直接返回记录（不重复发奖）。赛季格式不符 400；榜单不存在 404；无结算配置 `409 settlement_not_configured`；赛季未结束 `409 season_not_ended`。

### GET /v1/admin/games/{game}/boards/{board}/seasons/{season}/settlement?limit=100

结算记录加发放明细 `"payouts": [{"rank": 1, "player_id": "...", "score": 900, "grants": {"coin": 500}}]`（按名次，`limit` 1–1000）；无记录 404。

### GET | PUT /v1/admin/games/{game}/quotas

每游戏资源配额（覆盖值整体替换，`null`/缺省继承 `game.quota_defaults`，0 = 不限）与用量，详见 `docs/api/quotas.md`。

### Items catalog scope

The `items` catalog and `max_stack` apply to reward claims, offer purchases and
all player-claimed grants of the optional modules (mail, check-in, tasks,
redeem codes, invites → `409 unknown_asset` / `409 stack_limit`). Season
settlement clamps item grants to `max_stack` instead of failing. Purchases
(IAP), ad rewards and match settlements are server-authoritative and are not
capped, so a confirmed external event is never stranded.
