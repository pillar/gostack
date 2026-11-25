import assert from 'node:assert/strict';
import { test } from 'node:test';
import { GoStackError, RedeemError } from '../src/index.js';
import { LOGIN, res, setup, tick } from './helpers.js';

const errBody = (code: string, details?: object) => ({ error: { code, message: code, details }, request_id: 'req_1' });
const SAVE = { slot: 'main', revision: 5, schema_version: 1, checksum: 'x', payload: { a: 1 }, updated_at: 1 };

test('saves.restore and delete reuse one Idempotency-Key across retries; merge flag is sent', async () => {
  const { client, calls } = setup([
    res(200, LOGIN),
    new Error('timeout'),
    res(200, SAVE),
    res(503, errBody('overloaded')),
    res(200, { slot: 'main', deleted: true, revision: 5 }),
    res(200, { ...SAVE, merged: true }),
  ]);
  await client.auth.loginGuest();
  const r = await client.saves.restore('main', 2, { expectedRevision: 4 });
  assert.equal(r.revision, 5);
  assert.equal(calls[1]!.method, 'POST');
  assert.equal(calls[1]!.url, 'https://api.test/v1/games/demo/saves/main/restore');
  assert.deepEqual(JSON.parse(calls[1]!.body!), { revision: 2, expected_revision: 4 });
  assert.equal(calls[1]!.headers['Idempotency-Key'], calls[2]!.headers['Idempotency-Key']);

  const d = await client.saves.delete('main', { expectedRevision: 5 });
  assert.equal(d.deleted, true);
  assert.equal(calls[3]!.method, 'DELETE');
  assert.equal(calls[3]!.url, 'https://api.test/v1/games/demo/saves/main?expected_revision=5');
  assert.equal(calls[3]!.headers['Idempotency-Key'], calls[4]!.headers['Idempotency-Key']);
  assert.notEqual(calls[3]!.headers['Idempotency-Key'], calls[1]!.headers['Idempotency-Key']);

  const m = await client.saves.put('main', { a: 1 }, { expectedRevision: 4, merge: true });
  assert.equal(m.merged, true);
  assert.equal(JSON.parse(calls[5]!.body!).merge, true);
});

test('saves.revisions maps fields; schema_outdated surfaces details', async () => {
  const { client, calls } = setup([
    res(200, LOGIN),
    res(200, { slot: 'main', current_revision: 4, revisions: [{ revision: 4, schema_version: 1, checksum: 'c', size: 12, created_at: 7 }] }),
    res(409, errBody('schema_outdated', { min_schema_version: 3 })),
  ]);
  await client.auth.loginGuest();
  const r = await client.saves.revisions('main', { limit: 5 });
  assert.equal(calls[1]!.url, 'https://api.test/v1/games/demo/saves/main/revisions?limit=5');
  assert.deepEqual(r.revisions[0], { revision: 4, schemaVersion: 1, checksum: 'c', size: 12, createdAt: 7 });
  assert.equal(r.currentRevision, 4);
  await assert.rejects(client.saves.put('main', {}, { expectedRevision: 4 }), (e) => e instanceof GoStackError && e.code === 'schema_outdated' && e.details?.['min_schema_version'] === 3);
  assert.equal(calls.length, 3); // 409 is not retried
});

