import assert from 'node:assert/strict';
import { test } from 'node:test';
import { browserSocketFactory } from '../src/browser.js';
import { RealtimeClient, realtimeURL } from '../src/realtime.js';
import type { RealtimeSocket } from '../src/realtime.js';
import { wxSocketFactory } from '../src/wechat.js';
import { FakeTimers, tick } from './helpers.js';

class FakeSocket implements RealtimeSocket {
  sent: any[] = [];
  closed: Array<[number | undefined, string | undefined]> = [];
  private h: any = {};
  constructor(readonly url: string) {}
  send(d: string) {
    this.sent.push(JSON.parse(d));
  }
  close(c?: number, r?: string) {
    this.closed.push([c, r]);
  }
  onOpen(cb: () => void) {
    this.h.open = cb;
  }
  onMessage(cb: (d: string) => void) {
    this.h.msg = cb;
  }
  onClose(cb: (c: number, r: string) => void) {
    this.h.close = cb;
  }
  onError(cb: (m: string) => void) {
    this.h.err = cb;
  }
  // test drivers
  open() {
    this.h.open();
  }
  recv(m: object) {
    this.h.msg(JSON.stringify(m));
  }
  drop(code = 1006, reason = '') {
    this.h.close(code, reason);
  }
}

function rig(extra: Record<string, unknown> = {}) {
  const socks: FakeSocket[] = [];
  const timers = new FakeTimers();
  let tokens = 0;
  const c = new RealtimeClient({
    url: 'wss://api.test/v1/games/demo/realtime',
    getToken: () => 'tok_' + ++tokens,
    socketFactory: (url) => {
      const s = new FakeSocket(url);
      socks.push(s);
      return s;
    },
    scheduler: timers,
    random: () => 0.5,
    ...extra,
  });
  const events: Array<[string, unknown]> = [];
  for (const k of ['open', 'message', 'resync', 'error', 'ok', 'close'] as const) c.on(k, (d) => events.push([k, d]));
  const last = () => socks[socks.length - 1]!;
  const names = () => events.map((e) => e[0]);
  return { c, socks, timers, events, last, names };
}

/** Connect and authenticate the first socket. */
async function up(r: ReturnType<typeof rig>, epoch = 'e1', seq = 10) {
  r.c.connect();
  await tick();
  r.last().open();
  r.last().recv({ type: 'welcome', player_id: 'plr_1', epoch, seq });
}

test('auth is the first message and the token is never in the URL', async () => {
  const r = rig();
  r.c.connect();
  await tick();
  assert.equal(r.last().url, 'wss://api.test/v1/games/demo/realtime');
  assert.equal(r.last().sent.length, 0, 'nothing sent before open');
  r.last().open();
  assert.deepEqual(r.last().sent, [{ type: 'auth', token: 'tok_1' }]);
  r.last().recv({ type: 'welcome', player_id: 'plr_1', epoch: 'e1', seq: 10 });
  assert.equal(r.c.state, 'ready');
  assert.deepEqual(r.events[r.events.length - 1], ['open', { playerId: 'plr_1', epoch: 'e1', resumed: false }]);
  assert.throws(() => new RealtimeClient({ url: 'wss://x/realtime?token=abc', getToken: () => 't', socketFactory: () => new FakeSocket('') }));
  assert.equal(realtimeURL('https://api.test/', 'demo'), 'wss://api.test/v1/games/demo/realtime');
});

test('messages are delivered in order, duplicates dropped, acks batched', async () => {
  const r = rig({ ackEvery: 3 });
  await up(r);
  r.last().recv({ type: 'mail', seq: 11, data: { n: 1 } });
  r.last().recv({ type: 'mail', seq: 11, data: { n: 1 } }); // duplicate
  r.last().recv({ type: 'mail', seq: 12, data: { n: 2 } });
  assert.equal(r.events.filter((e) => e[0] === 'message').length, 2);
  assert.equal(r.last().sent.filter((m) => m.type === 'ack').length, 0);
  r.last().recv({ type: 'news', seq: 13, data: {} });
  assert.deepEqual(r.last().sent[r.last().sent.length - 1], { type: 'ack', seq: 13 });
  // a delayed ack for a partial batch
  r.last().recv({ type: 'news', seq: 14, data: {} });
  assert.equal(r.timers.pending >= 1, true);
  r.timers.fireAll();
  assert.deepEqual(r.last().sent[r.last().sent.length - 1], { type: 'ack', seq: 14 });
  assert.deepEqual(r.c.position, { epoch: 'e1', lastSeq: 14 });
});

