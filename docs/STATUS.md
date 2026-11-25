# 实现状态（2026-11-25）

只记录已执行验证的结果。"已实现"指代码存在且有下列测试覆盖；"未验证"指代码存在但缺少要求的环境或证据。开发分支 `dev/full-scope`。

## 验证命令与结果（本机 macOS arm64，Go 1.27.4 toolchain，Node 24，PostgreSQL 17.11，Valkey 9.2.0）
- `GOSTACK_TEST_POSTGRES_DSN=… GOSTACK_TEST_VALKEY_ADDR=… GOSTACK_TEST_VALKEY_SERVER=… ./scripts/ci.sh`：gofmt、`go vet`、`go test -race -count=1 ./...` 分别在 SQLite、PostgreSQL+Valkey、PostgreSQL（本地限流）三轮全部通过；linux amd64/arm64 `CGO_ENABLED=0` 交叉编译；SDK typecheck + 64 个测试；SDK 核心 gzip 6830 B（预算 30720 B），realtime 独立入口 2149 B。
- OpenAPI：`docs/openapi/gostack.yaml`（OpenAPI 3.1，137 路径/163 操作，含 AgentKit/WebKit）；`app/openapi_test.go` 双向核对文档与源码路由、错误码枚举与源码一致、每个文档化操作在开启全部模块的 app 上存在。
- 独立审查（第二轮，只读子代理，覆盖认证/资金/存储层/生命周期）：10 项问题全部修复且均有回归测试——PG 下埋点毒批次（入口拒绝控制字符/非法 UTF-8 + flush 逐行 SAVEPOINT 隔离）、`CF-Connecting-IP` 仅在 `http.trust_cf_connecting_ip` 显式开启时信任、本地存储预签名路由脱离通用 body/超时限制、存档响应字节与 checksum 一致、签到/任务改时区不再重复领取、游客邀请奖励延迟到绑定平台身份后领取、对战房间 token 改随机并绑定认领 key、空租户 billing key 被拒、隔离指标不在可重跑事务内计数、榜单规则在写事务内读取。另一项由 OpenAPI 核对发现：运营/社交发放绕过道具目录与 `max_stack`，已改走 `Kit.Grant`。
- 第一轮审查（P0 主干）7 项问题已修复（见 git 历史）。
- govulncheck：GitHub Actions 中运行；`toolchain go1.27.4`（1.27.1 标准库漏洞的修复版本）。CI 新增的 PostgreSQL / Valkey service job 尚未在 GitHub 上运行过。
- 未运行：linux 二进制实机运行、部署脚本在真实 Linux/systemd 上运行。