test('run token flow: startRun is not retried; submit carries run_token/rule_version and reuses its key on retry', async () => {
  const { client, calls } = setup([
    res(200, LOGIN),
    res(500, errBody('internal')), // startRun fails: must not be retried
    res(201, { run_token: 'run_abc', board: 'main', started_at: 1, expires_at: 2 }),
    new Error('timeout'),
    res(200, { status: 'accepted', best: 900, season: 'all' }),
  ]);
  await client.auth.loginGuest();
  await assert.rejects(client.boards.startRun('main'));
  assert.equal(calls.length, 2);
  const run = await client.boards.startRun('main');
  assert.deepEqual(run, { runToken: 'run_abc', board: 'main', startedAt: 1, expiresAt: 2 });
  assert.equal(calls[2]!.url, 'https://api.test/v1/games/demo/boards/main/runs');
  assert.equal(calls[2]!.headers['Idempotency-Key'], undefined);

  const s = await client.boards.submit('main', 900, { runToken: run.runToken, ruleVersion: 3 });
  assert.equal(s.status, 'accepted');
  assert.deepEqual(JSON.parse(calls[3]!.body!), { score: 900, rule_version: 3, run_token: 'run_abc' });
  assert.equal(calls[3]!.body, calls[4]!.body);
  assert.ok(calls[3]!.headers['Idempotency-Key']);
  assert.equal(calls[3]!.headers['Idempotency-Key'], calls[4]!.headers['Idempotency-Key']);
});

test('run_token_used and rule_version_mismatch are surfaced without retry', async () => {
  const { client, calls } = setup([res(200, LOGIN), res(409, errBody('run_token_used')), res(409, errBody('rule_version_mismatch', { rule_version: 4 }))]);
  await client.auth.loginGuest();
  await assert.rejects(client.boards.submit('main', 1, { runToken: 'r' }), (e) => (e as GoStackError).code === 'run_token_used');
  await assert.rejects(client.boards.submit('main', 1, { ruleVersion: 3 }), (e) => (e as GoStackError).details?.['rule_version'] === 4);
  assert.equal(calls.length, 3);
});

test('config sends platform, client_version and bucket_key; reports rollouts; cache is per parameter set', async () => {
  const { client, calls } = setup([
    res(200, { revision: 3, data: { shop: true }, rollouts: ['r1'] }, { ETag: '"3-abc"' }),
    res(304, ''),
    res(200, { revision: 3, data: { shop: false } }, { ETag: '"3"' }),
  ]);
  const a = await client.config.get({ platform: 'wechat', clientVersion: '1.2.0', bucketKey: 'install 1' });
  assert.equal(calls[0]!.url, 'https://api.test/v1/games/demo/config?client_version=1.2.0&platform=wechat&bucket_key=install%201');
  assert.deepEqual(a.rollouts, ['r1']);
  const b = await client.config.get({ platform: 'wechat', clientVersion: '1.2.0', bucketKey: 'install 1' });
  assert.equal(calls[1]!.headers['If-None-Match'], '"3-abc"');
  assert.deepEqual(b.rollouts, ['r1']);
  assert.equal(b.source, 'network');
  // different parameters must not send a stale ETag
  const c = await client.config.get({ platform: 'web', bucketKey: 'install 2' });
  assert.equal(calls[2]!.headers['If-None-Match'], undefined);
  assert.equal(c.rollouts, undefined);
});

test('economy.inventory and purchase (keyed, retried with the same key)', async () => {
  const { client, calls } = setup([
    res(200, LOGIN),
    res(200, { currencies: [{ id: 'coin', name: 'Coin', balance: 5 }], items: [{ id: 'sword', balance: 1, max_stack: 1 }], uncataloged: { old: 2 } }),
    res(503, errBody('overloaded')),
    res(200, { purchase_id: 'pur_1', cost: { coin: 50 }, grants: { potion: 3 }, balances: { coin: 0 }, config_revision: 3 }),
  ]);
  await client.auth.loginGuest();
  const inv = await client.economy.inventory();
  assert.deepEqual(inv.items, [{ id: 'sword', balance: 1, maxStack: 1 }]);
  assert.deepEqual(inv.uncataloged, { old: 2 });
  const p = await client.economy.purchase('potion_pack');
  assert.equal(p.purchaseId, 'pur_1');
  assert.equal(p.configRevision, 3);
  assert.equal(calls[2]!.url, 'https://api.test/v1/games/demo/offers/potion_pack/purchase');
  assert.equal(calls[2]!.headers['Idempotency-Key'], calls[3]!.headers['Idempotency-Key']);
});

