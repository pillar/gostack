# ADR-0004 Scale：Valkey 缓存与全局限流

- 状态：已采用，2026-11-25
- 范围：PRD §3 Scale 列（缓存"本地或 Valkey"、禁止本地限流冒充全局限流）、§4.2 Cache / RateLimiter 接口、F12。

## 背景

ADR-0003 之后 scale 可多实例运行，但所有限流器都是进程内令牌桶，配置校验只能要求 `rate_limit.accept_per_instance: true` 承认"有效配额 = 配置 × 实例数"。`cache.driver` 只有 `memory`，且没有任何代码使用缓存。

## 决定

1. **客户端：自写最小 RESP2 客户端 `adapters/valkey`，不引入 valkey-go。** 需要的命令只有 PING/GET/SET/DEL/EVALSHA/EVAL（+ 测试用 SCAN/PTTL），约 300 行标准库代码。理由：不增加直接依赖（AGENTS.md 要求新增依赖需 ADR，且 valkey-go 带来客户端缓存、集群、自动流水线等本项目不用的复杂度）；连接语义完全可控——每个连接一次只承载一条命令，任何 I/O/协议错误或超时都丢弃该连接，迟到的回复不可能被下一条命令读到；每条命令的截止时间 = min(ctx, Timeout)，ctx 取消会立即打断阻塞读写；连接池有界（`valkey.pool_size`），等待连接同样受截止时间约束。代价：无 TLS、Sentinel、Cluster、流水线（见限制）。协议解析与超时/连接丢弃有不依赖服务器的单测（假服务器），并对真实 Valkey 9.2.0 测试二进制安全读写和 NOSCRIPT→EVAL 回退。
2. **配置**：共享 `valkey:` 段（`address`、`username`、`password_ref`（env:/file:）、`db`、`key_prefix`（必填，`[A-Za-z0-9_.:-]{1,64}`，多个应用共用服务器时隔离键）、`dial_timeout`、`pool_size`），由 `cache.driver: valkey` 与 `rate_limit.driver: valkey` 共用一个连接池。校验：任一驱动为 valkey 时要求合法地址与前缀；两者都不是 valkey 时拒绝填写 `valkey.*`（防止以为已启用）；profile lite（单实例）拒绝 valkey 驱动；scale 下 `rate_limit.driver: local` 仍需 `accept_per_instance: true`，`driver: valkey` 时该确认项被拒绝（语义矛盾）。`rate_limit.timeout`（默认 50ms，上限 1s）与 `cache.timeout`（默认 100ms）分别限制每次调用。启动时 PING 失败则拒绝启动（配置错误不应静默降级）；运行期故障按下述策略处理；`/readyz` 不依赖 Valkey，避免 Valkey 故障把所有实例摘出负载均衡。
3. **缓存 `modules/cache`**：接口 `Get/Set(ttl)/Delete`（带 context）。`memory`：LRU，按 key+value+96 字节/条计入 `cache.max_bytes`，`ttl` 必须为正，过期条目读时删除，单值上限 `cache.max_value_bytes`（memory 下 ≤ max_bytes/8），读写都复制切片。`valkey`：键 `<prefix>cache:<key>`，`SET … PX`，单值上限，超时。**缓存失败不得改变业务事实**：包文档规定缓存只存可从数据库重算、允许短暂陈旧的数据；`cache.GetOrMiss` 把任何读错误降级为未命中；`App.Cache` 包装了 `cache_{hits,misses,errors}_total{driver}`。内置功能目前**不使用**缓存，资金/账本/库存/幂等/会话路径均不读缓存。
4. **全局限流 `ratelimit.Global`**：接口 `ratelimit.Allower{Allow(key) (bool, retryAfter); SetPerMinute(n)}`，本地 `Limiter` 与 `Global` 都实现它；所有使用点（GameKit 登录/分数/埋点、兑换码、社交、撮合、网页登录）改为通过 `ratelimit.Factory` 创建，名字唯一（`game_auth`、`game_score`、`game_events`、`redeem_player`、`redeem_ip`、`social_requests`、`match_tickets`、`web_auth_ip`、`web_auth_account`）。算法与本地实现相同的令牌桶（容量 n、每分钟补 n），以 Lua 脚本在 Valkey 中原子执行（脚本串行执行），用服务器 `TIME` 计时（不受实例时钟偏差影响）；哈希存 tokens/last/burst，热更新调高配额立即补差、调低立即截断（与本地 `SetPerMinute` 语义一致，跨实例生效）；键 `<prefix>rl:<name>:<key>`，在桶补满后 +1s 过期。EVALSHA，遇 NOSCRIPT（重启/SCRIPT FLUSH）回退 EVAL。
5. **故障策略（每个使用点显式选择）**：Valkey 超时/拒连/报错时：
   | 限流器 | 策略 | 理由 |
   |---|---|---|
   | `game_auth`（游客/平台登录，按 IP） | **FailLocal**：退化为同配额的进程内令牌桶 | 防刷仍有上界（= 引入 Valkey 前 scale 的保证），登录不中断 |
   | `game_score`（提交分数/开局，按玩家） | FailLocal | 同上；分数还有范围/隔离/会话校验 |
   | `web_auth_ip` / `web_auth_account`（密码校验） | FailLocal | 暴力破解仍受每实例上限约束，登录可用 |
   | `social_requests`、`match_tickets` | FailLocal | 滥用防护，非安全边界 |
   | `game_events`（埋点，按 IP） | **FailOpen** | P2 数据；分析缓冲区有界，溢出即丢弃 |
   | `redeem_player` / `redeem_ip`（兑换码） | **FailClosed**（429，Retry-After 1s） | 兑换码是可猜测的秘密；故障期间暂停兑换，避免实例数放大猜测预算 |
   单次检查最多阻塞 `rate_limit.timeout`。可观测：`ratelimit_global_errors_total{limiter,on_failure}` 计数，每个限流器每 10s 最多一条 WARN 日志；拒绝分别计入 `ratelimit_global_denied_total{limiter}` / `ratelimit_local_denied_total{limiter}`（指标名写明作用域），原 `game_rate_limited_total` 保留。
6. **测试**：`GOSTACK_TEST_VALKEY_ADDR` 启用 Valkey 测试（`adapters/valkeytest` 每测试随机前缀、结束时删除）；与 `GOSTACK_TEST_POSTGRES_DSN` 同时设置时 `pgtest.UseScale` 把 scale 应用切到 valkey 限流与 valkey 缓存，整个 PG 套件在全局限流下运行；`scripts/ci.sh`/CI 另跑一遍不带 Valkey 的 app/kits/tests/scale 以覆盖本地限流路径。`GOSTACK_TEST_VALKEY_SERVER`（valkey-server 路径）启用"服务器重启"测试。

## 后果与已知限制

- 仅支持单个 Valkey 主节点：**不支持 Sentinel/Cluster/HA、TLS、RESP3**。Valkey 宕机期间限流按上表退化；限流与缓存状态无需持久化；无持久化的 Valkey 重启后所有配额从满额开始（已测试）。主从切换或网络分区下的全局语义未实现也未测试。
- 每次受限请求多一次 Valkey 往返（EVALSHA）；吞吐与延迟未在目标环境压测（PRD §13/§23 对 Scale 仍为未验证）。
- 全局限流键数量由 Valkey 内存与 TTL 约束，不像本地表有 `LocalMaxKeys` 上限；需为 Valkey 配置 `maxmemory`（建议 `volatile-ttl` 或 `allkeys-lru` 淘汰策略——淘汰限流键等价于重置该键配额）。
- 未加入 `/readyz`；Valkey 健康靠上述指标与日志告警。
