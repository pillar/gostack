# GameKit 安全模型

范围：PRD §20.1（身份与隔离）、§20.3（排行榜与反作弊）、§20.4（经济与外部回调）、§21（API 契约）。通用核心（API Key、密钥引用、Web 认证、部署）见 `docs/SECURITY.md`。每项控制都列出证明它的测试；**没有测试的条目明确标注**。测试命令：`go test -race ./kits/game/... ./modules/... ./app/ ./tests/...`（PostgreSQL 版本设 `GOSTACK_TEST_POSTGRES_DSN`）。

## 1. 资产与攻击者

| 资产 | 价值 |
|---|---|
| 钱包余额、道具、IAP 权益 | 直接经济价值；不可凭空产生或重复发放 |
| 排行榜名次与赛季奖励 | 竞争公平、间接经济价值 |
| 玩家存档 | 进度；不可被他人读取/覆盖 |
| 会话令牌、游客秘密、API Key、平台 AppSecret | 冒充玩家 / 管理员 |
| 埋点 | 低价值，可丢失；不得影响经济 |

攻击者：恶意玩家（完全控制客户端、可重放与并发请求、可改包）、其他游戏/租户的合法用户、伪造平台回调的外部方、窃取到单个凭据的人。**不在范围**：被攻破的服务器主机、数据库管理员、平台（Apple/微信/AdMob）自身被攻破。

## 2. 控制项

### 2.1 会话绑定（§20.1）

| 控制 | 实现 | 测试 |
|---|---|---|
| 会话令牌 256 位随机，库内只存 SHA-256，令牌只走 `Authorization` 头 | `kits/game/identity.go`、`core/auth` | `TestGuestLifecycle` |
| 令牌绑定单一游戏与玩家：用 A 游戏的令牌访问 B 游戏 → 403；玩家只能访问自己的数据 | `Kit.resolveSession` 比较 `session.game_id` 与 URL | `TestCrossGameAndCrossPlayerIsolation`（G-A01） |
| 登出立即撤销；过期会话 401；游客秘密可轮换、旧值立即失效 | `sessions` 表查库校验 | `TestGuestLifecycle` |
| 平台身份由服务端验证（微信 code2session），不接受客户端 openid；AppSecret 只能是密钥引用 | `WeChatVerifier`、`core/config` | `TestWeChatLoginAndBind`（假服务器，**未对真实微信验证**） |
| 绑定冲突 `409 identity_conflict`，不覆盖存档或资产 | `bindPlatform` | `TestWeChatLoginAndBind` |
| 实时连接：token 不得出现在 URL（`400 token_in_url`），连接期间周期性复核会话，撤销后以 4001 关闭 | `kits/game/realtime` | `TestAuthFailures`、`TestSessionRevokedWhileConnected`、`TestAuthHeaderAndFirstMessage` |

### 2.2 跨游戏 / 跨租户隔离（§20.1、§21）

| 控制 | 测试 |
|---|---|
| 所有游戏数据以 `game_id` 作用域查询；`player_id`/`game_id` 不从请求体读取（严格解码拒绝未知字段，存档请求体带 `player_id` → 400） | `TestCrossGameAndCrossPlayerIsolation` |
| 未知游戏 404、停用游戏 403 | `TestCrossGameAndCrossPlayerIsolation`（G01） |
| 管理 API 需 `admin` scope，且 key 的租户拥有该游戏，否则 404 | `TestConfigETagAndAdmin`、`TestAdminAdjust`、`TestQuarantineReview` |
| 模块级隔离：实验、商业化、撮合（battle key 按租户）、社交、运营 | `TestFiltersAndCrossGameIsolation`、`TestAdminValidationAndIsolation`、`TestTenantIsolation`（match）、`TestFriendCodeAndPrivacy` |
| IAP 交易跨玩家/跨游戏重放被拒（`transaction_owned_by_other_player`、`transaction_already_used`） | `TestAppleCrossPlayerAndCrossGameReplay` |
| 多实例共享 PG 时同样只生效一次 | `tests/scale`：`TestTwoInstancesRewardAndPurchaseOnce` 等 |

### 2.3 重放与重复（§20.3、§20.4、§21）

