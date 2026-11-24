import assert from 'node:assert/strict';
import { test } from 'node:test';
import { GoStackError, RevisionConflictError, memoryStorage } from '../src/index.js';
import { LOGIN, res, setup } from './helpers.js';

const SAVE = { slot: 'main', revision: 4, schema_version: 1, checksum: 'x', payload: { a: 1 }, updated_at: 1 };
const errBody = (code: string, details?: object) => ({ error: { code, message: code, details }, request_id: 'req_1' });

test('GET retries on 503 and honors Retry-After', async () => {
  const { client, calls, sleeps } = setup([
    res(200, LOGIN),
    res(503, errBody('overloaded'), { 'Retry-After': '2' }),
    res(429, errBody('rate_limited'), { 'retry-after': '1' }),
    res(200, { balances: { coin: 5 } }),
  ]);
  await client.auth.loginGuest();
  const w = await client.wallet.get();
  assert.deepEqual(w.balances, { coin: 5 });
  assert.equal(calls.length, 4);
  assert.deepEqual(sleeps, [2000, 1000]);
});

test('backoff uses full jitter within exponential cap', async () => {
  const { client, sleeps } = setup([res(200, LOGIN), new Error('down'), new Error('down'), res(200, { balances: {} })], {
    retry: { baseDelayMs: 100, maxDelayMs: 1000 },
  });
  await client.auth.loginGuest();
  await client.wallet.get();
  assert.deepEqual(sleeps, [50, 100]); // random()=0.5 * min(1000, 100*2^(n-1))
});

test('gives up after maxAttempts with the last error', async () => {
  const { client, calls } = setup([res(200, LOGIN)], { retry: { maxAttempts: 2 } }, res(500, errBody('internal')));
  await client.auth.loginGuest();
  await assert.rejects(client.wallet.get(), (e) => e instanceof GoStackError && e.code === 'internal' && e.status === 500 && e.requestId === 'req_1');
  assert.equal(calls.length, 3); // 1 login + 2 attempts
});

test('Retry-After above cap fails immediately', async () => {
  const { client, calls } = setup([res(200, LOGIN), res(429, errBody('rate_limited'), { 'retry-after': '600' })]);
  await client.auth.loginGuest();
  await assert.rejects(client.wallet.get(), (e) => e instanceof GoStackError && e.code === 'rate_limited');
  assert.equal(calls.length, 2);
});

test('never retries other 4xx, even for GET', async () => {
  for (const status of [400, 403, 409]) {
    const { client, calls } = setup([res(200, LOGIN), res(status, errBody('x'))]);
    await client.auth.loginGuest();
    await assert.rejects(client.wallet.get());
    assert.equal(calls.length, 2, `status ${status}`);
  }
});

test('mutation without idempotency key is not retried (guest creation, logout)', async () => {
  const a = setup([res(500, errBody('internal'))]);
  await assert.rejects(a.client.auth.loginGuest());
  assert.equal(a.calls.length, 1);
  const b = setup([res(200, LOGIN), res(500, errBody('internal'))]);
  await b.client.auth.loginGuest();
  await assert.rejects(b.client.auth.logout());
  assert.equal(b.calls.length, 2);
});

test('idempotency key is generated per logical call and reused across retries', async () => {
  const { client, calls } = setup([
    res(200, LOGIN),
    new Error('timeout'),
    res(503, errBody('overloaded')),
    res(200, SAVE),
    res(200, SAVE),
  ]);
  await client.auth.loginGuest();
  await client.saves.put('main', { a: 1 }, { expectedRevision: 3 });
  const keys = calls.slice(1, 4).map((c) => c.headers['Idempotency-Key']);
  assert.ok(keys[0] && keys[0].length >= 1 && keys[0].length <= 128);
  assert.deepEqual(keys, [keys[0], keys[0], keys[0]]);
  await client.saves.put('main', { a: 2 }, { expectedRevision: 4 });
  assert.ok(calls[4]!.headers['Idempotency-Key'] !== keys[0]);
  assert.deepEqual(JSON.parse(calls[1]!.body!), { expected_revision: 3, schema_version: 1, payload: { a: 1 } });
  assert.equal(calls[1]!.method, 'PUT');
  assert.equal(calls[1]!.url, 'https://api.test/v1/games/demo/saves/main');
  assert.equal(calls[1]!.headers['Authorization'], 'Bearer tok_1');
});

