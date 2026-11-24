import type { SocketFactory } from './realtime.js';
import type { KeyValueStorage, Lifecycle, Transport, TransportResponse } from './types.js';

type FetchLike = (url: string, init: {
  method: string;
  headers: Record<string, string>;
  body?: string;
  signal?: AbortSignal;
}) => Promise<{ status: number; headers: { forEach(cb: (v: string, k: string) => void): void }; text(): Promise<string> }>;

/** fetch-based transport with AbortController timeout. */
export function fetchTransport(fetchImpl?: FetchLike): Transport {
  return {
    async request(method, url, headers, body, timeoutMs): Promise<TransportResponse> {
      const f = fetchImpl ?? ((globalThis as unknown as { fetch: FetchLike }).fetch.bind(globalThis) as FetchLike);
      const ctl = typeof AbortController !== 'undefined' ? new AbortController() : undefined;
      const t = ctl ? setTimeout(() => ctl.abort(), timeoutMs) : undefined;
      try {
        const init: Parameters<FetchLike>[1] = { method, headers };
        if (body !== undefined) init.body = body;
        if (ctl) init.signal = ctl.signal;
        const r = await f(url, init);
        const h: Record<string, string> = {};
        r.headers.forEach((v, k) => {
          h[k.toLowerCase()] = v;
        });
        return { status: r.status, headers: h, body: await r.text() };
      } finally {
        if (t !== undefined) clearTimeout(t);
      }
    },
  };
}

interface StorageLike {
  getItem(k: string): string | null;
  setItem(k: string, v: string): void;
  removeItem(k: string): void;
}

/** Async facade over window.localStorage (or an injected Storage-like). */
export function localStorageStorage(storage?: StorageLike): KeyValueStorage {
  const s = () => storage ?? (globalThis as unknown as { localStorage: StorageLike }).localStorage;
  return {
    get: async (k) => s().getItem(k),
    set: async (k, v) => s().setItem(k, v),
    remove: async (k) => s().removeItem(k),
  };
}

interface Listenable {
  addEventListener(type: string, cb: () => void): void;
  removeEventListener(type: string, cb: () => void): void;
}
interface DocLike extends Listenable {
  visibilityState?: string;
}

/**
 * Hide: visibilitychange to hidden, or pagehide. Show: visibilitychange to visible, or pageshow.
 * Analytics flushes via the normal transport; keepalive/sendBeacon are intentionally not used.
 */
export function browserLifecycle(doc?: DocLike, win?: Listenable): Lifecycle {
  const g = globalThis as unknown as { document?: DocLike; window?: Listenable };
  const d = doc ?? g.document;
  const w = win ?? g.window;
  const sub = (visible: boolean, pageEvt: string) => (cb: () => void) => {
    const onVis = () => {
      if (d && (d.visibilityState === 'visible') === visible) cb();
    };
    d?.addEventListener('visibilitychange', onVis);
    w?.addEventListener(pageEvt, cb);
    return () => {
      d?.removeEventListener('visibilitychange', onVis);
      w?.removeEventListener(pageEvt, cb);
    };
  };
  return { onHide: sub(false, 'pagehide'), onShow: sub(true, 'pageshow') };
}

interface WebSocketLike {
  send(data: string): void;
  close(code?: number, reason?: string): void;
  onopen: (() => void) | null;
  onmessage: ((ev: { data: unknown }) => void) | null;
  onclose: ((ev: { code: number; reason: string }) => void) | null;
  onerror: (() => void) | null;
}

/** Realtime socket factory over the browser (or any compatible) WebSocket constructor. */
export function browserSocketFactory(ctor?: new (url: string) => WebSocketLike): SocketFactory {
  return (url) => {
    const C = ctor ?? (globalThis as unknown as { WebSocket: new (url: string) => WebSocketLike }).WebSocket;
    const ws = new C(url);
    return {
      send: (d) => ws.send(d),
      close: (c, r) => ws.close(c, r),
      onOpen: (cb) => void (ws.onopen = () => cb()),
      onMessage: (cb) =>
        void (ws.onmessage = (ev) => {
          if (typeof ev.data === 'string') cb(ev.data); // binary frames are invalid for this protocol
        }),
      onClose: (cb) => void (ws.onclose = (ev) => cb(ev.code, ev.reason)),
      onError: (cb) => void (ws.onerror = () => cb('websocket error')),
    };
  };
}
