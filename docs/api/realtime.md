# GameKit 实时通信（G14 WebSocket 网关）

实现：`kits/game/realtime`（依赖 `github.com/coder/websocket`）。状态：单实例实现，P2。

**仅单实例语义**：连接、消息序号和重放缓冲都保存在进程内存中。多实例部署时每个实例只看到自己的连接，`Send`/`Broadcast` 不会跨实例送达；跨实例需要共享消息总线，**未实现**，配置不得声称支持多实例实时推送。服务重启后序号和缓冲全部丢失（客户端会收到 `resume_failed`，须通过 HTTP 重新拉取状态）。不按消息写 SQLite。

## 连接

```
GET /v1/games/{game}/realtime   (WebSocket upgrade)
```

升级前检查（失败返回普通 HTTP 错误信封）：

| HTTP | code | 条件 |
|---|---|---|
| 404 | not_found | 游戏不存在 |
| 403 | forbidden | 游戏停用；Header token 属于其他游戏 |
| 400 | token_in_url | URL 查询参数含 `token`/`access_token`/`authorization`/`auth`/`bearer`（token 不放 URL，即使有效也拒绝） |
| 403 | origin_not_allowed | 带 `Origin` 且不在该游戏 `allowed_origins` 中（无 `Origin` 的请求放行，与 HTTP CORS 策略一致） |
| 401 | unauthorized | `Authorization` 头存在但无效、会话不存在或过期 |
| 503 | overloaded | 已达 `MaxConnections`（含未认证连接）或网关正在关闭 |

以上 JSON 错误信封只覆盖握手之前的检查。通过这些检查后，真正的 WebSocket 握手由 `github.com/coder/websocket` 的 `Accept` 处理：普通 `GET`（缺少或不含 `Upgrade`/`Connection` 头，或 HTTP 版本低于 1.1）返回 `426`，`Sec-WebSocket-Version` 不是 13 或 `Sec-WebSocket-Key` 缺失/格式错误返回 `400`；这些响应是库写出的纯文本（`http.Error`），**不是** JSON 错误信封，也没有 `request_id` 体字段，客户端不应对它们做 JSON 解析。

认证二选一：

1. 非浏览器客户端：升级请求带 `Authorization: Bearer <access_token>`。
2. 浏览器：升级后 `AuthTimeout`（默认 5s）内发送第一条消息 `{"type":"auth","token":"<access_token>"}`。

会话校验语义与 GameKit HTTP 相同（`auth.Hash(token)` 查 `sessions`、未过期、game 匹配、游戏 active）。连接期间每 `SessionRecheck`（默认 1min，随心跳执行）重新校验，登出/过期后以 4001 关闭。game_id/player_id 只来自会话。

认证成功后服务端先发送：

```json
{"type":"welcome","player_id":"p_...","epoch":"3f9a1c2b7d10.4","seq":12}
```

`epoch` 标识该玩家的消息流（服务端重启或流过期后会变化），`seq` 是当前最后一个已分配序号；之后的新消息实时推送。

## 服务端 → 客户端

业务消息（经 `Hub.Send`/`Hub.Broadcast` 发出）带每玩家单调递增、连续的 `seq`（同一玩家的多个连接共享序列）：

```json
{"type":"mail","seq":13,"data":{...}}
```

控制消息不带 `seq`、不进入重放缓冲：

| type | 字段 | 含义 |
|---|---|---|
| welcome | player_id, epoch, seq | 认证成功 |
| pong | — | 应答客户端 `ping` |
| resumed | last_seq, epoch, seq | 续传成功，随后按序重放 `last_seq+1 … seq`，再接实时消息 |
| resume_failed | epoch, seq | 无法续传：客户端须通过 HTTP 重新拉取状态，之后继续接收实时消息 |
| ok | ref | 带 `id` 的应用消息处理成功 |
| error | code, message, ref? | 见下表；连接保持 |

`error.code`：`invalid_message`（二进制帧、非 JSON、缺 type、resume 缺 last_seq）、`invalid_ack`、`already_authenticated`、`unknown_type`（未配置处理器或类型不合法/保留）、`internal`，以及应用处理器返回的 `httpx.Error` 的 code。

## 客户端 → 服务端

| 消息 | 说明 |
|---|---|
| `{"type":"ping"}` | 应用层心跳（浏览器无法发送 WebSocket ping），回复 `pong` |
| `{"type":"ack","seq":N}` | 确认已处理到 N；服务端释放该玩家缓冲中 ≤ min(N, 各在线连接已发送位置) 的消息 |
| `{"type":"resume","epoch":"...","last_seq":N}` | 续传，见下 |
| `{"type":"<app>","id":"r1","data":{...}}` | 应用消息，交给 `Module.OnMessage(ctx, gameID, playerID, Message)`；`id` 可选，回显为 `ref` |

应用消息类型须匹配 `[a-z][a-z0-9_.:-]{0,63}`，且不能是保留类型（auth、ping、pong、ack、resume、resumed、resume_failed、welcome、error、ok）。未配置 `OnMessage` 时一律回复 `unknown_type`。处理器在读循环中同步执行（同一连接内串行），超时 `MessageTimeout`；处理器可调用 `Hub.Send` 回复。

### 重连与续传

