import { createGameClient, memoryStorage } from '../src/index.js';
import type { GameClientOptions, Transport, TransportResponse } from '../src/index.js';

export interface Call {
  method: string;
  url: string;
  headers: Record<string, string>;
  body: string | undefined;
}

export type Step = TransportResponse | Error | ((c: Call) => TransportResponse | Error);

export function res(status: number, body: unknown = {}, headers: Record<string, string> = {}): TransportResponse {
  return { status, headers, body: typeof body === 'string' ? body : JSON.stringify(body) };
}

export const LOGIN = {
  player_id: 'plr_1',
  guest_id: 'gst_1',
  guest_secret: 'sec_1',
  access_token: 'tok_1',
  expires_at: 4_000_000_000_000,
  created: true,
};

/** Scripted transport: consumes steps in order; once exhausted, repeats `fallback` (default 200 {}). */
export function fakeTransport(steps: Step[] = [], fallback: Step = res(200)) {
  const calls: Call[] = [];
  const q = [...steps];
  const t: Transport = {
    async request(method, url, headers, body) {
      const c = { method, url, headers, body };
      calls.push(c);
      let s = q.length ? (q.shift() as Step) : fallback;
      if (typeof s === 'function') s = s(c);
      if (s instanceof Error) throw s;
      return s;
    },
  };
  return { t, calls };
}

export class FakeTimers {
  private n = 1;
  readonly map = new Map<number, () => void>();
  set(fn: () => void, _ms: number): number {
    const id = this.n++;
    this.map.set(id, fn);
    return id;
  }
  clear(h: unknown): void {
    this.map.delete(h as number);
  }
  get pending(): number {
    return this.map.size;
  }
  fireAll(): void {
    const fns = [...this.map.values()];
    this.map.clear();
    fns.forEach((f) => f());
  }
}

export function setup(steps: Step[] = [], extra: Partial<GameClientOptions> = {}, fallback?: Step) {
  const ft = fakeTransport(steps, fallback);
  const sleeps: number[] = [];
  const timers = new FakeTimers();
  const storage = extra.storage ?? memoryStorage();
  const client = createGameClient({
    gameId: 'demo',
    baseURL: 'https://api.test/',
    transport: ft.t,
    storage,
    clientVersion: '1.0.0',
    sleep: async (ms) => {
      sleeps.push(ms);
    },
    random: () => 0.5,
    scheduler: timers,
    ...extra,
  });
  return { client, calls: ft.calls, sleeps, timers, storage };
}

export const tick = () => new Promise<void>((r) => setTimeout(r, 0));