## 通用核心（PRD §5）
| ID | 状态 | 说明 |
|---|---|---|
| F01 CLI | 已实现 | init（不覆盖文件）、doctor、config validate/diff、version、migrate status/up/to-postgres、backup snapshot/verify/restore（SQLite 与 PostgreSQL）、game/apikey/jobs 管理；退出码 0/1/2/3，`--json` |
| F02 HTTP | 已实现 | 请求 ID、统一错误信封、body/超时/并发上限、限流（本地或 Valkey 全局）、panic 恢复、可信代理 IP 解析（CF 头需显式开启） |
| F03 数据库 | 已实现 | Lite：SQLite 单写连接 + 只读池、每连接 PRAGMA、BEGIN IMMEDIATE、写入准入满 → 503、实例锁；Scale：PostgreSQL SERIALIZABLE + 有界重试；两种方言的迁移与漂移检测 |
| F04 配置 | 已实现 | 默认值<文件<环境变量<CLI、严格未知键、能力矩阵校验、密钥引用；SIGHUP 热更新 6 个热键（日志级别、并发上限、各限流配额），重启项只报告不生效（`app/reload_test.go` 含真实 SIGHUP） |
| F05 认证 | 已实现 | API Key（摘要、scope、过期、撤销即时生效）、游戏会话；Web 认证（Argon2id、Cookie 会话、`__Host-`、CSRF 令牌 + Origin/Sec-Fetch-Site、限流、改密吊销） |
| F06 队列 | 已实现 | 持久化任务（SQLite / PG `FOR UPDATE SKIP LOCKED`）：租约 token、重试退避、死信、重放、dedupe、同事务入队；Worker |
| F07 可观测性 | 已实现 | /healthz、/readyz、内网 /metrics 与 pprof、JSON 日志脱敏；`deploy/prometheus/alerts.yml` 17 条规则（测试核对每个指标名与 label 真实暴露、runbook 锚点存在）；`docs/RUNBOOK.md`。阈值为起点值，未经实测 |
| F08 部署 | 已实现（未在 Linux 实跑） | install/upgrade/rollback.sh、release.sh（sha256、`go version -m` SBOM，非 SPDX）；`deploy/deploy_test.go` 用桩命令在临时目录真实执行（校验和、原子切换、健康检查失败自动回滚、schema 更新时拒绝回滚） |
| F09 容灾 | 本地演练通过 | `backup snapshot/verify/restore`；Litestream v0.5.18 演练：本地文件副本 6 次 + MinIO（S3）2 次，kill -9 → 空目录恢复 → verify → API 回读全部通过，丢失窗口约一个 sync-interval（见 `docs/DISASTER_RECOVERY.md`）；**R2 与 1C1G 未演练**。PostgreSQL：`backup snapshot/verify/restore`（pg_dump/pg_restore + 清单，PG 测试覆盖）与 PITR（WAL 归档 + `pg_basebackup`，`deploy/postgres`）本机演练 4 次通过（到时间点 T 精确截断；机器丢失 RPO 0.8–6.9 s，取决于负载与 `archive_timeout`）；远程归档与 1C1G 未演练 |
| F10 计费 | 已实现 | `modules/billing`：账户/不可变账本/预留状态机（reserved→settled/released/expired/reconciling）、幂等、对账、HTTP（`billing` scope，空租户 key 拒绝） |
| F11 对象存储 | 已实现 | local（原子写、HMAC 预签名）与 S3 兼容（自写 SigV4，AWS 文档向量通过）；**对真实 MinIO 实测 9 项通过**；AWS S3 / R2 未实测 |
| F12 Scale | 已实现（见下两节） | PostgreSQL、多实例、SQLite→PG 迁移工具、Valkey 全局限流与缓存、PG 逻辑备份与 PITR（本机演练）；PG/Valkey HA 未实现 |
| F13 HA / Distributed | 未实现 | 配置校验拒绝 `distributed` profile |
| 缓存（PRD §4.2） | 已实现（无内置使用方） | memory（LRU、字节计量、TTL）与 valkey；读错误降级为未命中；资金路径不使用 |
| 6.2 组提交写入调度器 | 未实现 | 串行短事务 + 有界准入（ADR-0002）；组提交需基准证明收益 |

