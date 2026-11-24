# @pillar/gostack-game-sdk

Engine-agnostic TypeScript SDK for GoStack GameKit (HTTP contract: `docs/GAME_API.md`). ESM, strict TS, zero runtime dependencies. Core is ~4.5 KiB gzip (limit 30 KiB); browser and WeChat adapters live in separate entry points and are not part of the core size.

## Browser

```ts
import { createGameClient, RevisionConflictError } from '@pillar/gostack-game-sdk';
import { fetchTransport, localStorageStorage, browserLifecycle } from '@pillar/gostack-game-sdk/browser';

const game = createGameClient({
  gameId: 'demo',
  baseURL: 'https://api.example.com',
  transport: fetchTransport(),
  storage: localStorageStorage(),
  lifecycle: browserLifecycle(),
  clientVersion: '1.0.0',
  platform: 'web',
  defaults: { difficulty: 'normal' }, // used when config cannot be fetched
});

await game.auth.loginGuest();                       // creates or reuses the stored guest identity
const save = await game.saves.get('main');           // null when no save exists
try {
  await game.saves.put('main', progress, { expectedRevision: save?.revision ?? 0 });
} catch (e) {
  if (e instanceof RevisionConflictError) { /* e.currentRevision: fetch latest and merge */ }
  else throw e;                                      // network errors are NOT queued: show network status
}
await game.boards.submit('main', 900);
const page = await game.boards.get('main', { limit: 20 });
await game.rewards.claim('daily_login');
const { balances } = await game.wallet.get();
const cfg = await game.config.get();                 // { revision, data, source: 'network'|'cache'|'default' }
game.analytics.track('level_complete', { level: 10 });
```

## WeChat mini-game

Pass the real `wx` object; the adapters only use a small structural subset, so no `@types/wechat-miniprogram` is needed.

```ts
import { createGameClient } from '@pillar/gostack-game-sdk';
import { wxTransport, wxStorage, wxLifecycle } from '@pillar/gostack-game-sdk/wechat';

const game = createGameClient({
  gameId: 'demo',
  baseURL: 'https://api.example.com', // must be in the mini-game's request domain allowlist
  transport: wxTransport(wx),
  storage: wxStorage(wx),
  lifecycle: wxLifecycle(wx),
  platform: 'wechat',
});
await game.auth.loginPlatform('wechat', (await new Promise((ok) => wx.login({ success: ok }))).code);
// or: await game.auth.loginGuest();
```

## Behaviour

- **Retries**: only GET and mutations carrying an `Idempotency-Key` (saves.put, boards.submit and rewards.claim auto-generate one per call and reuse it for every retry; pass `idempotencyKey` to control it). Login with already-stored guest credentials and analytics batches (server dedups `event_id`) are also retried. Retried: network errors/timeouts, 429, 500, 502, 503, 504. Other 4xx are never retried. Backoff is exponential with full jitter (`retry: { maxAttempts = 3, baseDelayMs = 200, maxDelayMs = 5000 }`). `Retry-After` on 429/503 is honored; if it exceeds `maxRetryAfterMs` (30 s) the error is thrown instead of waiting.
- **Errors**: `GoStackError { code, status, requestId, details }`; network failures have `status 0`, `code 'network_error'`. `RevisionConflictError extends GoStackError` adds `currentRevision`.
- **Session**: the access token is kept in memory. On 401 (or a missing/expired token) the SDK re-logs-in with the stored guest credentials once and replays the request. `auth.logout()` revokes the session but keeps guest credentials (`logout({ forgetGuest: true })` deletes them, orphaning that guest account). Guest credentials are stored under `gostack:<gameId>:guest`.
- **Config**: `config.get()` sends `If-None-Match`, caches `{etag, revision, data}` under `gostack:<gameId>:config`, and on any failure returns the cache, else `defaults`. Never throws.
- **Analytics**: queue flushes at 50 events or 5 s, batches of at most 100 events / 64 KiB, flush on lifecycle hide/show. Buffer is bounded (1 MiB by default, `analytics.maxBufferBytes`); oldest events are dropped and counted in `analytics.stats().dropped`. Events whose properties exceed 4 KiB are dropped. Analytics never throws. Saves and rewards are never queued offline.
- **Shutdown**: `game.close()` clears timers, lifecycle subscriptions and pending retry sleeps. Call `await game.analytics.flush()` first if you want a final send.
- **Testing hooks**: `sleep`, `now`, `random`, `scheduler` options are injectable.

## Scripts

`npm run build | typecheck | test | size`. `size` bundles and minifies `src/index.ts` with esbuild, gzips it and fails above 30720 bytes.

`examples/e2e.mjs` runs the full flow against a live server:
`npm run build && GOSTACK_URL=http://127.0.0.1:8080 GAME_ID=demo node examples/e2e.mjs`
(the game's config must define board `main` and reward `daily_login`).