test('reconnect sends resume immediately after welcome and ignores app messages until resumed', async () => {
  const r = rig();
  await up(r);
  r.last().recv({ type: 'mail', seq: 11, data: 1 });
  r.last().drop(1006);
  assert.equal(r.c.state, 'reconnecting');
  r.timers.fireAll(); // backoff timer
  await tick();
  assert.equal(r.socks.length, 2);
  const s = r.last();
  s.open();
  assert.deepEqual(s.sent, [{ type: 'auth', token: 'tok_2' }]); // fresh token per connect
  s.recv({ type: 'welcome', player_id: 'plr_1', epoch: 'e1', seq: 13 });
  assert.equal(r.c.state, 'resuming');
  assert.deepEqual(s.sent[1], { type: 'resume', epoch: 'e1', last_seq: 11 });
  const before = r.events.filter((e) => e[0] === 'message').length;
  s.recv({ type: 'mail', seq: 13, data: 'live-too-early' }); // must be ignored
  s.recv({ type: 'mail', seq: 12, data: 'too-early' });
  assert.equal(r.events.filter((e) => e[0] === 'message').length, before);
  s.recv({ type: 'resumed', last_seq: 11, epoch: 'e1', seq: 13 });
  assert.equal(r.c.state, 'ready');
  assert.deepEqual(r.events[r.events.length - 1], ['open', { playerId: 'plr_1', epoch: 'e1', resumed: true }]);
  s.recv({ type: 'mail', seq: 12, data: 'r1' });
  s.recv({ type: 'mail', seq: 13, data: 'r2' });
  const got = r.events.filter((e) => e[0] === 'message').map((e) => (e[1] as any).data);
  assert.deepEqual(got.slice(-2), ['r1', 'r2']);
  assert.equal(r.c.position.lastSeq, 13);
  assert.equal(r.names().includes('resync'), false);
});

test('resume_failed emits resync, adopts the new epoch/seq and keeps going', async () => {
  const r = rig();
  await up(r);
  r.last().recv({ type: 'mail', seq: 11, data: 1 });
  r.last().drop(1001, 'going away'); // server restart
  r.timers.fireAll();
  await tick();
  const s = r.last();
  s.open();
  s.recv({ type: 'welcome', player_id: 'plr_1', epoch: 'e2', seq: 0 });
  assert.deepEqual(s.sent[1], { type: 'resume', epoch: 'e1', last_seq: 11 });
  s.recv({ type: 'mail', seq: 1, data: 'ignored' });
  s.recv({ type: 'resume_failed', epoch: 'e2', seq: 0 });
  assert.deepEqual(r.events[r.events.length - 1], ['resync', { epoch: 'e2', seq: 0 }]);
  assert.equal(r.c.state, 'ready');
  assert.deepEqual(r.c.position, { epoch: 'e2', lastSeq: 0 });
  s.recv({ type: 'mail', seq: 1, data: 'fresh' });
  assert.equal((r.events[r.events.length - 1]![1] as any).data, 'fresh');
  assert.equal(r.events.filter((e) => e[0] === 'message' && (e[1] as any).data === 'ignored').length, 0);
});

test('sequence gap forces a reconnect instead of skipping messages', async () => {
  const r = rig();
  await up(r);
  r.last().recv({ type: 'mail', seq: 12, data: 'gap' });
  assert.deepEqual(r.last().closed[0]?.[0], 4000);
  assert.equal(r.events.filter((e) => e[0] === 'message').length, 0);
  assert.equal(r.c.position.lastSeq, 10);
});

test('heartbeat: ping on idle, dead connection closed when pong is missing', async () => {
  const r = rig();
  await up(r);
  r.timers.fireAll(); // heartbeat timer -> ping + pong deadline
  assert.deepEqual(r.last().sent[r.last().sent.length - 1], { type: 'ping' });
  r.last().recv({ type: 'pong' }); // liveness clears the deadline and re-arms the heartbeat
  assert.equal(r.last().closed.length, 0);
  r.timers.fireAll(); // next ping
  r.timers.fireAll(); // pong deadline hit
  assert.deepEqual(r.last().closed[0]?.[0], 4000);
});

