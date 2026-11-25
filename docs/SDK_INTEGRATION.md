# SDK 集成指南

客户端 SDK：`sdk/typescript`（npm 包名 `@pillar/gostack-game-sdk`，ESM，零运行时依赖）。API 参考与完整行为说明在 `sdk/typescript/README.md`；HTTP 契约在 `docs/openapi/gostack.yaml`。本文讲接入步骤、离线与重试规则以及引擎集成。

> **验证状态**：SDK 核心、browser/wechat 适配器与 realtime 客户端有单元测试（`sdk/typescript/test`），`examples/e2e.mjs` 在 Node 中对真实服务器跑通。**Safari iPhone 与微信小游戏真机未验证（G-A08）**。下文第 6 节的 Cocos Creator / LayaAir / Three.js 片段是**未经测试的示例**，没有在任何引擎或设备上运行过，只演示接入点。

## 1. 入口与包结构

| 入口 | 内容 | 用途 |
|---|---|---|
| `@pillar/gostack-game-sdk` | `createGameClient`、错误类型、`memoryStorage`、类型 | 引擎无关核心（约 6.7 KiB gzip，预算 30 KiB） |
| `@pillar/gostack-game-sdk/browser` | `fetchTransport`、`localStorageStorage`、`browserLifecycle`、`browserSocketFactory` | 浏览器 / H5 |
| `@pillar/gostack-game-sdk/wechat` | `wxTransport`、`wxStorage`、`wxLifecycle`、`wxSocketFactory` | 微信小游戏（传入真实 `wx` 对象） |
| `@pillar/gostack-game-sdk/realtime` | `createRealtimeClient`、`realtimeURL` | 可选 WebSocket，不进入核心包体积 |

核心通过三个接口适配平台：`Transport`（HTTP 请求，任何状态码都 resolve，只有网络错误/超时 reject）、`KeyValueStorage`（异步 get/set/remove）、`Lifecycle`（onHide/onShow）。自定义平台只需实现这三个接口。

## 2. 服务端准备

```bash
gostack game create demo --origin https://game.example.com   # 浏览器来源白名单（CORS）
gostack game config publish demo game-config.json             # 至少包含 client / boards / rewards
```

浏览器 H5 必须把页面来源加入 `--origin`（不在白名单的 Origin 收不到 CORS 头）。微信小游戏请求不带 Origin，不受 CORS 影响，但 API 域名必须加入小游戏后台的 request 合法域名（WebSocket 用 socket 合法域名）。微信登录还需在服务配置 `game.platforms.wechat.apps.<game_id>` 中配置 `app_id` 与 `app_secret_ref`。

## 3. 浏览器

```ts
import { createGameClient, RevisionConflictError } from '@pillar/gostack-game-sdk';
import { fetchTransport, localStorageStorage, browserLifecycle } from '@pillar/gostack-game-sdk/browser';

export const game = createGameClient({
  gameId: 'demo',
  baseURL: 'https://api.example.com',
  transport: fetchTransport(),
  storage: localStorageStorage(),
  lifecycle: browserLifecycle(),
  clientVersion: '1.0.0',
  platform: 'web',
  defaults: { difficulty: 'normal' },   // 配置拉取失败且无缓存时使用
});

await game.auth.loginGuest();
const cfg = await game.config.get();     // 永不抛错：network → cache → defaults
```

## 4. 微信小游戏

```ts
import { createGameClient } from '@pillar/gostack-game-sdk';
import { wxTransport, wxStorage, wxLifecycle } from '@pillar/gostack-game-sdk/wechat';

const game = createGameClient({
  gameId: 'demo', baseURL: 'https://api.example.com',
  transport: wxTransport(wx), storage: wxStorage(wx), lifecycle: wxLifecycle(wx),
  platform: 'wechat',
});
const { code } = await new Promise<{ code: string }>((ok, fail) => wx.login({ success: ok, fail }));
await game.auth.loginPlatform('wechat', code);   // 服务端 code2session；客户端不接触 openid / session_key
```

游客账号升级为微信账号：已用游客登录时调用 `game.auth.bind('wechat', code)`；若该微信身份已属于其他玩家，返回 `409 identity_conflict`，服务端不会合并或覆盖任何数据，客户端应提示用户选择（例如改用该微信账号登录）。

## 5. 实时通信入口（可选）

```ts
import { createRealtimeClient, realtimeURL } from '@pillar/gostack-game-sdk/realtime';
import { browserSocketFactory } from '@pillar/gostack-game-sdk/browser';   // 微信：wxSocketFactory(wx)

const rt = createRealtimeClient({
  url: realtimeURL('https://api.example.com', 'demo'),   // wss://.../v1/games/demo/realtime
  getToken: () => game.auth.accessToken(),             // 每次（重）连时调用；token 作为第一条消息发送
  socketFactory: browserSocketFactory(),
});
rt.on('message', (m) => { /* 有序、不重复 */ });
rt.on('resync', () => { /* 续传失败：用 HTTP 重新拉取邮件、钱包等状态 */ });
rt.connect();
```

服务端需启用 `game.modules: [realtime]`（scale 下另需 `game.realtime.bus: postgres`；关闭码 4012 表示流被重置，SDK 按普通断线重连并续传）。token 绝不能放在 URL 中（服务端 `400 token_in_url`）。协议细节：`docs/api/realtime.md`。

## 6. 引擎集成示例（未经测试）

