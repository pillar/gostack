# @pillar/gostack-game-sdk

Engine-agnostic TypeScript SDK for GoStack GameKit (HTTP contract: `docs/GAME_API.md`). ESM, strict TS, zero runtime dependencies. Core is ~6.7 KiB gzip (limit 30 KiB); browser, WeChat and realtime live in separate entry points and are not part of the core size.

## Browser

```ts
import { createGameClient, RedeemError, RevisionConflictError } from '@pillar/gostack-game-sdk';
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

## More APIs (all on the same client)

```ts
// Saves: history, restore, delete, merge-on-conflict
const revs = await game.saves.revisions('main', { limit: 10 });
await game.saves.restore('main', revs.revisions[1].revision, { expectedRevision: revs.currentRevision });
await game.saves.delete('main', { expectedRevision: 5 });
await game.saves.put('main', progress, { expectedRevision: 4, merge: true }); // result.merged === true when the server merged
// 409 schema_outdated -> GoStackError{ code, details.min_schema_version }

// Anti-cheat boards: run token + rule version
const run = await game.boards.startRun('main');                  // never auto-retried
await game.boards.submit('main', 900, { runToken: run.runToken, ruleVersion: 3 });

// Economy
const inv = await game.economy.inventory();                      // { currencies, items, uncataloged }
await game.economy.purchase('potion_pack');

// Config rollouts: platform / clientVersion / bucketKey (use a stable install id for anonymous bucketing)
const cfg = await game.config.get({ bucketKey: installId });     // cfg.rollouts lists matched rollout ids (when any)

// Live ops
await game.ops.announcements(); await game.ops.events();
await game.ops.mail.list(); await game.ops.mail.read(id); await game.ops.mail.claim(id); await game.ops.mail.delete(id, { discard: true });
await game.ops.checkin.get();   // null until configured
await game.ops.checkin.claim();
await game.ops.tasks.list(); await game.ops.tasks.claim('daily_wins');
try { await game.ops.redeem(code); } catch (e) { if (e instanceof RedeemError) e.reason; /* invalid_code | already_redeemed | rate_limited */ }

// Experiments: exposures are deduped per client and sent in batches of <= 50 (5 s timer, on hide, or flush())
const a = await game.experiments.assignments();
game.experiments.expose('new_shop');

// Social
await game.social.profile.get(); await game.social.profile.update({ displayName: 'Ann' });
await game.social.friends.request({ friendCode: 'K7M2QX9A' }); await game.social.friends.list();
await game.social.blocks.add(playerId); await game.social.friends.board('main');
await game.social.invites.create(); await game.social.shares.create('wechat', { level: 3 });

// Matchmaking (poll with backoff)
const t = await game.match.createTicket('duel', { rating: 1200 });
const matched = await game.match.waitForMatch(t.ticketId, { timeoutMs: 60000 });

// Commerce
const order = await game.commerce.iap.createOrder('gems100');    // pass order.appAccountToken to StoreKit
await game.commerce.iap.verify('apple', signedTransaction);
const intent = await game.commerce.ads.intent('reward_video');   // intent.token -> ad SDK custom data
```

Retry policy for the new calls: operations the server makes idempotent send one `Idempotency-Key` per logical call (reused by every retry; override with `idempotencyKey`): saves restore/delete, purchase, mail/task/check-in claim, redeem, iap order, match ticket create and all social mutations. Naturally idempotent ones are retried without a key (exposures, mail read, ticket cancel, iap verify). Not retried: `boards.startRun`, `ops.mail.delete`, `commerce.ads.intent`.

## Realtime (optional, `/realtime` entry)

Separate entry point, not in the core bundle. The token is sent as the first WebSocket message (the URL must not contain it; the constructor rejects `?token=`).

```ts
import { createRealtimeClient, realtimeURL } from '@pillar/gostack-game-sdk/realtime';
import { browserSocketFactory } from '@pillar/gostack-game-sdk/browser';   // or wxSocketFactory(wx) from /wechat

const rt = createRealtimeClient({
  url: realtimeURL('https://api.example.com', 'demo'),
  getToken: () => game.auth.accessToken(),        // called on every (re)connect
  socketFactory: browserSocketFactory(),
});
rt.on('message', (m) => { /* { type, seq, data }, in order, no duplicates */ });
rt.on('resync', () => { /* resume failed: re-fetch state over HTTP (mail, wallet, ...) */ });
rt.on('close', (c) => { /* c.fatal: no more reconnects (4003 wrong game, 4009 replaced, repeated 4001, manual close) */ });
rt.connect();
rt.send('chat', { text: 'hi' }, 'req-1');         // false unless ready; replies arrive as 'ok' / 'error' events
```

Behaviour: acks are batched (every 8 messages or 500 ms); application-level `ping` every 20 s with a 10 s pong deadline; reconnect with exponential full-jitter backoff; after `welcome` on a reconnect the client sends `resume {epoch, last_seq}` immediately and ignores business messages until `resumed`/`resume_failed` (the server replays after `resumed`); `resume_failed` emits `resync`. A sequence gap forces a reconnect. Single-instance server semantics apply (docs/api/realtime.md).

## Behaviour

- **Retries**: only GET and mutations carrying an `Idempotency-Key` (saves.put, boards.submit and rewards.claim auto-generate one per call and reuse it for every retry; pass `idempotencyKey` to control it). Login with already-stored guest credentials and analytics batches (server dedups `event_id`) are also retried. Retried: network errors/timeouts, 429, 500, 502, 503, 504. Other 4xx are never retried. Backoff is exponential with full jitter (`retry: { maxAttempts = 3, baseDelayMs = 200, maxDelayMs = 5000 }`). `Retry-After` on 429/503 is honored; if it exceeds `maxRetryAfterMs` (30 s) the error is thrown instead of waiting.
- **Errors**: `GoStackError { code, status, requestId, details }`; network failures have `status 0`, `code 'network_error'`. `RevisionConflictError extends GoStackError` adds `currentRevision`.
- **Session**: the access token is kept in memory. On 401 (or a missing/expired token) the SDK re-logs-in with the stored guest credentials once and replays the request. `auth.logout()` revokes the session but keeps guest credentials (`logout({ forgetGuest: true })` deletes them, orphaning that guest account). Guest credentials are stored under `gostack:<gameId>:guest`.
- **Config**: `config.get()` sends `If-None-Match`, caches `{etag, revision, data}` under `gostack:<gameId>:config`, and on any failure returns the cache, else `defaults`. Never throws.
- **Analytics**: queue flushes at 50 events or 5 s, batches of at most 100 events / 64 KiB, flush on lifecycle hide/show. Buffer is bounded (1 MiB by default, `analytics.maxBufferBytes`); oldest events are dropped and counted in `analytics.stats().dropped`. Events whose properties exceed 4 KiB are dropped. Analytics never throws. Saves and rewards are never queued offline.
- **Shutdown**: `game.close()` clears timers, lifecycle subscriptions and pending retry sleeps. Call `await game.analytics.flush()` first if you want a final send.
- **Testing hooks**: `sleep`, `now`, `random`, `scheduler` options are injectable.

## Scripts

`npm run build | typecheck | test | size`. `size` bundles and minifies `src/index.ts` with esbuild, gzips it, fails above 30720 bytes, and also fails if realtime/WebSocket code leaks into core (it reports the separate `realtime` entry size).

`examples/e2e.mjs` runs the full flow against a live server:
`npm run build && GOSTACK_URL=http://127.0.0.1:8080 GAME_ID=demo node examples/e2e.mjs`
(the game's config must define board `main` and reward `daily_login`).
