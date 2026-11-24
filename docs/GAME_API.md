# GameKit HTTP API（v1 首期契约）

状态：首期实现契约。变更须同步更新本文件、`kits/game` 测试与 `sdk/typescript`。

## 通用约定

- 基础路径：`/v1/games/{game}`，`{game}` 为公开 game_id（`[a-z0-9][a-z0-9_-]{0,31}`）。
- 认证：`Authorization: Bearer <access_token>`。token 不放 URL。
- 请求/响应体均为 JSON（`Content-Type: application/json`），默认请求体上限 1 MiB（存档、埋点另有更小上限）。
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
| 409 | insufficient_funds | 扣减会导致余额为负（服务端内部 API） |
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

首次注册：空 body `{}`，返回新玩家与一次性展示的 `guest_secret`（客户端须持久保存）。

再次登录：`{"guest_id": "gst_...", "guest_secret": "..."}`，返回同一玩家与新会话；`"rotate": true` 时返回新的 `guest_secret`，旧值立即失效。

```json
{ "player_id": "plr_...", "guest_id": "gst_...", "guest_secret": "... 或 null",
  "access_token": "...", "expires_at": 1764000000000, "created": true }
```

### POST /auth/platform

`{"platform": "wechat", "code": "..."}`。服务端调用平台接口验证（微信 code2session），按 (game, platform, openid) 找到或创建玩家，返回同上结构（无 guest 字段）。未配置平台时返回 `501 not_implemented`。

### POST /auth/bind（需认证）

`{"platform": "wechat", "code": "..."}`，把平台身份绑定到当前玩家。身份已属于其他玩家时 `409 identity_conflict`，不覆盖任何数据。返回 `{"player_id": "...", "platform": "wechat"}`。

### POST /auth/logout（需认证）

撤销当前会话，返回 `204`。

### GET /me（需认证）

`{"player_id": "...", "game_id": "...", "created_at": 0, "identities": ["guest", "wechat"]}`

## 配置

### GET /config?client_version=1.2.0

返回该游戏当前发布配置中 `client` 段：`{"revision": 3, "data": {...}}`，带 `ETag: "3"`（带引号的强 ETag）；请求带 `If-None-Match` 且未变化时返回 `304`。未发布任何配置时 `{"revision": 0, "data": {}}`。不需要认证。

配置文档（管理员发布）结构：

```json
{
  "client": { "任意客户端可见键": "值" },
  "boards": { "main": { "order": "desc", "mode": "best", "period": "none|daily|weekly", "min_score": 0, "max_score": 1000000, "quarantine_above": 500000 } },
  "rewards": { "daily_login": { "grants": {"coin": 100}, "period": "once|daily", "per_period": 1, "starts_at": 0, "ends_at": 0 } }
}
```

## 存档（需认证）

槽位名 `[a-z0-9_-]{1,32}`；默认单槽 64 KiB、每玩家 5 槽。

### GET /saves/{slot}

`{"slot": "main", "revision": 3, "schema_version": 1, "checksum": "sha256hex", "payload": <JSON>, "updated_at": 0}`；无存档时 `404 not_found`。

### PUT /saves/{slot}

```json
{ "expected_revision": 3, "schema_version": 1, "payload": <JSON>, "checksum": "可选，sha256hex(payload 原始字节)" }
```

`checksum` 若提供，必须是请求 JSON 中 `payload` 值原始字节（不重新序列化）的 SHA-256 十六进制；服务端返回的 checksum 按同一规则计算。`expected_revision` 为 0 表示首次创建。成功返回新存档（revision+1）。版本不匹配返回 `409 revision_conflict` 与 `details.current_revision`。

## 排行榜

### GET /boards/{board}?limit=50&cursor=...&season=...

不需要认证；带 token 时附带 `me`。

```json
{ "board": "main", "season": "2026-11-24", "entries": [ {"rank": 1, "player_id": "plr_...", "score": 900, "achieved_at": 0} ],
  "next_cursor": "opaque 或 null", "me": {"rank": 12, "score": 300} }
```

排序：分数（按 order）→ 达成时间早者优先 → player_id。`limit` 1–100，默认 50。

### POST /boards/{board}/scores（需认证，支持幂等键）

`{"score": 900}` → `{"status": "accepted|quarantined", "best": 900, "season": "..."}`。超出 `[min_score,max_score]` 返回 400；超过 `quarantine_above` 的成绩进入隔离，不上榜。

## 经济（需认证）

### GET /wallet

`{"balances": {"coin": 100, "gem": 0}}`（资产代码 → 整数余额，道具同样以资产代码表示）。

### POST /rewards/{reward}/claim（需认证，支持幂等键）

请求体为 `{}`（幂等摘要包含请求体，重试须发送相同内容）。

服务端按当前配置判定资格。成功：`{"claim_id": "rcl_...", "grants": {"coin": 100}, "balances": {...}, "config_revision": 3}`。超出领取上限：`409 already_claimed`；不在时间窗或未定义：`404 not_found`/`403 forbidden`。

## 埋点

### POST /events/batch（可匿名；带 token 时关联玩家）

```json
{ "events": [ { "event_id": "唯一", "event_name": "level_end", "session_id": "...", "client_time": 0,
  "client_version": "1.0.0", "platform": "web|wechat", "schema_version": 1, "properties": {} } ] }
```

单批 ≤100 条、请求体 ≤64 KiB、单事件 properties ≤4 KiB。返回 `202 {"accepted": n, "dropped": m}`。服务端缓冲满时丢弃并计数（可丢失语义），`event_id` 去重，因此不使用 `Idempotency-Key`，重发安全。携带无效/过期 token 返回 401，客户端可去掉 token 匿名重发。

## 管理接口（API Key，`Authorization: Bearer gsk_...`，需 `admin` 权限）

- `POST /v1/admin/games` `{"game_id": "demo", "name": "Demo", "allowed_origins": ["https://example.com"]}`
- `PUT /v1/admin/games/{game}/config` body 为配置文档 → `{"revision": n}`
- `POST /v1/admin/games/{game}/config/rollback` `{"revision": n}` → 以旧内容发布新 revision
