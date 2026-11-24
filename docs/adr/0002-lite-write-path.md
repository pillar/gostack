# ADR-0002 Lite 写入路径：串行短事务 + 有界准入，组提交延后

- 状态：已采用，2026-11-24
- 背景：PRD §6.2 描述 8192 容量 channel、64 条/15ms 组提交及单事件失败隔离。组提交把多个请求合并到一个事务，需要 SAVEPOINT 隔离、逐个确认、部分失败语义，复杂度高；收益（减少 fsync 次数）只有在基准显示 FULL 同步写成为瓶颈时才成立。
- 决定：`sqlite.DB.WriteTx` 在唯一写连接上执行 `BEGIN IMMEDIATE` 短事务；准入信号量（`database.write_queue`，默认 1024）满时立即返回 `ErrOverloaded` → HTTP 503，不无限排队；等待受请求 deadline 约束；SQLITE_BUSY 有界重试 3 次。每个请求在提交成功后才响应，语义与 PRD P0 一致。
- 后果：每个写请求一次 fsync（FULL）。若 M0/A10 基准显示 FULL 同步写达不到 100 RPS / P99 ≤150ms，再按 PRD §6.2 实现组提交（每事件 SAVEPOINT，批内失败事件单独回滚并返回可重试错误）。
- 相关：埋点写入使用独立 `analytics.db`（synchronous=NORMAL）与独立写连接，不占用主库写入通道（PRD 评审 D4）。