| 控制 | 测试 |
|---|---|
| 幂等键：同键同载荷重放、同键不同载荷 409，与业务同事务 | `TestSaveConflictAndIdempotency`、`TestIdempotencyReplayAndConflict`（billing） |
| 账本来源键唯一 + 只追加触发器：同一事件不能入账两次、历史不能篡改 | `TestLedgerRejectsOverdraftAndDuplicateSource`、`TestLedgerAppendOnly`、`TestLedgersAppendOnly`、`TestReconcileDetectsTamperingAndLedgerIsAppendOnly` |
| 并发领奖/购买/邮件领取/兑换码只成功一次 | `TestRewardClaimConcurrencyAndLedger`（G-A02）、`TestOffersInventoryAndStacks`、`TestMailConcurrentClaimPaysOnce`、`TestRedeemSingleUseConcurrentAndHashed` |
| 存档条件更新：并发保存只有一方成功（不以客户端时钟判定） | `TestSaveConflictAndIdempotency`（G-A03） |
| 对局令牌一次性、过期、绑定玩家与榜单 | `TestRunTokensAndRuleVersion` |
| 赛季结算唯一键，重复执行/部分失败恢复不重复发奖 | `TestSeasonSettlement`、`TestSeasonSettlementResumesAfterPartialRun`、`TestTwoInstancesSettleSeasonOnce` |
| 房间结算：fencing token、租约过期后不能结算、同体重放、不同体 409 | `TestRoomLifecycleAndSettlement`、`TestConcurrentSettlement`、`TestFencingTokensGrowAndAreRoomScoped`、`TestLeaseExpiryAbandonsRoom` |
| 实时消息序号与续传不重复投递 | `TestSeqOrderingAndIsolation`、`TestAckAndResume` |

### 2.4 反作弊的边界（§20.3）

已实现：分数范围（`min_score`/`max_score` → 400）、隔离阈值（`quarantine_above` → 不上榜、人工审核）、规则版本（`409 rule_version_mismatch`）、对局令牌与最短对局时长（过短 → 隔离）、每玩家提交限流、隔离审核与赛季结算互斥（`season_settled`）、服务端权威的房间结算（battle scope）。测试：`TestLeaderboard`、`TestRunTokensAndRuleVersion`、`TestQuarantineReview`、`TestSettlementQuarantineAndNoBoard`。

**明确的限制**：纯客户端游戏的成绩无法被证明真实；改包客户端可以在范围内、超过最短时长后提交任意分数。服务端不做回放校验或行为分析，没有设备指纹。高价值奖励应来自 match 模块的战斗服结算或人工审核。任务进度只能由服务端代码（`ops.Module.Progress`）或管理接口推进，客户端无接口（`TestTasks`）。

### 2.5 IAP 与广告回调的信任（§20.4）

| 控制 | 测试 |
|---|---|
| Apple JWS：ES256、证书链到配置的根证书、Apple 标记 OID、bundle id 与环境校验；验证在事务外 | `TestAppleRejectsBadProofs`、`TestAppleVerifyDeliversOnce` |
| 交易与玩家绑定：服务端签发 `app_account_token`，签名内的 token 决定归属；无 token 交易默认拒绝 | `TestAppleCrossPlayerAndCrossGameReplay`、`TestAppleUnboundTransactions` |
| 同一交易全局至多交付一次 | `TestAppleConcurrentDuplicateVerify`、`TestAppleVerifyDeliversOnce` |
| 服务器通知可重复、乱序：inbox 去重 + 确定性状态机；退款先于购买时永不发放；处理失败记录 `failed` 并允许重投 | `TestAppleNotificationsDuplicateAndRefund`、`TestAppleRefundBeforePurchase`、`TestAppleFailedNotificationRetried` |
| 退款不产生负余额，差额记为 `outstanding`（`refund_shortfall`）待人工处理 | `TestRefundShortfall` |
| 广告奖励只来自签名的服务端回调（AdMob SSV ECDSA / HMAC-SHA256 + 时间偏差），玩家来自一次性 intent token，不信任客户端“播放完成”；`reward_amount` 被忽略；每日上限在入账事务内计数 | `TestAdMobCallback`、`TestHMACAdCallback`、`TestAdDailyCap` |
| 未实现的提供方被配置拒绝 | `TestValidateProviders` |

**未验证**：未对真实 Apple / AdMob 端点或真实签名载荷测试（测试使用本地生成的证书链与密钥）。Google Play、微信支付未实现。

### 2.6 管理密钥