test('explicit idempotency key is honored for claim and submit', async () => {
  const { client, calls } = setup([
    res(200, LOGIN),
    res(200, { claim_id: 'rcl_1', grants: { coin: 100 }, balances: { coin: 100 }, config_revision: 3 }),
    res(200, { status: 'accepted', best: 9, season: 's' }),
  ]);
  await client.auth.loginGuest();
  const c = await client.rewards.claim('daily_login', { idempotencyKey: 'k1' });
  assert.equal(c.claimId, 'rcl_1');
  assert.equal(c.configRevision, 3);
  assert.equal(calls[1]!.headers['Idempotency-Key'], 'k1');
  assert.equal(calls[1]!.url, 'https://api.test/v1/games/demo/rewards/daily_login/claim');
  const s = await client.boards.submit('main', 9);
  assert.equal(s.status, 'accepted');
  assert.deepEqual(JSON.parse(calls[2]!.body!), { score: 9 });
  assert.ok(calls[2]!.headers['Idempotency-Key']);
});

test('RevisionConflictError carries currentRevision and is not retried', async () => {
  const { client, calls } = setup([res(200, LOGIN), res(409, errBody('revision_conflict', { current_revision: 7 }))]);
  await client.auth.loginGuest();
  await assert.rejects(client.saves.put('main', {}, { expectedRevision: 3 }), (e) => {
    return e instanceof RevisionConflictError && e instanceof GoStackError && e.currentRevision === 7 && e.status === 409 && e.requestId === 'req_1';
  });
  assert.equal(calls.length, 2);
});

test('saves.get returns null on 404 and maps fields', async () => {
  const { client } = setup([res(200, LOGIN), res(404, errBody('not_found')), res(200, SAVE)]);
  await client.auth.loginGuest();
  assert.equal(await client.saves.get('main'), null);
  const s = await client.saves.get<{ a: number }>('main');
  assert.equal(s?.revision, 4);
  assert.equal(s?.schemaVersion, 1);
  assert.equal(s?.payload.a, 1);
});

test('guest credentials are persisted, scoped by game, and reused', async () => {
  const storage = memoryStorage();
  const a = setup([res(200, LOGIN)], { storage });
  const s = await a.client.auth.loginGuest();
  assert.equal(s.created, true);
  assert.equal(a.calls[0]!.body, '{}');
  const stored = await storage.get('gostack:demo:guest');
  assert.deepEqual(JSON.parse(stored!), { guest_id: 'gst_1', guest_secret: 'sec_1' });
  assert.equal(await storage.get('gostack:other:guest'), null);

  const b = setup([res(200, { ...LOGIN, guest_secret: null, created: false })], { storage });
  const s2 = await b.client.auth.loginGuest();
  assert.equal(s2.created, false);
  assert.deepEqual(JSON.parse(b.calls[0]!.body!), { guest_id: 'gst_1', guest_secret: 'sec_1' });
  assert.deepEqual(JSON.parse((await storage.get('gostack:demo:guest'))!), { guest_id: 'gst_1', guest_secret: 'sec_1' });
});

test('login with stored credentials is retried on network failure', async () => {
  const storage = memoryStorage();
  await storage.set('gostack:demo:guest', JSON.stringify({ guest_id: 'gst_1', guest_secret: 's' }));
  const { client, calls } = setup([new Error('x'), res(200, { ...LOGIN, guest_secret: null })], { storage });
  await client.auth.loginGuest();
  assert.equal(calls.length, 2);
});

test('authed call without session auto-logs-in from stored creds, else throws not_logged_in', async () => {
  const none = setup();
  await assert.rejects(none.client.wallet.get(), (e) => e instanceof GoStackError && e.code === 'not_logged_in');
  assert.equal(none.calls.length, 0);
  const storage = memoryStorage();
  await storage.set('gostack:demo:guest', JSON.stringify({ guest_id: 'g', guest_secret: 's' }));
  const s = setup([res(200, LOGIN), res(200, { balances: { coin: 1 } })], { storage });
  assert.deepEqual((await s.client.wallet.get()).balances, { coin: 1 });
});