test('mail: list, read, claim (key reused on retry), delete with discard (not retried)', async () => {
  const mail = { id: 'm1', title: 't', body: 'b', grants: { coin: 5 }, created_at: 1, expires_at: 0, read: false, claimed: false, claimable: true };
  const { client, calls } = setup([
    res(200, LOGIN),
    res(200, { mail: [mail] }),
    res(204, ''),
    new Error('timeout'),
    res(200, { mail_id: 'm1', grants: { coin: 5 } }),
    res(409, errBody('unclaimed_attachments')),
    res(500, errBody('internal')),
  ]);
  await client.auth.loginGuest();
  const list = await client.ops.mail.list();
  assert.deepEqual(list[0], { id: 'm1', title: 't', body: 'b', grants: { coin: 5 }, createdAt: 1, expiresAt: 0, read: false, claimed: false, claimable: true });
  await client.ops.mail.read('m1');
  assert.equal(calls[2]!.url, 'https://api.test/v1/games/demo/mail/m1/read');
  const c = await client.ops.mail.claim('m1');
  assert.deepEqual(c, { mailId: 'm1', grants: { coin: 5 } });
  assert.equal(calls[3]!.headers['Idempotency-Key'], calls[4]!.headers['Idempotency-Key']);
  assert.ok(calls[3]!.headers['Idempotency-Key']);
  await assert.rejects(client.ops.mail.delete('m1'), (e) => (e as GoStackError).code === 'unclaimed_attachments');
  assert.equal(calls[5]!.method, 'DELETE');
  assert.equal(calls[5]!.url, 'https://api.test/v1/games/demo/mail/m1');
  await assert.rejects(client.ops.mail.delete('m1', { discard: true }));
  assert.equal(calls[6]!.url, 'https://api.test/v1/games/demo/mail/m1?discard=true');
  assert.equal(calls.length, 7); // delete never retried
});

test('redeem maps errors to RedeemError reasons and sends a keyed body', async () => {
  const { client, calls } = setup([
    res(200, LOGIN),
    res(400, errBody('invalid_code')),
    res(409, errBody('already_redeemed')),
    res(429, errBody('rate_limited'), { 'retry-after': '600' }), // above maxRetryAfter: fails fast
    res(200, { batch_id: 'b1', grants: { gem: 10 } }),
  ]);
  await client.auth.loginGuest();
  await assert.rejects(client.ops.redeem('abcd-efgh'), (e) => e instanceof RedeemError && e.reason === 'invalid_code' && e.status === 400);
  await assert.rejects(client.ops.redeem('abcd-efgh'), (e) => e instanceof RedeemError && e.reason === 'already_redeemed' && e.status === 409);
  await assert.rejects(client.ops.redeem('abcd-efgh'), (e) => e instanceof RedeemError && e.reason === 'rate_limited' && e.retryAfterMs === 600000 && e instanceof GoStackError);
  assert.deepEqual(JSON.parse(calls[1]!.body!), { code: 'abcd-efgh' });
  assert.ok(calls[1]!.headers['Idempotency-Key']);
  const ok = await client.ops.redeem('ZZZZ');
  assert.deepEqual(ok, { batchId: 'b1', grants: { gem: 10 } });
  assert.equal(calls.length, 5);
});