API Key `gsk_` 前缀、256 位随机、只存 SHA-256；scope（`admin`、`battle`、`billing`）按路由强制；可设过期；撤销即时生效（每次请求查库）。管理调整需要 `reason`，与操作 key id 一起写入审计表 `wallet_adjustments`；隔离审核记录审核人 key id。客户端会话令牌不能调用管理接口（401）。测试：`TestAdminAdjust`、`TestQuarantineReview`、`TestHTTP`（billing scope）、`TestQueueAdmin`（match）。**未实现**：管理后台 UI、细粒度角色、管理操作的统一审计日志（目前只有调整与审核两张表）。

### 2.7 CORS 与浏览器

GameKit 路由只对游戏 `allowed_origins` 中的 Origin 返回 CORS 头（不带凭据，允许头 `Authorization, Content-Type, Idempotency-Key, If-None-Match`）；无 Origin 的请求（小游戏、服务端）不受影响。CORS 不是认证：真正的保护是 Bearer 令牌（不使用 Cookie，故不需要 CSRF）。实时网关对带 Origin 且不在白名单的升级请求返回 `403 origin_not_allowed`。测试：`TestCORSAllowlist`、`TestOriginCheck`。Cookie 模式（webauth）的 CSRF/来源检查见 `docs/SECURITY.md`，测试 `TestCSRFAndOrigin`、`TestSecureCookieAndHTTPSOrigin`。

### 2.8 速率限制与过载

| 限制 | 键 | 测试 |
|---|---|---|
| 登录（游客/平台） | 客户端 IP（IPv6 /64），`game.auth_rate_per_minute` | `TestRateLimitAndErrorsEnvelope`（429 + `Retry-After` + 错误信封）、`TestReloadRetunesGameLimiters`、`TestIPKey` |
| 成绩提交与开局 | 玩家，`game.score_rate_per_minute` | **无专门测试**（测试夹具把速率调高；限流器本身见 `TestBucketAndBoundedKeys`） |
| 埋点 | IP，登录速率 × 4；批量与体积上限；缓冲满丢弃计数 | `TestEventsBatch`、`TestAnalyticsOverloadDoesNotBlockEconomy` |
| 兑换码 | 玩家 + IP，失败也计数 | `TestRedeemRateLimits` |
| 社交请求、撮合票据 | 玩家（`game.social.requests_per_minute`、`game.match.tickets_per_minute`） | **无专门的 429 测试**（`TestFriendLimits` 覆盖好友/待处理数量上限） |
| 实时消息 | 连接（令牌桶）、消息大小、连接数 | `TestMessageSizeAndRateLimits`、`TestConnectionLimits` |
| 并发与写队列 | 全进程，503 + `Retry-After` | `TestReloadRetunesConcurrencyGate`、`TestWriteQueueBounded` |

客户端 IP 只在直连对端属于 `http.trusted_proxies` 时才读取转发头：默认只从右向左解析 `X-Forwarded-For` 并跳过受信代理（`TestClientIPTrustsOnlyConfiguredProxies`）；`CF-Connecting-IP` 仅在显式设置 `http.trust_cf_connecting_ip: true` 时采用（要求 `trusted_proxies` 非空，且只应在所有受信代理都是 Cloudflare 时开启——其他代理会原样转发客户端伪造的该头，从而绕过每 IP 限流；`TestClientIPCFConnectingIPOnlyWhenEnabled`）。**限制**：所有限流器都是进程内的；scale profile 下有效配额 = 配置 × 实例数（配置须显式 `rate_limit.accept_per_instance: true` 承认），没有全局限流。限流表有容量上限，满时随机淘汰。

### 2.9 输入与错误

严格 JSON（未知字段、尾随数据 400）、各路由体积上限（413）、id/slot 正则、参数化 SQL；错误信封不包含内部错误文本（500 只返回 `internal` 与 request_id）；日志对 secret/token/password/cookie 等字段脱敏（`TestAccessLogRecordsRoutePatternAndRedacts`）。`app/openapi_test.go` 确保所有路由与错误码都在 OpenAPI 中列出。

## 3. 未执行的安全验收

系统性渗透/模糊测试、真实平台端点联调、Safari/微信真机、目标 Linux 主机的部署权限验证均**未执行**（同 `docs/SECURITY.md`、`docs/STATUS.md`）。
