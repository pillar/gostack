# ADR-0005 Scale：实时模块多实例（PostgreSQL 总线）

- 状态：已采用，2026-11-25
- 范围：G14 实时通信在 profile scale（多个应用实例共用一个 PostgreSQL）下的投递、序号、续传与故障语义；配置键 `game.realtime.bus`。

## 背景

`kits/game/realtime` 的 Hub 把连接、每玩家序号和重放缓冲放在进程内存。多实例时：在实例 B 调用 `Send` 的消息到不了连在实例 A 的玩家；两个实例各自给同一玩家分配序号会冲突；客户端重连到另一实例时 epoch 不同，只能 `resume_failed`。因此此前配置在 scale 下直接拒绝 realtime 模块。

## 备选方案

| 方案 | 优点 | 缺点 |
|---|---|---|
| A. PostgreSQL LISTEN/NOTIFY（无持久化） | 无新组件；通知按提交顺序投递 | 载荷上限 8000 字节；每实例一个专用连接；不持久：连接断开期间的通知永久丢失；序号仍需一个权威 |
| B. Valkey pub/sub（+ INCR 作序号、LIST 作重放） | Lua 脚本内原子 INCR+RPUSH+PUBLISH，延迟低 | `adapters/valkey` 是一次一命令的 RESP2 客户端，**不支持 SUBSCRIBE**（需新写推送读循环）；Valkey 在 scale 中是可选组件（`rate_limit.accept_per_instance` 的部署没有 Valkey）；pub/sub 同样不持久，订阅断开期间消息丢失；无持久化的 Valkey 重启即丢序号与重放 |
| **C. PostgreSQL 表 + NOTIFY（采用）** | 序号权威与重放窗口在同一事务里落库，通知只是"唤醒"；断线期间的消息可从表补齐；任一实例都能按同一窗口回答续传；不新增依赖或组件（pgx 已在依赖中） | 每条消息一次 PG 写事务（SERIALIZABLE，同一玩家的并发发送会产生序列化失败重试）；跨实例延迟含一次提交 |

选择 C：多实例下"续传要么正确重放要么 `resume_failed`、绝不跳号或重复"需要一个所有实例都能读到的序号权威和重放窗口；A、B 都只是易失的广播通道，仍要另外解决这两点。scale 必然有 PostgreSQL，Valkey 不一定有。`game.realtime.bus: valkey` 未实现，配置校验拒绝。

## 决定

1. **序号权威**：表 `realtime_streams(game_id, player_id, epoch, last_seq)`（迁移 0015，SQLite 端同名表仅为 schema 对齐，lite 从不写）。每条消息在一个 `WriteTx` 中：`INSERT … ON CONFLICT DO UPDATE SET last_seq = realtime_streams.last_seq + 1 RETURNING epoch, last_seq` → 写入 `realtime_messages(game_id, player_id, seq, broadcast_id, body, created_at)` → 删除该玩家 `seq <= 新seq − ReplayBuffer` 的行 → `pg_notify('gostack_realtime', …)`。同一玩家的提交被该行串行化，NOTIFY 按提交顺序送达所有监听会话，所以每个实例看到的同一玩家消息都按 seq 递增。epoch 在行创建时随机生成，此后不变（行不删除，基数 ≤ 玩家数）。
2. **发布不阻塞请求路径**：`Hub.Send`/`Broadcast` 只把消息放入每实例有界队列（`game.realtime.bus_queue`，默认 4096），满则返回 `ErrOverloaded` 并计 `realtime_send_dropped_total{reason="bus_queue"}`；单个发布协程按 FIFO 每批最多 64 条写库，因此同一实例对同一玩家的发送顺序保持不变。不同实例之间的发送没有相对顺序，只有一个全序（seq）。
3. **扇出**：每实例一个专用 pgx 连接（`application_name=gostack-realtime`）`LISTEN gostack_realtime`，只把通知投递给本实例已有的该玩家流（有连接或仍在缓冲期）。通知载荷 `{"k":"s","g","p","e","s","b"}` 内联客户端信封；超过 7900 字节时省略 `b`，由接收方从表读取。空闲 15s 发 Ping 探活。
4. **一致性规则**（每实例的本地流是权威流的连续子区间）：通知 seq ≤ 本地 last 忽略（去重）；= last+1 追加；有缺口或载荷未内联则在同一快照中读 `realtime_streams` + `realtime_messages` 补齐；表也补不上（已被按条数/时间裁剪）则以 **4012** 关闭该玩家在本实例的连接并丢弃本地流——客户端重连续传，得到按表窗口的正确重放或 `resume_failed`，不会静默跳号。epoch 与本地不一致同样 4012。新连接首次接入时先读（必要时创建）权威行得到 `welcome` 的 epoch/seq，同步期间到达的通知暂存后按序合并。
5. **续传**：任一实例收到 `resume(epoch, last_seq)` 时，先从表读取 `last_seq` 之后（以及本地落后部分）的消息与本地缓冲合并，再按单实例相同的判定：epoch 相同、`last_seq ≤ 当前 seq`、且 `last_seq+1 …` 全在（合并后、受 `ReplayBuffer`/`ReplayMaxBytes` 约束的）缓冲中 → `resumed` 并按序重放；否则 `resume_failed`。表窗口：每玩家最近 `ReplayBuffer` 条、且未超过 `ReplayMaxAge`（各实例 janitor 每 max(ReplayMaxAge/4, 1s) 删除过期行）。`ack` 在总线模式下只释放本实例内存，不删除表中的行。
6. **广播**：`Broadcast` 发一条不落库的 NOTIFY `{"k":"b",id,…}`（`data` 须放得进一条通知，否则 `ErrTooLarge`）；每个实例为自己当前有连接的该游戏玩家各写一条带 `broadcast_id` 的消息（唯一索引 + 事务内存在性检查保证同一玩家同时连在两个实例时也只得到一条、一个 seq）。返回值为调用时连在**本实例**的玩家数。
7. **故障与降级**：总线"健康" = LISTEN 会话已连接 且 最近一次发布失败后的退避期已过。不健康时：
   - 发布协程不写库，直接**本地投递**（`realtime_bus_degraded_total{reason="bus_down"}`）；写库失败（含写入准入队列满、超时）同样把该批本地投递（`reason="publish_failed"`）并按 100ms→5s 抖动退避；
   - 本地投递使用**本地流**：玩家在本实例的权威流被替换为新的本地 epoch（seq 从 1 起），其连接先以 4012 关闭，因此任何连接内都不会混用两个权威的序号；触发切换的那条消息落在新流的 seq 1，重连后的 `welcome.seq` 已越过它，客户端的 `resume`（旧 epoch）得到 `resume_failed` 并通过 HTTP 重新拉取；之后的本地消息实时送达。连在其他实例的玩家在此期间收不到本实例发出的消息；
   - LISTEN 断开后按 100ms→5s 抖动退避重连（`realtime_bus_reconnects_total`、`realtime_bus_connect_failures_total`、`realtime_bus_disconnects_total`，gauge `realtime_bus_up`）；重连成功后先 LISTEN 再：所有本地流 4012 重置（回到权威流），所有权威流从表补齐断线期间其他实例发布的消息；
   - 任一权威活动（本实例发布成功、收到该玩家的通知）也会结束该玩家的本地流（4012）。
   Send/Broadcast 永不等待数据库或网络；`Hub.Close` 先把队列中的消息发布出去（受关闭 ctx 约束），再断开 LISTEN。
