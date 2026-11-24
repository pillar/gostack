# 实现状态（2026-11-24）

只记录已执行验证的结果。"已实现"指代码存在且有下列测试覆盖；"未验证"指代码存在但缺少要求的环境或证据。

## 验证命令与结果（本机 macOS arm64, Go 1.27.1, Node 24.21）
- `./scripts/ci.sh`：gofmt 干净、`go vet` 通过、`go test -race -count=1 ./...` 全部通过、linux amd64/arm64 `CGO_ENABLED=0` 交叉编译成功、SDK typecheck + 35 个测试通过、SDK 核心 gzip 4486 B（预算 30720 B）。
- 生成项目验收：`gostack init demo --kit game --gostack-path <repo>` → `go build` → `doctor` / `migrate up` / `game create` / `game config publish` → `serve` → `sdk/typescript/examples/e2e.mjs` 全部步骤 ok → SIGTERM 优雅退出 → `game reconcile` 0 差异。空载 RSS 约 22 MiB（macOS，仅供参考，非 A10 证据）。
- 独立代码审查（只读子代理，聚焦认证/经济/幂等）：未发现隔离绕过、重复发奖或负余额路径；提出的 7 项中低级问题已修复（sum 榜按单次成绩隔离、存档原样返回字节以匹配 checksum、幂等键按玩家+操作隔离、平台登录响应去掉游客字段、埋点批次不因停机中断、限流表满时随机淘汰而非全部拒绝且 IPv6 按 /64 聚合、停机超时强制关闭连接），前 4 项有回归测试，修复后 `./scripts/ci.sh` 与 SDK 实机 e2e 复跑通过。
- 未运行：govulncheck（本地执行被权限策略拒绝，已写入 CI workflow，未在 CI 实跑）、linux 二进制实机运行。

## 通用核心（PRD §5）
| ID | 状态 | 说明 |
|---|---|---|
| F01 CLI | 已实现 | init（不覆盖文件）、doctor、config validate、version、migrate status/up、game/apikey/jobs 管理；退出码 0/1/2/3，`--json` |
| F02 HTTP | 已实现 | 请求 ID、统一错误信封、body/超时/并发上限、本地限流、panic 恢复、可信代理 IP 解析 |
| F03 数据库 | 已实现（Lite） | 单写连接 + 只读池、每连接 PRAGMA、BEGIN IMMEDIATE、BUSY 有界重试、写入准入队列满 → 503、实例锁、迁移与漂移检测 |
| F04 配置 | 部分 | 默认值<文件<环境变量<CLI、严格未知键、能力矩阵校验、密钥引用 env:/file:；**热更新未实现**（全部重启生效） |
| F05 认证 | 部分 | API Key（SHA-256 摘要、scope、过期、撤销即时生效）、游戏会话；**密码登录 / Cookie 会话 / CSRF 未实现** |
| F06 队列 | 已实现 | SQLite 持久化任务：租约 token、重试退避、死信、重放、dedupe、同事务入队；Worker 运行器 |
| F07 可观测性 | 部分 | /healthz、/readyz、内网 /metrics（Prometheus 文本）与 pprof、JSON 日志与敏感字段脱敏；**告警规则与故障手册未编写** |
| F08 部署 | 模板 | systemd 单元模板（生成项目内）；**安装/升级/回滚脚本未实现，未在 Linux 实测** |
| F09 容灾 | 模板 | Litestream 配置模板；**R2 复制与恢复演练未执行（A08 未通过）** |
| F10–F13 | 未实现 | 通用计费、对象存储、Scale、HA |
| 6.2 组提交写入调度器 | 未实现 | 首期用串行短事务 + 有界准入（见 ADR-0002），组提交需基准证明收益 |

## GameKit（PRD §20）
| ID | 状态 | 测试 |
|---|---|---|
| G01 游戏注册 | 已实现 | 未知/停用游戏拒绝、CORS 白名单、管理 API 租户隔离 |
| G02 玩家身份 | 已实现 | 游客注册/复登/轮换/登出撤销；微信 code2session（用假服务测试，**未对真实微信验证**）；绑定冲突不覆盖 |
| G03 云存档 | 已实现 | 条件更新、并发仅一方成功（G-A03）、幂等重放、大小与槽位上限、历史保留；**合并函数、恢复历史版本 API 未实现** |
| G04 排行榜 | 已实现 | best/sum、desc/asc、none/daily/weekly 赛季、稳定 tie-break、游标、本人名次；**赛季结算发奖未实现** |
| G05 经济 | 部分 | 整数余额、不可变账本、来源键唯一、领奖资格/时间窗/次数、并发领奖仅一笔（G-A02）、对账 CLI；**道具独立模型、消费 HTTP 接口未实现** |
| G06 配置 | 部分 | 版本化、ETag/304、回滚、管理 API；**灰度/按客户端版本下发未实现** |
| G07 埋点 | 已实现 | 独立 analytics.db、有界缓冲丢弃计数、event_id 去重、保留期清理；过载不影响经济事务（G-A06） |
| G08 反作弊 | 部分 | 分数范围、隔离阈值、提交限流、会话与幂等；**规则版本、防重放签名、人工审核 API 未实现** |
| G09 SDK | 已实现（未真机） | 见 `sdk/typescript/README.md`；**Safari/微信真机（G-A08）未验证** |
| G10–G15 | 未实现 | |

## 验收项对照
- A01 构建：linux amd64/arm64 交叉编译通过，迁移与模板内嵌；未在 Linux 实机运行。
- A02 数据库：每连接 PRAGMA、读库拒写、迁移重复执行与漂移检测——通过。
- A03 并发：race 通过；余额非负由 CHECK 与 ApplyLedger 保证；幂等不重复扣费——通过。
- A04 崩溃：`tests/fault` SIGKILL 后已确认写入全部保留、integrity_check 与对账通过——通过（进程崩溃模型；**断电模型需目标机验证**）。
- A05 过载：写入准入满返回 503、埋点满丢弃计数——通过。
- A06 任务：租约回收、旧 token 无法 Ack——通过。
- A07 安全：跨游戏/跨玩家拒绝、未知字段拒绝、参数化 SQL；**系统性安全测试与依赖扫描未执行**。
- A08 容灾、A09 运维、A10 性能、A11 扩展：**未执行**。
- 游戏专项：G-A01/02/03/06 通过；G-A04（外部回调）不适用（IAP/广告回调未实现）；G-A05 部分（隔离阈值）；G-A07/08/09/10 未执行。

## 下一步（建议顺序）
1. M0 剩余：在 1C1G Linux 上跑驱动基准（modernc vs CGO）与 §13/§23 压测，按结果确认 ADR-0001。
2. 部署脚本（install/upgrade/rollback）与 Litestream→R2 恢复演练（A08/A09）。
3. GameKit P0 缺口：赛季结算（用 jobs + dedupe 键）、存档历史恢复、配置灰度、道具模型。
4. 真机：Safari iPhone 与微信小游戏验证 SDK（G-A08）。
