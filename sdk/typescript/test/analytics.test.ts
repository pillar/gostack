import assert from 'node:assert/strict';
import { test } from 'node:test';
import { res, setup, tick } from './helpers.js';
import type { Lifecycle } from '../src/index.js';

const bodies = (calls: { url: string; body: string | undefined }[]) =>
  calls.filter((c) => c.url.endsWith('/events/batch')).map((c) => JSON.parse(c.body!).events as Record<string, any>[]);

test('flushes at 50 events', async () => {
  const { client, calls, timers } = setup([], {}, res(202, { accepted: 50, dropped: 0 }));
  for (let i = 0; i < 49; i++) client.analytics.track('e', { i });
  assert.equal(calls.length, 0);
  assert.equal(timers.pending, 1);
  client.analytics.track('e', { i: 49 });
  await client.analytics.flush();
  const b = bodies(calls);
  assert.equal(b.length, 1);
  assert.equal(b[0]!.length, 50);
  assert.equal(timers.pending, 0);
  assert.deepEqual(client.analytics.stats(), { queued: 0, queuedBytes: 0, sent: 50, dropped: 0 });
});

test('flushes on the 5s timer; event shape and ids', async () => {
  const { client, calls, timers } = setup([], { platform: 'wechat' }, res(202, { accepted: 2, dropped: 0 }));
  client.analytics.track('launch');
  client.analytics.track('level_end', { level: 1 });
  timers.fireAll();
  await client.analytics.flush();
  await tick();
  const b = bodies(calls)[0]!;
  assert.equal(b.length, 2);
  assert.equal(calls[0]!.method, 'POST');
  assert.equal(calls[0]!.url, 'https://api.test/v1/games/demo/events/batch');
  assert.equal(calls[0]!.headers['Authorization'], undefined);
  assert.ok(b[0]!['event_id'] !== b[1]!['event_id']);
  assert.equal(b[0]!['session_id'], client.sessionId);
  assert.equal(b[1]!['session_id'], client.sessionId);
  assert.equal(b[0]!['platform'], 'wechat');
  assert.equal(b[0]!['client_version'], '1.0.0');
  assert.equal(b[0]!['event_name'], 'launch');
  assert.equal(b[1]!['schema_version'], 1);
  assert.equal(typeof b[1]!['client_time'], 'number');
  assert.deepEqual(b[1]!['properties'], { level: 1 });
});

test('batches are at most 100 events', async () => {
  const { client, calls } = setup([], { analytics: { flushAt: 100000 } }, res(202, {}));
  for (let i = 0; i < 250; i++) client.analytics.track('e', { i });
  await client.analytics.flush();
  assert.deepEqual(bodies(calls).map((b) => b.length), [100, 100, 50]);
});

test('batch body never exceeds 64 KiB', async () => {
  const { client, calls } = setup([], { analytics: { flushAt: 100000, maxBufferBytes: 10_000_000 } }, res(202, {}));
  const big = 'é'.repeat(1500); // 3000 bytes utf-8
  for (let i = 0; i < 80; i++) client.analytics.track('e', { big });
  await client.analytics.flush();
  const sizes = calls.map((c) => new TextEncoder().encode(c.body!).length);
  assert.ok(sizes.length > 1);
  assert.ok(sizes.every((s) => s <= 65536), JSON.stringify(sizes));
  assert.equal(bodies(calls).flat().length, 80);
});

test('oversized properties are dropped and counted, never thrown', () => {
  const { client } = setup();
  client.analytics.track('e', { big: 'x'.repeat(5000) });
  const cyc: Record<string, unknown> = {};
  cyc['self'] = cyc;
  client.analytics.track('e', cyc);
  assert.equal(client.analytics.stats().dropped, 1);
  assert.equal(client.analytics.stats().queued, 0);
});

test('offline buffer is byte-bounded, drops oldest and counts drops; failures never throw', async () => {
  const { client, calls } = setup([], { analytics: { flushAt: 100000, maxBufferBytes: 2000 }, retry: { maxAttempts: 2 } }, new Error('offline'));
  for (let i = 0; i < 30; i++) client.analytics.track('e', { i, pad: 'p'.repeat(50) });
  const before = client.analytics.stats();
  assert.ok(before.queuedBytes <= 2000);
  assert.ok(before.dropped > 0);
  assert.equal(before.queued + before.dropped, 30);
  await client.analytics.flush(); // must resolve
  assert.equal(calls.length, 2); // retried (event_id dedup makes it safe) up to maxAttempts
  const after = client.analytics.stats();
  assert.equal(after.queued, before.queued);
  assert.equal(after.sent, 0);
  // oldest were dropped: remaining ones are the newest
  const { client: c2, calls: calls2 } = setup([res(202, {})], { analytics: { flushAt: 100000, maxBufferBytes: 800 } });
  for (let i = 0; i < 10; i++) c2.analytics.track('e', { i });
  await c2.analytics.flush();
  const idx = bodies(calls2).flat().map((e) => e['properties'].i);
  assert.equal(idx[idx.length - 1], 9);
  assert.ok(idx[0] > 0);
});

test('failed batch is requeued and delivered after recovery', async () => {
  const { client, calls, timers } = setup([new Error('x'), new Error('x'), new Error('x')], { analytics: { flushAt: 100000 } }, res(202, {}));
  client.analytics.track('a');
  await client.analytics.flush();
  assert.equal(client.analytics.stats().queued, 1);
  assert.equal(timers.pending, 1); // retry scheduled
  timers.fireAll();
  await tick();
  await client.analytics.flush();
  assert.equal(client.analytics.stats().sent, 1);
  assert.equal(client.analytics.stats().queued, 0);
  assert.equal(calls.length, 4);
});

test('permanent 4xx drops the batch instead of retrying forever', async () => {
  const { client, calls } = setup([res(400, { error: { code: 'invalid_request', message: 'x' } })], { analytics: { flushAt: 100000 } });
  client.analytics.track('a');
  await client.analytics.flush();
  assert.equal(calls.length, 1);
  assert.deepEqual(client.analytics.stats(), { queued: 0, queuedBytes: 0, sent: 0, dropped: 1 });
});

test('lifecycle hide flushes best-effort; close unsubscribes and leaves no timers', async () => {
  let hide: (() => void) | undefined;
  let unsubbed = 0;
  const lifecycle: Lifecycle = {
    onHide: (cb) => {
      hide = cb;
      return () => void unsubbed++;
    },
    onShow: () => () => void unsubbed++,
  };
  const { client, calls, timers } = setup([], { lifecycle }, res(202, {}));
  client.analytics.track('a');
  assert.equal(timers.pending, 1);
  hide!();
  await client.analytics.flush();
  await tick();
  assert.equal(bodies(calls).length, 1);
  assert.equal(timers.pending, 0);
  client.analytics.track('b');
  assert.equal(timers.pending, 1);
  client.close();
  assert.equal(timers.pending, 0);
  assert.equal(unsubbed, 2);
  client.analytics.track('c'); // no-op after close
  assert.equal(timers.pending, 0);
});

test('sends bearer token when logged in', async () => {
  const { client, calls } = setup(
    [res(200, { player_id: 'p', guest_id: 'g', guest_secret: 's', access_token: 'tok', expires_at: 4e12, created: true })],
    { analytics: { flushAt: 100000 } },
    res(202, {}),
  );
  await client.auth.loginGuest();
  client.analytics.track('a');
  await client.analytics.flush();
  assert.equal(calls[1]!.headers['Authorization'], 'Bearer tok');
});