8. **配置**：`game.realtime.bus: none|postgres`（环境变量 `GOSTACK_GAME_REALTIME_BUS`），`game.realtime.bus_queue`。scale 下启用 realtime 模块必须 `bus: postgres`；`postgres` 要求 `database.driver: postgres`（lite 只能 `none`，保持进程内 Hub，不写库）；其他值（含 `valkey`）报"未实现"。LISTEN 会话使用 `database.dsn_ref`。

## 验证（本机，2026-11-25）

- 测试（`kits/game/realtime/bus_test.go`，需 `GOSTACK_TEST_POSTGRES_DSN`；两个 App 实例共用一个 PG 库）：B 上 Send → A 上连接按序、seq 连续；两实例并发发送 80 条无重复无缺口、各发送方内 FIFO；第二个连接接在 B 上共享同一流；广播到达两实例的玩家，同时连在两实例的玩家只得一条；跨实例续传（表窗口重放、epoch 错误/超前/窗口外 → `resume_failed`）；经 TCP 代理切断 A 的 LISTEN 期间 B 发出的消息在重连后补齐且有序；切断 B 的 LISTEN 时 B 降级本地投递（4012、新 epoch、本地实时送达、指标计数），恢复后 4012 回到原 epoch 且共享流从断点继续；4008 慢消费者与发布队列上限；通过配置接线（`game.realtime.bus: postgres`）的双实例投递。原有 realtime 测试在 SQLite 与 PG 上（不带总线）均通过。
- 本机跨实例投递延迟（同一台 Apple Silicon 开发机，PostgreSQL 17 本地、`go test -race`，Send 到另一实例 WebSocket 客户端读到该帧，串行 300 次）：p50 ≈ 0.43ms，p95 ≈ 0.61ms，p99 ≈ 0.69–0.81ms，max ≈ 1.3ms；同实例经总线约 p50 0.43ms。**仅为本机数字**，未在 1C1G 目标环境、真实网络或负载下测量，不是性能验收。

## 后果与已知限制

- 每条消息（含广播的每个接收者）一次 PG 写事务；同一玩家的高频并发发送会产生 SERIALIZABLE 重试。吞吐上限、对其他业务写入的影响未压测。
- 降级期间跨实例消息不可达；切换（降级/恢复）时该玩家在本实例的连接会被 4012 关闭一次，触发切换的那条消息不实时送达（客户端经 `resume_failed` 重新拉取）。
- `MaxPerPlayer`、`MaxConnections`、`MaxStreams` 均为每实例限额。
- 表窗口只按条数（`ReplayBuffer`）与时间（`ReplayMaxAge`）限制，`ReplayMaxBytes` 只约束实例内存；单玩家表中最多约 `ReplayBuffer × MaxPayloadBytes`。
- 各实例的 `ReplayBuffer`/`ReplayMaxAge` 应一致，否则窗口大小取决于最后写入的实例。
- PostgreSQL 主从切换、连接池耗尽时 LISTEN 无法建立等场景只按上述通用规则降级，未专门测试。