## F12 Scale：PostgreSQL（2026-11-25，ADR-0003）
验证环境：本机 macOS arm64，PostgreSQL 17.11（临时集群，max_connections=100），Go 1.27.4。
- 命令与结果：`./scripts/ci.sh`（未设 `GOSTACK_TEST_POSTGRES_DSN`，SQLite）通过；`GOSTACK_TEST_POSTGRES_DSN=postgres://postgres@127.0.0.1:55432/postgres?sslmode=disable ./scripts/ci.sh`（SQLite + PostgreSQL 各跑一遍 `go test -race -count=1 ./...`）通过。PG 运行：178 个顶层测试中 175 通过、3 个跳过（SQLite 备份演练、`migrate status` 未知版本检测——两者操作 SQLite 文件），0 失败。kits/game/*、modules/{jobs,billing,webauth}、app 的存储测试在该模式下各自使用新建的 PG 数据库；adapters/sqlite、tests/fault、deploy 仍跑 SQLite。
- 已实现且有测试：`adapters/store` 方言无关句柄；`adapters/postgres`（pgx stdlib、SERIALIZABLE + 40001/40P01 有界重试、fn 吞错仍重试、读池只读、`?`→`$n` 改写单测、C 排序校验、analytics 独立 schema）；`migrations/postgres{,_analytics}` 与 SQLite 逐版本对应，`TestSchemaEquivalence` 比较 60 张表的列/主键/唯一约束/索引/外键/触发器；账本 UPDATE/DELETE/TRUNCATE 被拒；迁移 advisory lock 并发启动；配置校验（scale/lite/distributed、realtime、local storage、litestream、`rate_limit.accept_per_instance`）；`migrate status|up`、`doctor`、`game reconcile` 在 PG 上运行；`backup snapshot/verify/restore` 在 PG 上用 pg_dump/pg_restore（`app/backup_pg_test.go`：快照一致性清单、临时库验证并删除、篡改/截断/缺清单退出 3、恢复拒绝非空目标、失败恢复目标保持为空、恢复后应用可启动写入、独立 analytics 库；`password`/`sslpassword`/`oauth_client_secret` 不进入工具 argv：口令走 `PGPASSWORD`，其余走临时 0600 服务文件）。
- PG 备份/PITR（2026-11-25）：`GOSTACK_TEST_POSTGRES_DSN=… go test -race -count=1 ./...`（PATH 含 PostgreSQL 17.11 客户端工具）全部通过；`deploy/postgres_test.go` 在无服务器下执行归档/恢复脚本。PITR 演练 `scripts/drill/pg_pitr.sh` 两组各 2 次（8 并发无间隔 / 4 并发 20 ms 间隔，60 s，`archive_timeout=10s`）全部通过：负载中基础备份与逻辑快照+verify、kill -9 主库进程组、恢复到 T 后 0 个 T 前确认写入缺失且 0 个 T 后发送写入出现、latest 恢复重放全部已归档 WAL、RTO 0.9–3.7 s、RPO 0.8–6.9 s（本机 macOS、本地归档目录，见 `docs/DISASTER_RECOVERY.md`）。
- 多实例（`tests/scale`）：两个 App 实例共享一个 PG 库，并发领奖/购买/幂等重放、赛季结算、撮合、任务 Claim 均只生效一次。
- SQLite→PG 迁移（`migrate to-postgres`）：经 HTTP 在 SQLite 上写入玩家/会话/存档/分数/隔离记录/账本/邮件/好友/撮合票/计费/网页用户/任务/埋点 → 服务运行时迁移被拒（实例锁）→ 停服后迁移，逐表行数与摘要一致、对账 0 差异 → 再次迁移因目标非空被拒 → 在 PG 上启动应用，11 个读接口响应与迁移前逐字节一致，幂等重放一致，identity 序列续号正常。
- 未验证 / 未实现：Scale 性能预算（PRD §13/§23）未压测；PG 远程 WAL 归档（pgBackRest/WAL-G）与 1C1G 上的备份恢复时间；PG HA；pgmq（不使用）；在线双写迁移。热点行在 SERIALIZABLE 下产生大量序列化失败重试（测试中可见数百次），吞吐影响未测量。

## F12 Scale：Valkey 全局限流与缓存（2026-11-25，ADR-0004）
验证环境：本机 macOS arm64，Valkey 9.2.0（临时实例，无持久化）、PostgreSQL 17.11（临时集群），Go 1.27.4。CI 配置为 `valkey/valkey:8` 服务容器（本次未在 CI 上运行）。
- 命令与结果：`./scripts/ci.sh`（不设任何测试变量，SQLite）通过；`GOSTACK_TEST_POSTGRES_DSN=… GOSTACK_TEST_VALKEY_ADDR=127.0.0.1:56391 GOSTACK_TEST_VALKEY_SERVER=…/valkey-server ./scripts/ci.sh`（SQLite 一遍、PG+Valkey 全套一遍、PG 无 Valkey 的 app/kits/tests/scale 一遍）通过。
- 已实现且有测试：自写 RESP2 客户端（假服务器测协议、超时丢弃连接、ctx 取消、连接池上限；真服务器测二进制安全与 NOSCRIPT 回退）；令牌桶 Lua 脚本（服务器时间、键 TTL、热更新升降配额、两个实例 + 400 goroutine 共享键总放行不超过配额、无响应/拒连时按 FailOpen/FailClosed/FailLocal 快速返回并计数、服务器重启后自动恢复且配额从满额开始）；所有限流使用点改为 `ratelimit.Allower`；配置校验（valkey 驱动需地址与键前缀、lite 拒绝 valkey、scale 下 local 需确认、valkey 时拒绝确认项、孤立 `valkey.*` 被拒）；`tests/scale`：两个 App 实例共享 PG 与 Valkey，游客登录每 IP 配额 12/分钟，48 个并发请求在两实例上合计放行 12（全局生效），两实例均记录全局拒绝；跨实例缓存可见；Valkey 不可达时拒绝启动。
- 未实现 / 未验证：Valkey Sentinel/Cluster/HA、TLS、主从切换下的语义；Valkey 往返对延迟/吞吐的影响未压测；内置功能尚未使用缓存。

## GameKit（PRD §20）
| ID | 状态 | 说明与测试 |
|---|---|---|
| G01 游戏注册 | 已实现 | 未知/停用游戏拒绝、CORS 白名单、管理 API 租户隔离 |
| G02 玩家身份 | 已实现 | 游客注册/复登/轮换/登出撤销；微信 code2session（**只对假服务测试**）；绑定冲突不覆盖 |
| G03 云存档 | 已实现 | 条件更新、并发仅一方成功（G-A03）、幂等、历史列表/恢复/删除、合并钩子 `Kit.SaveMerge`、`min_schema_version` |
| G04 排行榜 | 已实现 | best/sum、赛季、稳定排序、游标、本人名次；赛季结算（唯一结算键、可恢复、并发结算只发一次、定时调度任务） |
| G05 经济 | 已实现 | 整数余额、不可变账本、道具目录与 `max_stack`、礼包购买、管理调整（审计）、对账；并发购买/领奖只成交一笔 |
| G06 配置 | 已实现 | 版本化、ETag/304、回滚、灰度 rollouts（稳定分桶、平台/客户端版本、停止开关） |
| G07 埋点 | 已实现 | 独立 analytics 存储、有界缓冲、去重、保留期；入口文本校验与 flush 坏行隔离（G-A06） |
| G08 反作弊 | 已实现 | 分数范围、隔离阈值、`rule_version`、一次性对局令牌（防重放/最短时长）、人工审核 API |
| G09 SDK | 已实现（未真机） | 全部客户端接口 + 独立 realtime 入口；fake transport/socket 单测；**Safari/微信真机（G-A08）未验证** |
| G10 运营 | 已实现 | 公告、邮件（广播懒创建）、签到、任务（服务端进度）、活动、兑换码（哈希存储、并发单次） |
| G11 商业化 | 已实现（未接真实平台） | Apple JWS（x5c 链到配置根证书）、通知 V2、退款与不足额补偿、AdMob SSV / HMAC 广告回调、inbox 去重、outbox；Google Play（服务账号 JWT→OAuth2、purchases.products.get、obfuscatedExternalAccountId 绑定、consume/acknowledge 任务（200 次重试预算≈4 天、403/404 等标记 needs_attention 并持续重试、Maintain 对死亡/缺失任务补发、订单记录 finish_state，迁移 0019）、RTDN Pub/Sub OIDC 校验、作废退款、促销/激励购买无 orderId 时以 token 为准、令牌/JWKS 单飞刷新）；微信支付 API v3 JSAPI（商户签名下单、回调验签+时间窗、AES-256-GCM 解密、金额/商户号/AppID 校验、退款冲正、Maintain 按 out_trade_no 查单补发货）；provider 回调不受每游戏请求配额限制（Kit.HandleCallback）；**全部只用本地生成的密钥和 httptest 假服务验证，未对接真实 Apple/Google/微信**；微信小游戏虚拟支付（米大师）、订阅、部分退款自动处理未实现 |
| G12 实验 | 已实现 | 稳定分桶、层互斥、曝光去重、停止开关、结果统计 |
| G13 社交 | 已实现 | 资料与隐私、好友/请求/拉黑、好友码、邀请（游客奖励延迟到绑定后）、分享归因、好友榜 |
| G14 实时通信 | 已实现（lite 单实例；scale 经 PostgreSQL 总线多实例，ADR-0005） | WebSocket 网关：token 不进 URL、Origin、连接/消息/速率预算、心跳、序号 + ack + 续传、慢消费者断开、停机 1001；本机 1000 空闲连接约 28 KiB/连接（非验收）。scale 需 `game.realtime.bus: postgres`（迁移 0015）：PG 分配每玩家 seq、表存重放窗口、LISTEN/NOTIFY 扇出；双实例测试覆盖跨实例 Send 有序连续、广播两实例且每玩家一条、跨实例续传（重放或 resume_failed）、LISTEN 断开/恢复后补齐、总线不可用时本地降级（4012 + 新 epoch + 指标）与恢复、4008/发布队列上限；本机跨实例延迟 p50≈0.43ms p99≈0.8ms（非验收）。吞吐/对其他写入的影响未压测；`bus: valkey` 未实现 |
| G15 匹配与房间 | 已实现 | 票据（单次消费）、按 rating 窗口撮合、房间租约 + 随机 token + 绑定认领 key、结算幂等 |
| 多游戏配额（G-A09） | 已实现 | `kits/game/quota.go`，迁移 0014：`max_players`/`save_bytes_total` 与业务写同事务条件更新计数（SQLite、PG 并发存档/恢复/删除后计数 = 实际字节且不超限）；`requests_per_minute` 每游戏限流器（Valkey 下两实例合计放行 = 配额，已测）；`analytics_events_per_day` 按实例按 UTC 日计数、启动后从埋点库补计（N 实例最多约 N×配额）；`write_concurrency` 按实例限制每游戏在途写请求（503）；管理 API GET/PUT `/v1/admin/games/{game}/quotas`、5 s 缓存、CLI `game quotas [--recount]`；每游戏指标（标签基数 = 游戏数）。测试：`kits/game/quota_test.go`（alpha 触发每项配额时 beta 不受影响）。未测：PG 单游戏计数行串行化的吞吐影响 |
| 资源发布（PRD §22，G-A07） | 已实现（MinIO 实测；R2/CDN 未实测） | `modules/assets` + `gostack assets publish/list/rollback/verify/prune` + 模块 `assets`（manifest/entry 公共接口、管理端列表/回滚）：内容哈希不可变键 + `Cache-Control: immutable`、版本化 manifest、发布顺序资源→manifest→入口→事务切换指针、失败保留旧版本、按游戏租约串行化、去重、回滚前存在性检查、verify 全量哈希、prune 永不删除保留/当前版本引用的对象；见 `docs/api/assets.md`、`docs/ASSET_RELEASE.md` |

## AgentKit / WebKit P1（PRD §19、§24，2026-11-25）
验证环境：本机 macOS arm64，Go 1.27.4，PostgreSQL 17.11（临时集群）。命令：`gofmt -l .`（无输出）、`go vet ./...`、`go test -race -count=1 ./...`（SQLite）与 `GOSTACK_TEST_POSTGRES_DSN=… go test -race -count=1 ./...`（PostgreSQL，`TestSchemaEquivalence` 含迁移 0017/0018）全部通过。生成项目 `gostack init --kit agent|web|game` 在本机 `go mod tidy`（离线模块缓存）+ `go build` + `config validate` 通过，agent/web 项目各自 `serve` 后用 curl 走通一次（创建 run 至 succeeded；建站/发布/公开读/sitemap/robots/下单支付）。
| ID | 状态 | 说明与测试 |
|---|---|---|
| AgentKit（`kits/agent`，`docs/api/agent.md`） | 已实现（P1） | run_id、步骤（llm/tool/wait）状态与 attempt、每步 checkpoint（大输出进对象存储）、回放式恢复（租约过期由另一 worker 接管，已完成步骤不重跑，旧 worker 被 owner_token 隔离）、不确定结果的非幂等工具调用（worker 丢失租约，或调用在 `step_timeout`/步骤 ctx 结束后失败）进入 `needs_reconciliation` 并由人工 resolve（重试型工作流不会以新 key 重发）、幂等工具同 key 重发、取消（步骤间 + 步骤 ctx）、运行截止与单步超时、`max_steps`/`max_cost_units` 预算、租户与工作流并发上限、租约前让出续跑任务、密钥引用解析与错误脱敏、追加型工具调用审计、HTTP（`agent` scope，租户来自 key）。不提供 exactly-once；工作流版本化、步骤自动重试策略、已完成 run 的保留清理未实现 |
| WebKit（`kits/web`，`docs/api/web.md`） | 已实现（P1） | 站点/页面/文章、不可变修订、slug 租户内唯一、draft→scheduled→published→archived（定时发布走 jobs，过期任务无效）、SEO 元数据、公开读 + sitemap.xml + robots.txt（ETag/304、Cache-Control）；订单状态机示例（billing 预留→结算、取消/退款补偿至多一次、事件追加日志；支付中断超过预留 TTL 后重试会释放过期预留并以新支付代次（迁移 0020 `pay_generation`）重新预留，仍只扣一次）；webhook outbox（同事务入队、HMAC 签名、退避重试、重复任务不重发、SSRF 防护：显式拒绝 IPv4/IPv6 特殊用途地址段（含 CGNAT `100.64/10`、NAT64/6to4/映射地址按内嵌 IPv4 判断），拨号时检查解析后的 IP、创建时检查字面 IP；默认投递客户端每个 Kit 只建一次并复用）；管理端 API key（`web` scope）或编辑者 web 会话（CSRF）。非 CMS/电商：无编辑器 UI、媒体库、商品目录、真实支付渠道、正文 HTML 清洗 |

## 验收项对照
- A01 构建：linux amd64/arm64 交叉编译通过，迁移与模板内嵌；未在 Linux 实机运行。
- A02 数据库：每连接 PRAGMA、读库拒写、迁移重复执行与漂移检测、两方言 schema 等价——通过。
- A03 并发：race 通过（三种后端）；余额非负；幂等不重复扣费；多实例并发只生效一次——通过。
- A04 崩溃：`tests/fault` SIGKILL 后已确认写入全部保留——通过（进程崩溃模型；**断电模型需目标机验证**）。
- A05 过载：写入准入满 503、埋点满丢弃计数、realtime 慢消费者断开——通过。
- A06 任务：租约回收、旧 token 无法 Ack、跨进程 Claim 不重复——通过。
- A07 安全：跨游戏/跨玩家/跨租户拒绝、未知字段拒绝、参数化 SQL、CSRF、两轮独立审查问题已修复、govulncheck；**无第三方渗透测试**。
- A08 容灾：本地文件与 MinIO 副本恢复演练通过；**R2 未演练**。
- A09 运维：脚本在桩环境通过；**真实 systemd 未演练**。
- A10 性能：**目标环境未执行**（本机基准见 `docs/BENCHMARK_REPORT.md`，仅供参考）。
- A11 扩展：PG 跑同一套业务契约测试 + 多实例测试——通过。
- 游戏专项：G-A01/02/03/04/06 通过；G-A05 通过（规则版本、对局令牌、隔离不发奖）；G-A07（资源版本切换与回滚无缺失文件）在 local 存储（SQLite/PG 测试）与真实 MinIO（`scripts/drill/assets_minio.sh`）上通过，**R2 与真实 CDN 未执行**；G-A08 真机未执行；G-A09 通过（多游戏隔离 + 每游戏资源配额，见 `docs/api/quotas.md`；配额吞吐成本未测）；G-A10 本地恢复后对账通过，R2 未执行。

## 仍需外部环境才能完成
1. 1C1G Linux 目标机：§13/§23 压测、驱动选型确认（ADR-0001）、部署脚本与 systemd 实跑、断电模型。
2. Cloudflare R2：S3 适配器实测、资源发布（`assets`）与 CDN 实测、Litestream→R2 复制与恢复（RPO/RTO、费用）。
3. 真实平台：微信 code2session、Apple 沙盒交易与通知、Google Play 购买与 RTDN、微信支付下单与回调、AdMob 回调。
4. 真机：Safari iPhone 与微信小游戏验证 SDK（G-A08）。
5. 未实现：F13 HA/Distributed、Valkey HA/TLS、realtime 的 Valkey 总线；AgentKit/WebKit 仅 P1 范围（见上节“不提供”项）。