test('checkin get returns null before configuration; tasks list/claim map fields', async () => {
  const { client, calls } = setup([
    res(200, LOGIN),
    res(404, errBody('not_found')),
    res(200, { day: '2026-11-25', streak: 2, index: 1, grants: { coin: 20 } }),
    res(200, { tasks: [{ id: 't1', title: 'T', metric: 'wins', target: 3, period: 'daily', grants: { coin: 1 }, period_key: '2026-11-25', progress: 3, completed: true, claimed: false, claimable: true }] }),
    res(409, errBody('not_completed')),
  ]);
  await client.auth.loginGuest();
  assert.equal(await client.ops.checkin.get(), null);
  const c = await client.ops.checkin.claim();
  assert.equal(c.streak, 2);
  const t = await client.ops.tasks.list();
  assert.equal(t[0]?.periodKey, '2026-11-25');
  assert.equal(t[0]?.claimable, true);
  await assert.rejects(client.ops.tasks.claim('t1'), (e) => (e as GoStackError).code === 'not_completed');
  assert.equal(calls[4]!.url, 'https://api.test/v1/games/demo/tasks/t1/claim');
});

test('announcements pass the platform and work anonymously', async () => {
  const { client, calls } = setup([res(200, { announcements: [{ id: 'a', title: 'T', body: 'B', priority: 1, starts_at: 0, ends_at: 0, platforms: [] }] })], { platform: 'wechat' });
  const a = await client.ops.announcements();
  assert.equal(calls[0]!.url, 'https://api.test/v1/games/demo/announcements?platform=wechat');
  assert.equal(calls[0]!.headers['Authorization'], undefined);
  assert.equal(a[0]?.startsAt, 0);
});

test('experiments: exposures are deduped, batched in 50s and flushed by timer; failures keep the queue', async () => {
  const { client, calls, timers } = setup([res(200, LOGIN), res(200, { assignments: { e1: { layer: 'l', variant: 'b', params: { x: 1 } } } })], { platform: 'wechat' });
  await client.auth.loginGuest();
  const a = await client.experiments.assignments();
  assert.equal(calls[1]!.url, 'https://api.test/v1/games/demo/experiments?platform=wechat&client_version=1.0.0');
  assert.equal(a['e1']?.variant, 'b');

  client.experiments.expose('e1');
  client.experiments.expose('e1');
  client.experiments.expose('e2');
  assert.equal(client.experiments.pending(), 2);
  assert.equal(timers.pending, 1); // one flush timer for the queue
  timers.fireAll();
  await tick();
  await tick();
  assert.equal(client.experiments.pending(), 0);
  assert.deepEqual(JSON.parse(calls[2]!.body!), { experiment_ids: ['e1', 'e2'] });
  assert.equal(calls[2]!.url, 'https://api.test/v1/games/demo/experiments/exposures?platform=wechat&client_version=1.0.0');
});

test('experiments flush: single request, retry-safe, requeue on failure', async () => {
  const { client, calls, timers } = setup(
    [res(200, LOGIN), new Error('down'), new Error('down'), new Error('down'), res(204, '')],
    {},
  );
  await client.auth.loginGuest();
  for (let i = 0; i < 3; i++) client.experiments.expose('e' + (i % 2)); // e0, e1, (e0 deduped)
  assert.equal(client.experiments.pending(), 2);
  await assert.rejects(client.experiments.flush());
  assert.equal(client.experiments.pending(), 2, 'kept after failure');
  assert.equal(calls.length, 4); // login + 3 attempts (exposures POST is retryable)
  assert.deepEqual(JSON.parse(calls[1]!.body!), { experiment_ids: ['e0', 'e1'] });
  assert.equal(calls[1]!.url, 'https://api.test/v1/games/demo/experiments/exposures?platform=web&client_version=1.0.0');
  await client.experiments.flush();
  assert.equal(client.experiments.pending(), 0);
  client.experiments.expose('e0'); // still deduped
  assert.equal(client.experiments.pending(), 0);
  void timers;
});

test('experiments: 120 exposures are sent in batches of at most 50', async () => {
  const { client, calls } = setup([res(200, LOGIN)], {}, res(204, ''));
  await client.auth.loginGuest();
  for (let i = 0; i < 120; i++) client.experiments.expose('e' + i);
  await client.experiments.flush();
  const sizes = calls.filter((c) => c.url.includes('/exposures')).map((c) => JSON.parse(c.body!).experiment_ids.length);
  assert.equal(sizes.reduce((a, b) => a + b, 0), 120);
  assert.ok(sizes.every((n) => n <= 50));
  assert.equal(client.experiments.pending(), 0);
});