以下片段**未在对应引擎中编译或运行**，API 名称来自各引擎公开文档的常见用法，接入前请按引擎版本核对。原则：SDK 与引擎无关，只在启动时创建一次客户端，在引擎生命周期回调里不要阻塞首帧；联网失败时单机玩法照常进行。

### 6.1 Cocos Creator 3.x（TypeScript 组件，未经测试）

```ts
import { _decorator, Component, sys } from 'cc';
import { createGameClient } from '@pillar/gostack-game-sdk';
import { fetchTransport, localStorageStorage, browserLifecycle } from '@pillar/gostack-game-sdk/browser';
import { wxTransport, wxStorage, wxLifecycle } from '@pillar/gostack-game-sdk/wechat';
const { ccclass } = _decorator;

declare const wx: any;
const isWx = sys.platform === sys.Platform.WECHAT_GAME;

export const game = createGameClient({
  gameId: 'demo', baseURL: 'https://api.example.com',
  transport: isWx ? wxTransport(wx) : fetchTransport(),
  storage: isWx ? wxStorage(wx) : localStorageStorage(),
  lifecycle: isWx ? wxLifecycle(wx) : browserLifecycle(),
  platform: isWx ? 'wechat' : 'web',
});

@ccclass('Boot')
export class Boot extends Component {
  start() {
    // 不 await：首帧不等待网络
    game.auth.loginGuest().then(() => game.config.get()).catch(() => { /* 显示离线状态 */ });
  }
  onDestroy() { game.close(); }
}
```

### 6.2 LayaAir 3.x（未经测试）

```ts
import { createGameClient } from '@pillar/gostack-game-sdk';
import { fetchTransport, localStorageStorage, browserLifecycle } from '@pillar/gostack-game-sdk/browser';

const game = createGameClient({
  gameId: 'demo', baseURL: 'https://api.example.com',
  transport: fetchTransport(), storage: localStorageStorage(), lifecycle: browserLifecycle(),
});

const { regClass } = Laya;
@regClass()
export class Main extends Laya.Script {
  onStart(): void {
    void game.auth.loginGuest().catch(() => {});
  }
  async onLevelEnd(score: number): Promise<void> {
    try { await game.boards.submit('main', score); } catch { /* 提示网络状态；成绩不会离线排队 */ }
  }
}
```

（LayaAir 发布到微信小游戏时换用 `/wechat` 入口的三个适配器。）

### 6.3 Three.js（浏览器，未经测试）

```ts
import * as THREE from 'three';
import { game } from './gostack';        // 第 3 节创建的客户端

const renderer = new THREE.WebGLRenderer();
game.analytics.track('engine_ready');
renderer.setAnimationLoop(() => { /* 渲染循环中不要 await 网络调用 */ });

async function saveProgress(progress: unknown, revision: number) {
  return game.saves.put('main', progress, { expectedRevision: revision });
}
```

## 7. 离线与重试规则

| 规则 | SDK 行为 | 服务端依据 |
|---|---|---|
| 只重试幂等操作 | GET；带 `Idempotency-Key` 的变更（saves.put / boards.submit / rewards.claim 等自动生成一个键并在重试间复用）；带已保存凭据的游客登录；埋点批次（服务端按 `event_id` 去重） | `Idempotency-Key` 同键同体重放、异体 409 |
| 不自动重试 | 首次游客注册（可能产生第二个游客）、`boards.startRun`、`commerce.ads.intent`、`ops.mail.delete` | 这些接口不支持幂等键 |
| 退避 | 指数退避 + 全抖动，默认 3 次、200ms 起、5s 封顶；429/503 遵守 `Retry-After`，超过 30s 直接抛错 | 429 `rate_limited`、503 `overloaded` 带 `Retry-After` |
| 可重试的失败 | 网络错误/超时、429、500、502、503、504；其他 4xx 不重试 | 错误码稳定（OpenAPI `ErrorCode`） |
| 会话过期 | 401 时用已保存游客凭据重新登录一次并重放请求；已保存凭据被拒（401）时不会自动新建游客 | 会话默认 24h，无 refresh token |
| 存档冲突 | 抛 `RevisionConflictError`（`currentRevision`），由游戏拉取最新版本合并后再保存，或使用 `merge: true`（需服务端设置 `Kit.SaveMerge`） | `409 revision_conflict` |
| 离线 | **存档、领奖、购买、成绩不离线排队**，失败直接抛错，游戏应显示网络状态；只有埋点进入有界缓冲（默认 1 MiB，满时丢弃最旧并计数） | 经济与存档只以服务端确认为准 |
| 配置 | `If-None-Match` + 本地缓存；任何失败回退缓存或 `defaults`，不阻塞进入游戏 | 强 ETag、304 |
| 生命周期 | 切后台时刷新埋点；`game.close()` 清理定时器与订阅 | — |

付费资产（余额、道具、IAP 权益）只来自服务端，永远不要从本地存档恢复。

## 8. 验收清单（接入方自行执行）

- 浏览器：Safari iPhone、Chrome Android 真机上完成 登录 → 读配置 → 存档冲突 → 提交成绩 → 领奖 → 切后台再回来。
- 微信小游戏：真机（非开发者工具模拟器）上完成同样流程，并验证域名白名单与 `wx.login` 绑定。
- 断网/弱网：确认单机玩法不被阻塞、联网操作有明确提示、恢复网络后埋点有界补传。

以上项目在本仓库中**均未执行**（见 `docs/STATUS.md`）。
