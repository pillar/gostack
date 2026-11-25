# 资源发布与回滚手册（PRD §22，验收 G-A07）

适用于用 `gostack assets` 把游戏静态构建（H5 / 小游戏远程资源）发布到对象存储（R2 / S3 / MinIO，开发时可用 local）并由 CDN 分发的场景。接口与数据模型见 [api/assets.md](api/assets.md)。

## 原则

- 资源按内容哈希寻址（`games/<game>/assets/<sha256>/<name>`），对象写入后**永不覆盖**，带 `Cache-Control: public, max-age=31536000, immutable`。
- 每次发布生成一个版本化 manifest（`games/<game>/manifests/<release>.json`）和该版本的入口文件（`games/<game>/releases/<release>/<entry>`），两者同样不可变。
- 只有两个 API 响应跟随"当前版本"指针并采用短缓存（`game.assets.manifest_max_age`，默认 60 s）：`GET /v1/games/{game}/assets/manifest`（ETag = 版本号）与 `GET /v1/games/{game}/assets/entry`（302）。
- 发布顺序：资源 → manifest → 入口 → 数据库事务内记录版本并切换指针。切换之前任何一步失败，线上仍是上一个版本。
- 旧版本保留（对象与数据库记录），回滚只改指针；`prune` 才会删除，且永远不删当前版本与保留版本引用的对象。

## 前置条件

1. 配置 `storage.driver: s3`（R2：`region: auto`、`path_style: true`；R2 是否接受 UNSIGNED-PAYLOAD 未实测，先用 `sign_payload: true`），密钥用 `env:` / `file:` 引用。
2. 需要 HTTP 接口时在 `game.modules` 加入 `assets`，并设置 `game.assets.public_base_url` 为 CDN（或桶的公开域名）地址，使 `base_url + key` 能访问对象。
3. CDN 回源到桶；**不要**在 CDN 上对 `/v1/games/*/assets/manifest|entry` 设置长于 `manifest_max_age` 的缓存。
4. 入口 HTML 不能用相对路径引用其它构建文件（资源键是内容哈希，不在入口目录下）；入口应先拉取 manifest，再按 `files[path].url` 加载。
5. 游戏已创建（`gostack game create <id>`），数据库已迁移到 0013（`gostack migrate up`）。

## 发布

```bash
gostack --config /etc/APP/gostack.yaml assets publish --game demo --dir ./dist --version 1.4.0
# release 20261125T101530Z-a1b2c3 published for game demo (previous: 20261120T…)
# files 812 (40213337 bytes): uploaded 37 objects (1830211 bytes), reused 777, 9.8s
gostack --config … assets verify --game demo            # 下载当前版本全部对象，校验大小与 SHA-256；有问题退出码 3
curl -sI https://api.example.com/v1/games/demo/assets/manifest   # ETag 应为新版本号
```

- 未变化的文件不会重新上传（`reused`）。
- 同一游戏同时只能有一个 publish / prune；第二个会立即失败（"another publish or prune of this game is in progress"），稍后重试即可。进程崩溃后租约最长 10 分钟后过期。
- 发布失败（网络、存储错误、构建目录在发布期间被修改）：当前版本不变；直接重跑同一命令，已上传的对象会被复用。
- 报错 `object exists with a different body`：桶里同一内容键下有不同内容（人为写入或损坏）。不要强行覆盖；用 `assets verify` 定位，确认后由运维删除该对象再重新发布。

## 回滚

```bash
gostack --config … assets list --game demo                         # * 标记当前版本
gostack --config … assets rollback --game demo --to 20261120T090000Z-0c1d2e
gostack --config … assets verify --game demo                        # 回滚后验证无缺失文件（G-A07）
```

或通过管理 API（`admin` scope 的 API key）：

```bash
curl -X POST -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json' \
  -d '{"release_id":"20261120T090000Z-0c1d2e"}' https://api.example.com/v1/admin/games/demo/assets/rollback
```

- 回滚前会检查目标版本的每个对象存在且大小一致（HEAD），缺失则拒绝（`409 release_incomplete`），指针不变；已 prune 的版本拒绝（`409 release_pruned`）。
- 向前切换（回到较新的版本）用同一命令。
- 生效时间：API 立即返回新 manifest；客户端与 CDN 最多在 `manifest_max_age` 后看到（带 `If-None-Match` 的客户端会拿到 200 新内容）。如 CDN 也缓存了这两个 API 路径，需要在 CDN 上清除这两个 URL（资源对象本身无需清除）。

## 清理旧版本

```bash
gostack --config … assets prune --game demo --keep 5          # 演练（dry run），列出将删除的版本与对象
gostack --config … assets prune --game demo --keep 5 --yes    # 执行
```

- 保留最新 5 个已发布版本**以及当前版本**（即使它更旧）；其余版本标记为 `pruned`（之后不能回滚到它们），删除没有任何保留版本引用的对象，包括失败发布遗留的对象。
- 删除失败的对象保持 `deleting` 状态并以退出码 3 报告，重跑 prune 会继续删除。
- 建议保留数不少于"可能需要回滚的版本数 + 1"，并在 prune 前确认 `assets list` 中当前版本正确。

## 事故处理

| 现象 | 处理 |
|---|---|
| 新版本上线后有问题 | `assets rollback --to <上一版本>` → `assets verify` |
| `assets verify` 报 `missing` / `size_mismatch` / `sha256_mismatch` | 对象被外部删除或改写。若是当前版本：先回滚到验证通过的版本；再用同一构建重新发布（缺失对象会重新上传；内容不符的对象需先人工删除） |
| 发布卡住 / 进程被杀 | 当前版本不受影响；等待租约过期（≤10 分钟）后重新发布 |
| manifest 接口 404 | 该游戏尚无发布版本，或 `assets` 模块未在 `game.modules` 启用 |
| manifest 接口 500 | 对象存储不可读（manifest 读取失败）；检查存储凭据与网络，已缓存的版本在进程内继续可用 |

## 验证记录（2026-11-25，本机 macOS arm64）

- `go test -race -count=1 ./modules/assets/ ./app/`（SQLite 与 PostgreSQL 17.11 各一轮）：发布 → 验证 → 部分文件变化的二次发布（未变化文件不重传）→ 回滚 → manifest 返回旧版本且验证通过 → prune 保留被引用对象；存储故障注入（资源、manifest、入口三个阶段）后当前版本不变；并发发布被串行化（第二个立即失败）；租约过期被接管的发布不能切换指针；CLI 全流程与退出码。
- `scripts/drill/assets_minio.sh`（真实 MinIO，本地从源码构建）：发布、`Cache-Control` 写入并在预签名 GET 返回、基于 HEAD 的去重、回滚、验证、prune 全部通过。
- **未验证**：Cloudflare R2 / AWS S3、真实 CDN（缓存命中、清除）、上千文件规模的发布耗时、客户端按 manifest 加载。