test('social: friend request body, keyed mutations, empty 204 bodies, friend board mapping', async () => {
  const { client, calls } = setup([
    res(200, LOGIN),
    res(201, { status: 'pending' }),
    res(204, ''),
    res(200, { board: 'score', season: 'all', entries: [{ rank: 1, player_id: 'p', display_name: 'Bob', score: 3, achieved_at: 1, me: false }] }),
    res(200, { player_id: 'plr_1', display_name: 'Ann', friend_code: 'K7M2QX9A', privacy: { searchable: true, friend_requests: 'nobody', show_on_friend_boards: false } }),
  ]);
  await client.auth.loginGuest();
  assert.deepEqual(await client.social.friends.request({ friendCode: 'K7M2QX9A' }), { status: 'pending' });
  assert.deepEqual(JSON.parse(calls[1]!.body!), { friend_code: 'K7M2QX9A' });
  assert.ok(calls[1]!.headers['Idempotency-Key']);
  await client.social.blocks.add('plr_9');
  assert.equal(calls[2]!.url, 'https://api.test/v1/games/demo/blocks/plr_9');
  const b = await client.social.friends.board('score', { season: 'all' });
  assert.equal(b.entries[0]?.displayName, 'Bob');
  const p = await client.social.profile.update({ displayName: 'Ann', privacy: { friendRequests: 'nobody' } });
  assert.deepEqual(JSON.parse(calls[4]!.body!), { display_name: 'Ann', privacy: { friend_requests: 'nobody' } });
  assert.equal(calls[4]!.method, 'PUT');
  assert.equal(p.privacy.friendRequests, 'nobody');
  assert.equal(p.friendCode, 'K7M2QX9A');
});

test('social: invites and shares map responses', async () => {
  const { client } = setup([
    res(200, LOGIN),
    res(201, { code: 'AAAA' }),
    res(200, { attributed: true, grants: {}, grants_pending: true }),
    res(200, { paid: true, grants: { coin: 5 } }),
    res(201, { share_id: 'shr_1' }),
    res(200, { share_id: 'shr_1', channel: 'wx', payload: { a: 1 }, first_open: true }),
    res(200, { share_id: 'shr_1', channel: 'wx', payload: { a: 1 }, created_at: 1, opens: 2 }),
  ]);
  await client.auth.loginGuest();
  assert.deepEqual(await client.social.invites.create(), { code: 'AAAA' });
  assert.deepEqual(await client.social.invites.accept('AAAA'), { attributed: true, grants: {}, grantsPending: true });
  assert.deepEqual(await client.social.invites.claimPending(), { paid: true, grants: { coin: 5 } });
  assert.deepEqual(await client.social.shares.create('wx', { a: 1 }), { shareId: 'shr_1' });
  assert.equal((await client.social.shares.open('shr_1')).firstOpen, true);
  assert.equal((await client.social.shares.get('shr_1')).opens, 2);
});

