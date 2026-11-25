# ADR-0001 SQLite 驱动：暂选 modernc.org/sqlite（待 1C1G 基准确认）

- 状态：暂定（Provisional），2026-11-24；2026-11-25 补充本机对比证据，**状态不变**
- 背景：PRD §4 要求比较纯 Go 与 CGO 静态构建，依据实测选择，并验证 FTS 等编译特性与许可证。
- 决定：首期使用 `modernc.org/sqlite` v1.63.0（SQLite 3.54.1，BSD-3-Clause）。
- 理由：`CGO_ENABLED=0` 即可交叉编译 linux amd64/arm64（已验证）；FTS5 默认可用；DSN 支持每连接 `_pragma` 与 `_txlock=immediate`（已由 `adapters/sqlite` 测试验证每连接生效）；本机对比中写路径与 CGO 驱动无可区分差异（见下）。

## 2026-11-25 本机证据（Apple M5 Pro / macOS / APFS 内置 SSD，local, not acceptance）

工具：`benchmarks/driver`（独立 module，`github.com/mattn/go-sqlite3` v1.14.53 只出现在 `benchmarks/go.mod`）。两驱动相同设置：WAL、`synchronous=FULL`（测试断言 PRAGMA 实际为 2）、`BEGIN IMMEDIATE`、只读读连接。`go test -bench . -benchmem -count 5 -cpu 1,4`，原始输出与汇总在 `benchmarks/results/2026-11-25/driver_bench.{txt,md,json}`。下表为 `-cpu 1` 的 5 次 min / median / max（ns/op）：

| 基准 | modernc | mattn (CGO) | 结论 |
|---|---|---|---|
| FULL 单行写事务（1 KiB） | 98,732 / 115,094 / 135,646 | 78,830 / 99,246 / 108,329 | 区间重叠，spread 30–60%，无可区分差异；耗时由 fsync 主导 |
| 同上 + `fullfsync=1`（macOS F_FULLFSYNC） | 3.51 / 4.24 / 4.46 ms | 3.33 / 3.45 / 3.50 ms | 由磁盘刷新主导，约 3.3–4.5 ms/事务 |
| 批量插入 1000 行/事务 | 5.50 / 5.72 / 6.01 ms | 5.80 / 6.58 / 9.15 ms | 无可区分差异 |
| 主键点查，4 读协程/4 连接 | 5,522 / 5,914 / 6,228 | 3,550 / 3,730 / 3,852 | mattn 快约 35–40%；allocs 25 vs 17 |

- 点查的绝对差约 2 µs/次。按 1C1G 预算口径，500 RPS 点查的驱动 CPU 差约 1 ms/s（0.1% 核），不构成预算瓶颈；在本机 HTTP 压测中，请求成本主要在 HTTP/JSON/会话查询（见 `docs/BENCHMARK_REPORT.md`）。这一点尚需在 1C1G 上用 HTTP 负载确认。
- **FTS5**：modernc 默认启用（`ENABLE_FTS5`）；mattn 默认未启用（`no such module: fts5`），需 `-tags sqlite_fts5` 编译（已验证该标签下可用）。两者都捆绑 SQLite 3.54.1，JSON 函数可用。
- **二进制大小**（最小程序，`-trimpath`，`benchmarks/results/2026-11-25/driver_size.txt`）：darwin/arm64 modernc 9.94 MB（strip 后 6.45 MB），mattn 7.05 MB（strip 后 3.77 MB）；modernc linux/amd64 strip 后 6.65 MB、linux/arm64 6.49 MB（`CGO_ENABLED=0`）。mattn 体积小约 2.7 MB。
- **交叉编译**：在本机用 `CGO_ENABLED=1 GOOS=linux` 构建 mattn **失败**（系统 clang 无 Linux 目标 libc：`implicit declaration of function 'clearenv'`）。CGO 静态 Linux 构建需要 musl/zig 交叉工具链或在 Linux 上构建，增加发布流水线复杂度；modernc 不需要。
- **许可证**（从模块缓存中的文件核对，而非凭记忆）：`modernc.org/sqlite@v1.63.0/LICENSE` 为 BSD-3-Clause，附 `LICENSE-SQLITE`（SQLite 公有领域声明）、`LICENSE-SQLITE_VEC`（MIT）、`LICENSE-3RD-PARTY.md`（链接组件：musl MIT、Go BSD-3、go-humanize MIT、uuid/bigfft BSD-3 等）；`modernc.org/libc@v1.80.1/LICENSE` 为 BSD-3-Clause。`github.com/mattn/go-sqlite3@v1.14.53/LICENSE` 为 MIT，内含的 `sqlite3-binding.c` 为 SQLite 公有领域（"blessing"）。两者均允许商业使用与再分发；modernc 再分发需附带上述 BSD/MIT 声明。

## 为什么仍是"暂定"

本机证据不足以定案，缺少：

1. **Linux 1C1G 目标机**上的对比：`fsync(2)` 在 macOS 上不刷新磁盘缓存（本机 fsync p50 0.023 ms，而 F_FULLFSYNC p50 3.0 ms，见 `benchmarks/results/2026-11-25/fsync.json`），Linux 上 FULL 写的成本与本机完全不同；单核下驱动 CPU 开销占比也会更高。
2. **整机 HTTP 负载下**两驱动的 P99、CPU 与 RSS 对比（需临时把 `adapters/sqlite` 切到 mattn 的分支构建；本次未做，以免修改主代码）。
3. CGO 静态 Linux 构建（musl/zig）的可行性与体积。

在以上完成之前保持 modernc。若 1C1G 上点查/混合负载被证明受驱动 CPU 限制且 CGO 构建链可接受，再考虑切换；驱动差异仍被限制在 `adapters/sqlite`（DSN 构造与错误码判断）。
