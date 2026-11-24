# ADR-0001 SQLite 驱动：暂选 modernc.org/sqlite（待基准确认）

- 状态：暂定（Provisional），2026-11-24
- 背景：PRD §4 要求比较纯 Go 与 CGO 静态构建，依据实测选择，并验证 FTS 等编译特性与许可证。
- 决定：首期使用 `modernc.org/sqlite` v1.63.0（SQLite 3.54.1，BSD-3-Clause）。
- 理由：`CGO_ENABLED=0` 即可交叉编译 linux amd64/arm64（已验证），无需 C 工具链；FTS5 可用（`gostack doctor` 显示）；DSN 支持每连接 `_pragma` 与 `_txlock=immediate`（已由 `adapters/sqlite` 测试验证每连接生效）。
- 未验证：与 `mattn/go-sqlite3`（CGO）在 1C1G 上的吞吐、P99、CPU 与 RSS 对比。M0 基准完成前本决定可推翻；驱动差异被限制在 `adapters/sqlite`（DSN 构造与错误码判断）。