test('match: createTicket keyed, cancel retried, waitForMatch polls with growing jittered delays', async () => {
  const waiting = { ticket_id: 'tkt_1', queue: 'duel', status: 'waiting', expires_at: 9 };
  const matched = { ...waiting, status: 'matched', room: { room_id: 'rm_1', status: 'open', lease_until: 3, connection: null, members: [{ player_id: 'p', state: 'assigned' }] } };
  const { client, calls, sleeps } = setup([
    res(200, LOGIN),
    new Error('timeout'),
    res(201, waiting),
    res(200, waiting),
    res(200, waiting),
    res(200, waiting),
    res(200, matched),
    new Error('timeout'),
    res(204, ''),
  ]);
  await client.auth.loginGuest();
  const t = await client.match.createTicket('duel', { rating: 1200 });
  assert.equal(t.ticketId, 'tkt_1');
  assert.deepEqual(JSON.parse(calls[1]!.body!), { queue: 'duel', attributes: { rating: 1200 } });
  assert.equal(calls[1]!.headers['Idempotency-Key'], calls[2]!.headers['Idempotency-Key']);
  sleeps.length = 0;
  const seen: string[] = [];
  const m = await client.match.waitForMatch('tkt_1', { intervalMs: 100, maxIntervalMs: 200, onUpdate: (x) => seen.push(x.status) });
  assert.equal(m.status, 'matched');
  assert.equal(m.room?.roomId, 'rm_1');
  assert.equal(m.room?.members[0]?.playerId, 'p');
  assert.deepEqual(seen, ['waiting', 'waiting', 'waiting', 'matched']);
  // random()=0.5 -> factor 0.75: 100, 150, 200(capped) scaled by 0.75
  assert.deepEqual(sleeps, [75, 113, 150]);
  await client.match.cancelTicket('tkt_1');
  assert.equal(calls[calls.length - 1]!.method, 'DELETE');
});

test('match: waitForMatch times out and can be aborted', async () => {
  const waiting = { ticket_id: 'tkt_1', queue: 'duel', status: 'waiting', expires_at: 9 };
  const { client } = setup([res(200, LOGIN)], {}, res(200, waiting));
  await client.auth.loginGuest();
  await assert.rejects(client.match.waitForMatch('tkt_1', { intervalMs: 100, timeoutMs: 0 }), (e) => (e as GoStackError).code === 'timeout');
  await assert.rejects(client.match.waitForMatch('tkt_1', { isAborted: () => true }), (e) => (e as GoStackError).code === 'aborted');
});

test('commerce: createOrder keyed -> appAccountToken; verify retried; ad intent not retried', async () => {
  const { client, calls } = setup([
    res(200, LOGIN),
    res(503, errBody('overloaded')),
    res(201, { order_id: 'o1', product_id: 'gems100', app_account_token: 'uuid-1', provider_product_ids: { apple: 'com.x.gems100' }, state: 'created' }),
    new Error('timeout'),
    res(200, { order: { order_id: 'o1', provider: 'apple', product_id: 'gems100', provider_product_id: 'com.x.gems100', provider_txn_id: 't1', state: 'delivered', quantity: 1, grants: { gem: 100 }, outstanding: { gem: 100 }, player_id: 'plr_1', created_at: 1, updated_at: 2, delivered_at: 2 }, replayed: false }),
    res(500, errBody('internal')),
  ]);
  await client.auth.loginGuest();
  const o = await client.commerce.iap.createOrder('gems100');
  assert.equal(o.appAccountToken, 'uuid-1');
  assert.deepEqual(o.providerProductIds, { apple: 'com.x.gems100' });
  assert.equal(calls[1]!.headers['Idempotency-Key'], calls[2]!.headers['Idempotency-Key']);
  const v = await client.commerce.iap.verify('apple', 'eyJ.jws');
  assert.equal(v.order.deliveredAt, 2);
  assert.equal(v.order.providerTxnId, 't1');
  assert.deepEqual(JSON.parse(calls[3]!.body!), { signed_transaction: 'eyJ.jws' });
  assert.equal(calls[3]!.url, 'https://api.test/v1/games/demo/iap/apple/verify');
  await assert.rejects(client.commerce.ads.intent('reward_video'));
  assert.equal(calls[5]!.url, 'https://api.test/v1/games/demo/ads/reward_video/intent');
  assert.equal(calls.length, 6);
});

test('auth.accessToken returns the session token', async () => {
  const { client } = setup([res(200, LOGIN)]);
  await client.auth.loginGuest();
  assert.equal(await client.auth.accessToken(), 'tok_1');
});
