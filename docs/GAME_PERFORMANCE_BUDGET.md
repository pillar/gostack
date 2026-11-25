# 游戏性能预算（PRD §23）

状态：**目标环境未验证**。环境、工具与执行方法沿用 `docs/PERFORMANCE_BUDGET.md`（1C1G Linux、本地 SSD、独立压测机、预热 2 min、稳态 10 min）。本地趋势数据见 `docs/BENCHMARK_REPORT.md`（local, not acceptance）。

## 1. 数据集（`benchmarks/cmd/gsseed`，固定种子 20261125）

| 项 | 规模 |
|---|---|
| 玩家 | 100,000（每人一个 guest 身份、一个会话） |
| 榜单 | 板 `main`（desc/best/weekly），10 个周赛季 × 10 万 = 1,000,000 行；当前赛季 10 万行 |
| 存档 | 每玩家 `main` 8 KiB + `profile` 1 KiB，revision 1 |
| 远程配置 | 一个版本（含 `bench_coin` 每日奖励，per_period 1000） |
| 埋点 | 每批 20 条（写入独立 `analytics.db`） |

数据库约 1 GiB（大于 1C1G 机器可用的 page cache，这正是目标机上需要观测冷/热缓存差异的原因）。

## 2. 预算与对应场景

| PRD 场景 | 目标 | gsload 场景与请求 |
|---|---|---|
| 配置与玩家读取 | 300 RPS，P95 ≤30 ms | `config_player`：50% `GET /config`、50% `GET /me` |
| Top 100 榜单 | 200 RPS，P95 ≤20 ms | `board`：匿名 `GET /boards/main?limit=100`（不含本人名次计数） |
| 8 KiB 存档写入 | 50 RPS，P95 ≤50 ms，确认后可恢复 | `save8k`：`PUT /saves/main`，8 KiB，`expected_revision`，`Idempotency-Key`，FULL |
| 经济事务 FULL | 50 RPS，P99 ≤150 ms，无重复发奖 | `economy`：`POST /rewards/bench_coin/claim`；2% 请求重放先前的 Idempotency-Key，要求返回同一 `claim_id` 与 `Idempotent-Replayed: true`；结束后 `gsseed -verify` + `game reconcile` |
| 混合负载 | 200 RPS，读 70% / 存档 10% / 经济 5% / 埋点 15%，P99 ≤150 ms | `gamemix`：读 = config 20、me 15、8 KiB 存档读 15、Top-100 10、钱包 10；存档写 10；领取 5；埋点批 15 |
| SDK 核心 | gzip ≤30 KiB | `sdk/typescript` `npm run size`（CI 已检查，不属本工具） |
| 客户端性能、离线弱网 | 长任务 ≤50 ms、真机帧耗时 | 需真机（G-A08），不属本工具，未验证 |

"确认后可恢复"与"无重复发奖"的完整验证还依赖 `tests/fault`（进程 kill）与目标机断电测试；gsload 只验证在线语义（重放一致、账本 = 领取数 = 余额和）。

## 3. 目标环境命令

准备与启动同 `docs/PERFORMANCE_BUDGET.md` §3.1–3.3，然后在压测机：

```sh
U=http://10.0.0.5:18080
for s in config_player board save8k economy gamemix; do
  ./gsload -url $U -scenario $s -warmup 2m -duration 10m -label "1C1G target, separate load host" -out target_$s.json
done
for s in config_player board save8k economy gamemix; do
  ./gsload -url $U -scenario $s -sat -sat-warmup 30s -sat-duration 2m -out sat_$s.json
done
```

写场景之间保留 `gsload-state.json`（存档 revision 与游标），否则第一轮写会因 revision 不一致得到 409（会被计为错误，不会被掩盖）。

G-A06（埋点过载不拖垮经济事务）：在 `economy` 目标速率运行的同时，另起一个 `gsload -scenario gamemix -rate <埋点饱和速率>` 或专门的埋点洪峰，观察经济事务 P99 与 `analytics_dropped_total`；尚未执行。

## 4. 状态

| 项 | 状态 |
|---|---|
| 数据集与场景实现 | 已实现，本机运行过 |
| 本机趋势数据 | 见 `docs/BENCHMARK_REPORT.md`（local, not acceptance） |
| 1C1G 目标机验收 | **未运行，全部预算未验证，未冻结** |
| G-A06 埋点洪峰隔离 | **未运行** |