1. 客户端记录 `epoch` 与已处理的最大 `seq`。
2. 重连并认证后**立即**发送 `resume`；在收到 `resumed`/`resume_failed` 之前到达的业务消息应忽略（服务端会在 `resumed` 之后按序重发）。
3. 续传成功条件：`epoch` 相同，且 `last_seq+1 … 当前 seq` 全部仍在该玩家的缓冲中。否则 `resume_failed`（缓冲溢出、超龄、已被 ack 释放、服务端重启、epoch 不同）。
4. 玩家离线时 `Send` 的消息也进入缓冲（受 `ReplayBuffer`/`ReplayMaxBytes`/`ReplayMaxAge`/`MaxStreams` 限制），可在重连后续传取回。不续传的新连接只接收 `welcome.seq` 之后的消息。

## 关闭码

| 码 | 含义 |
|---|---|
| 1001 | 服务端关闭（`Hub.Close`） |
| 1009 | 消息超过 `MaxMessageBytes` |
| 1011 | 认证时内部错误 |
| 4001 | 未认证：缺少/无效 auth 消息、认证超时、会话失效或登出 |
| 4003 | token 属于其他游戏（首消息认证） |
| 4008 | 慢消费者：未发送的实时消息超过 `SendQueue`、需要的消息已被挤出缓冲、写超时或控制消息队列满 |
| 4009 | 同一玩家的新连接替换了最旧连接 |
| 4029 | 入站消息速率超限 |

心跳超时（`PingInterval` 后 `PongTimeout` 内无 pong）的连接被视为死连接，直接断开（不发关闭帧）。

## 资源预算（`realtime.Options`，独立于 HTTP 请求限制）

升级完成后处理函数立即返回，长连接不占用 HTTP 并发槽（`http.max_concurrent`），也不受请求超时约束；每个连接 2 个常驻 goroutine（读循环、写循环），心跳 goroutine 仅在 ping 等待期间存在。

| 选项 | 默认 | 说明 |
|---|---|---|
| MaxConnections | 4000 | 总连接（含未认证），超出 503 |
| MaxPerPlayer | 2 | 每玩家连接数；超出时**替换最旧连接**（4009），避免网络切换后的半开旧连接把玩家锁在门外直到心跳超时 |
| MaxMessageBytes | 4096 | 入站单条消息上限 |
| InboundRate / InboundBurst | 10/s / 20 | 每连接入站令牌桶（含 ping/ack） |
| SendQueue | 32 | 每连接未发送实时消息上限（不超过 ReplayBuffer）；续传重放不计入 |
| MaxPayloadBytes | 4096 | `Send`/`Broadcast` 的 data JSON 上限 |
| ReplayBuffer / ReplayMaxBytes | 64 条 / 16 KiB | 每玩家重放缓冲 |
| ReplayMaxAge | 2m | 缓冲条目及无连接空闲流的过期时间 |
| MaxStreams | 4000 | 为离线玩家新建缓冲流的上限（在线连接的流受 MaxConnections 约束） |
| PingInterval / PongTimeout | 25s / 10s | 服务端 WebSocket ping |
| AuthTimeout | 5s | 首消息认证时限 |
| WriteTimeout | 5s | 单帧写超时 |
| SessionRecheck | 1m | 会话重新校验间隔 |
| MessageTimeout | 5s | 应用处理器与认证查询超时 |

离线缓冲最坏内存约为 `MaxStreams × ReplayMaxBytes`（默认约 64 MiB）加在线流缓冲；`Broadcast` 只发给本实例当前在线的玩家，工作量受 MaxConnections 约束。

## 服务端 API

```go
m := &realtime.Module{Kit: a.Game, DB: a.DB, Metrics: a.Metrics, Options: opts, OnMessage: h}
m.Routes(mux)
hub := m.Hub()
err := hub.Send(gameID, playerID, "mail", payload)  // 非阻塞；ErrClosed/ErrTooLarge/ErrInvalidType/ErrOverloaded
n, err := hub.Broadcast(gameID, "news", payload)    // 返回送达的在线玩家数
err = hub.Close(ctx)                                // 1001 关闭全部连接，等待至 ctx 结束后强制断开
```

关闭顺序：`Hub.Close` 须在关闭数据库之前调用（`http.Server.Shutdown` 不管理已升级的连接）。

## 指标（observability.Registry）

- `realtime_connections`、`realtime_streams`（gauge）
- `realtime_connections_total`（认证成功的连接）
- `realtime_rejected_total{reason=token_in_url|origin|auth|max_connections}`
- `realtime_closed_total{reason=slow_consumer|ping_timeout|rate_limited|too_large|replaced|auth|auth_timeout|session_revoked|shutdown|write_error|internal|client}`
- `realtime_messages_out_total`、`realtime_messages_in_total`
- `realtime_resume_total{reason=ok|failed}`
- `realtime_send_dropped_total{reason=invalid|max_streams}`

## 未实现 / 未验证

- 多实例共享总线、房间（room）接口、匹配票据：未实现。
- PRD §13 的 3000 空闲连接 30 分钟、活跃连接吞吐/延迟：未在 1C1G 目标环境测量。测试中仅有本机 1000 空闲连接的 goroutine/堆增量记录（客户端与服务端同进程）。