test('close codes: 4009 and 4003 are fatal, 4001 gives up after repeated failures, others reconnect', async () => {
  const a = rig();
  await up(a);
  a.last().drop(4009, 'replaced');
  assert.equal(a.c.state, 'closed');
  assert.equal(a.timers.pending, 0);
  assert.deepEqual(a.events[a.events.length - 1], ['close', { code: 4009, reason: 'replaced', fatal: true }]);

  const b = rig({ maxAuthFailures: 2 });
  b.c.connect();
  await tick();
  b.last().open();
  b.last().drop(4001);
  assert.equal(b.c.state, 'reconnecting');
  b.timers.fireAll();
  await tick();
  b.last().open();
  b.last().drop(4001);
  assert.equal(b.c.state, 'closed');

  const c = rig();
  await up(c);
  c.last().drop(4008);
  assert.equal(c.c.state, 'reconnecting');
  assert.equal((c.events[c.events.length - 1]![1] as any).fatal, false);
});

test('reconnect backoff grows with full jitter and resets after a successful welcome', async () => {
  const delays: number[] = [];
  const r = rig({ scheduler: { set: (fn: () => void, ms: number) => (delays.push(ms), r.timers.set(fn, ms)), clear: (h: unknown) => r.timers.clear(h) }, reconnectBaseMs: 100, reconnectMaxMs: 400 });
  r.c.connect();
  await tick();
  delays.length = 0;
  for (let i = 0; i < 4; i++) {
    r.last().drop(1006);
    r.timers.fireAll();
    await tick();
  }
  assert.deepEqual(
    delays.filter((d) => d !== 10000 && d !== 20000),
    [50, 100, 200, 200],
  );
});

test('send: only when ready; validates type and size; manual close is final', async () => {
  const r = rig();
  assert.equal(r.c.send('chat', {}), false);
  await up(r);
  assert.equal(r.c.send('chat', { t: 'hi' }, 'r1'), true);
  assert.deepEqual(r.last().sent[r.last().sent.length - 1], { type: 'chat', id: 'r1', data: { t: 'hi' } });
  assert.throws(() => r.c.send('ping'));
  assert.throws(() => r.c.send('Bad Type'));
  assert.throws(() => r.c.send('chat', 'x'.repeat(5000)));
  r.last().recv({ type: 'ok', ref: 'r1' });
  r.last().recv({ type: 'error', code: 'unknown_type', message: 'm', ref: 'r2' });
  assert.deepEqual(r.events.filter((e) => e[0] === 'ok' || e[0] === 'error').map((e) => e[1]), [{ ref: 'r1' }, { code: 'unknown_type', message: 'm', ref: 'r2' }]);
  const s = r.last();
  r.c.close();
  assert.equal(s.closed[0]?.[0], 1000);
  assert.equal(r.c.state, 'closed');
  r.c.connect();
  await tick();
  assert.equal(r.socks.length, 1);
});

test('browser and wx socket factories adapt their sockets', () => {
  const made: any[] = [];
  class WS {
    onopen: any;
    onmessage: any;
    onclose: any;
    onerror: any;
    sent: string[] = [];
    constructor(public url: string) {
      made.push(this);
    }
    send(d: string) {
      this.sent.push(d);
    }
    close() {}
  }
  const s = browserSocketFactory(WS as any)('wss://x/realtime');
  const got: unknown[] = [];
  s.onOpen(() => got.push('open'));
  s.onMessage((d) => got.push(d));
  s.onClose((c, r) => got.push([c, r]));
  made[0].onopen();
  made[0].onmessage({ data: '{"a":1}' });
  made[0].onmessage({ data: new Uint8Array(1) }); // binary ignored
  made[0].onclose({ code: 4008, reason: 'slow' });
  assert.deepEqual(got, ['open', '{"a":1}', [4008, 'slow']]);

  const h: any = {};
  const sent: string[] = [];
  const w = wxSocketFactory({
    connectSocket: (o) => {
      assert.equal(o.url, 'wss://x/realtime');
      return {
        send: (x) => void sent.push(x.data),
        close: () => {},
        onOpen: (cb) => (h.open = cb),
        onMessage: (cb) => (h.msg = cb),
        onClose: (cb) => (h.close = cb),
        onError: (cb) => (h.err = cb),
      };
    },
  })('wss://x/realtime');
  const got2: unknown[] = [];
  w.onMessage((d) => got2.push(d));
  w.onClose((c) => got2.push(c));
  w.send('hi');
  h.msg({ data: 'a' });
  h.msg({ data: new ArrayBuffer(1) });
  h.close({ code: 4001, reason: '' });
  assert.deepEqual(sent, ['hi']);
  assert.deepEqual(got2, ['a', 4001]);
});
