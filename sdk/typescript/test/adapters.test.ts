import assert from 'node:assert/strict';
import { test } from 'node:test';
import { browserLifecycle, fetchTransport, localStorageStorage } from '../src/browser.js';
import { wxLifecycle, wxStorage, wxTransport } from '../src/wechat.js';

test('fetchTransport maps response and lowercases headers', async () => {
  let seen: any;
  const t = fetchTransport(async (url, init) => {
    seen = { url, init };
    const h = new Map([['Retry-After', '3']]);
    return { status: 503, headers: { forEach: (cb) => h.forEach((v, k) => cb(v, k)) }, text: async () => 'oops' };
  });
  const r = await t.request('POST', 'https://x/y', { A: 'b' }, '{}', 1000);
  assert.deepEqual(r, { status: 503, headers: { 'retry-after': '3' }, body: 'oops' });
  assert.equal(seen.init.method, 'POST');
  assert.equal(seen.init.body, '{}');
  assert.ok(seen.init.signal);
});

test('fetchTransport aborts on timeout', async () => {
  const t = fetchTransport((_u, init) => new Promise((_res, rej) => init.signal!.addEventListener('abort', () => rej(new Error('aborted')))));
  await assert.rejects(t.request('GET', 'u', {}, undefined, 5));
});

test('localStorageStorage wraps Storage', async () => {
  const m = new Map<string, string>();
  const s = localStorageStorage({ getItem: (k) => m.get(k) ?? null, setItem: (k, v) => void m.set(k, v), removeItem: (k) => void m.delete(k) });
  assert.equal(await s.get('a'), null);
  await s.set('a', '1');
  assert.equal(await s.get('a'), '1');
  await s.remove('a');
  assert.equal(await s.get('a'), null);
});

test('browserLifecycle: visibility hidden/visible and pagehide, unsubscribe works', () => {
  const l = new Map<string, Set<() => void>>();
  const target = (extra: object = {}) => ({
    ...extra,
    addEventListener: (t: string, cb: () => void) => void (l.get(t) ?? l.set(t, new Set()).get(t)!).add(cb),
    removeEventListener: (t: string, cb: () => void) => void l.get(t)?.delete(cb),
  });
  const doc = target({ visibilityState: 'visible' }) as ReturnType<typeof target> & { visibilityState: string };
  const win = target();
  const lc = browserLifecycle(doc, win);
  let hides = 0;
  let shows = 0;
  const u1 = lc.onHide(() => void hides++) as () => void;
  const u2 = lc.onShow(() => void shows++) as () => void;
  const fire = (t: string) => l.get(t)?.forEach((f) => f());
  fire('visibilitychange');
  assert.deepEqual([hides, shows], [0, 1]);
  doc.visibilityState = 'hidden';
  fire('visibilitychange');
  fire('pagehide');
  assert.deepEqual([hides, shows], [2, 1]);
  u1();
  u2();
  fire('pagehide');
  fire('visibilitychange');
  assert.deepEqual([hides, shows], [2, 1]);
});

test('wxTransport maps wx.request result and failure', async () => {
  let opts: any;
  const ok = wxTransport({ request: (o) => ((opts = o), o.success({ statusCode: 200, header: { ETag: '"1"' }, data: '{"a":1}' })) });
  const r = await ok.request('PUT', 'u', { h: '1' }, '{"x":1}', 777);
  assert.deepEqual(r, { status: 200, headers: { ETag: '"1"' }, body: '{"a":1}' });
  assert.equal(opts.method, 'PUT');
  assert.equal(opts.data, '{"x":1}');
  assert.equal(opts.timeout, 777);
  assert.notEqual(opts.dataType, 'json');
  const obj = wxTransport({ request: (o) => o.success({ statusCode: 200, data: { a: 1 } }) });
  assert.equal((await obj.request('GET', 'u', {}, undefined, 1)).body, '{"a":1}');
  const bad = wxTransport({ request: (o) => o.fail({ errMsg: 'request:fail timeout' }) });
  await assert.rejects(bad.request('GET', 'u', {}, undefined, 1), (e: any) => e.code === 'network_error');
});

test('wxStorage treats missing key as null', async () => {
  const m = new Map<string, string>();
  const s = wxStorage({
    getStorage: (o) => (m.has(o.key) ? o.success({ data: m.get(o.key) }) : o.fail({ errMsg: 'fail data not found' })),
    setStorage: (o) => (m.set(o.key, o.data), o.success()),
    removeStorage: (o) => (m.delete(o.key), o.success()),
  });
  assert.equal(await s.get('k'), null);
  await s.set('k', 'v');
  assert.equal(await s.get('k'), 'v');
  await s.remove('k');
  assert.equal(await s.get('k'), null);
});

test('wxLifecycle wires onHide/onShow and off*', () => {
  const hs = new Set<() => void>();
  const ss = new Set<() => void>();
  const lc = wxLifecycle({ onHide: (c) => void hs.add(c), onShow: (c) => void ss.add(c), offHide: (c) => void hs.delete(c), offShow: (c) => void ss.delete(c) });
  const u = lc.onHide(() => {}) as () => void;
  lc.onShow(() => {});
  assert.equal(hs.size, 1);
  assert.equal(ss.size, 1);
  u();
  assert.equal(hs.size, 0);
});