test('401 triggers one re-login and replay with the same idempotency key', async () => {
  const { client, calls } = setup([
    res(200, LOGIN),
    res(401, errBody('unauthorized')),
    res(200, { ...LOGIN, access_token: 'tok_2', guest_secret: null }),
    res(200, SAVE),
  ]);
  await client.auth.loginGuest();
  await client.saves.put('main', {}, { expectedRevision: 0 });
  assert.equal(calls.length, 4);
  assert.equal(calls[3]!.headers['Authorization'], 'Bearer tok_2');
  assert.equal(calls[3]!.headers['Idempotency-Key'], calls[1]!.headers['Idempotency-Key']);
});

test('logout revokes the session and clears the token', async () => {
  const { client, calls } = setup([res(200, LOGIN), res(204, '')]);
  await client.auth.loginGuest();
  await client.auth.logout();
  assert.equal(calls[1]!.url, 'https://api.test/v1/games/demo/auth/logout');
  assert.equal(calls[1]!.headers['Authorization'], 'Bearer tok_1');
});

test('boards.get builds query and maps entries; me()', async () => {
  const { client, calls } = setup([
    res(200, { board: 'main', season: 's', entries: [{ rank: 1, player_id: 'plr_1', score: 9, achieved_at: 5 }], next_cursor: null }),
    res(200, LOGIN),
    res(200, { player_id: 'plr_1', game_id: 'demo', created_at: 1, identities: ['guest'] }),
  ]);
  const b = await client.boards.get('main', { limit: 10, cursor: 'a b' });
  assert.equal(calls[0]!.url, 'https://api.test/v1/games/demo/boards/main?limit=10&cursor=a%20b');
  assert.equal(calls[0]!.headers['Authorization'], undefined);
  assert.equal(b.entries[0]?.playerId, 'plr_1');
  assert.equal(b.me, null);
  await client.auth.loginGuest();
  assert.deepEqual((await client.auth.me()).identities, ['guest']);
});

test('config: ETag revalidation, cache, offline fallback, defaults', async () => {
  const storage = memoryStorage();
  const a = setup(
    [res(200, { revision: 3, data: { x: 1 } }, { ETag: '"3"' }), res(304, ''), new Error('offline')],
    { storage, retry: { maxAttempts: 1 }, defaults: { x: 0, y: 2 } },
  );
  const first = await a.client.config.get();
  assert.deepEqual(first, { revision: 3, data: { x: 1, y: 2 }, source: 'network' });
  assert.equal(a.calls[0]!.url, 'https://api.test/v1/games/demo/config?client_version=1.0.0&platform=web');
  assert.equal(a.calls[0]!.headers['If-None-Match'], undefined);
  const second = await a.client.config.get();
  assert.equal(a.calls[1]!.headers['If-None-Match'], '"3"');
  assert.deepEqual(second, { revision: 3, data: { x: 1, y: 2 }, source: 'network' });
  const third = await a.client.config.get();
  assert.equal(third.source, 'cache');
  assert.equal(third.revision, 3);

  const fresh = setup([new Error('offline')], { retry: { maxAttempts: 1 } });
  const d = await fresh.client.config.get({ defaults: { z: 9 } });
  assert.deepEqual(d, { revision: 0, data: { z: 9 }, source: 'default' });
  const bad = setup([res(500, errBody('internal'))], { retry: { maxAttempts: 1 } });
  assert.equal((await bad.client.config.get()).source, 'default');
});

test('close() cancels pending retry sleeps (real timers) and blocks further calls', async () => {
  const { client } = setup([res(200, LOGIN), new Error('x'), new Error('x'), res(200, { balances: {} })], {
    sleep: undefined,
    retry: { baseDelayMs: 60000, maxDelayMs: 60000 },
    random: () => 0.99,
  });
  await client.auth.loginGuest();
  const p = client.wallet.get();
  p.catch(() => {});
  await new Promise((r) => setTimeout(r, 10));
  client.close();
  await assert.rejects(p, (e) => e instanceof GoStackError && e.code === 'client_closed');
  await assert.rejects(client.wallet.get(), (e) => e instanceof GoStackError && e.code === 'client_closed');
});
