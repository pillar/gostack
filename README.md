# GoStack

轻量优先的 Go 后端框架：Lite profile（SQLite WAL + systemd）、GameKit（游客/平台登录、云存档、排行榜、钱包账本、远程配置、埋点）与 TypeScript 游戏 SDK。

- API 契约：`docs/GAME_API.md`；实现与验证状态：`docs/STATUS.md`
- 开发约定与命令：`AGENTS.md`

## 快速开始

```bash
go install github.com/pillar/gostack/cmd/gostack@latest   # 或在仓库内 go build -o bin/gostack ./cmd/gostack
gostack init mygame --kit game        # go mod tidy 拉取 github.com/pillar/gostack 最新版本
# 开发框架本身时可加 --gostack-path "$PWD"，生成本地 replace
cd mygame && go mod tidy && go build -o bin/mygame .
./bin/mygame --config gostack.yaml migrate up
./bin/mygame --config gostack.yaml game create demo --origin http://localhost:5173
./bin/mygame --config gostack.yaml game config publish demo game-config.json
./bin/mygame --config gostack.yaml serve
```

客户端：见 `sdk/typescript/README.md`。

## 验证

```bash
./scripts/ci.sh
```

未实现的 profile（scale/distributed）、驱动（postgres/valkey/pgmq）和 Kit（web/agent）会被配置校验拒绝，而不是假装可用。

## License

MIT, see `LICENSE`.
