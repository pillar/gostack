import { GoStackError, errorFromResponse } from './errors.js';
import type { RetryOptions, Transport, TransportResponse } from './types.js';

export interface Spec {
  method: string;
  path: string;
  query?: Record<string, string | number | undefined>;
  body?: unknown;
  /** Pre-serialised JSON body (takes precedence over body). */
  rawBody?: string;
  headers?: Record<string, string>;
  idempotencyKey?: string;
  /** Mark a mutation as safe to retry even without an Idempotency-Key (e.g. login with existing credentials). */
  retry?: boolean;
}

export interface HttpDeps {
  baseURL: string;
  transport: Transport;
  timeoutMs: number;
  retry: Required<RetryOptions>;
  sleep: (ms: number) => Promise<void>;
  now: () => number;
  random: () => number;
  isClosed: () => boolean;
}

const RETRY_STATUS = [429, 500, 502, 503, 504];

export function retryAfterMs(h: Record<string, string>, now: number): number | undefined {
  const v = h['retry-after'];
  if (!v) return undefined;
  const n = Number(v);
  if (isFinite(n) && n >= 0) return n * 1000;
  const d = Date.parse(v);
  return isNaN(d) ? undefined : Math.max(0, d - now);
}

export class Http {
  constructor(private d: HttpDeps) {}

  url(spec: Spec): string {
    let u = this.d.baseURL + spec.path;
    if (spec.query) {
      const q: string[] = [];
      for (const k of Object.keys(spec.query)) {
        const v = spec.query[k];
        if (v !== undefined) q.push(encodeURIComponent(k) + '=' + encodeURIComponent(String(v)));
      }
      if (q.length) u += '?' + q.join('&');
    }
    return u;
  }

  async send(spec: Spec, token?: string): Promise<TransportResponse> {
    const { retry } = this.d;
    const idempotent = spec.method === 'GET' || !!spec.idempotencyKey || !!spec.retry;
    const max = idempotent ? Math.max(1, retry.maxAttempts) : 1;
    const headers: Record<string, string> = { Accept: 'application/json', ...spec.headers };
    let body: string | undefined;
    if (spec.rawBody !== undefined || spec.body !== undefined) {
      body = spec.rawBody ?? JSON.stringify(spec.body);
      headers['Content-Type'] = 'application/json';
    }
    if (token) headers['Authorization'] = 'Bearer ' + token;
    if (spec.idempotencyKey) headers['Idempotency-Key'] = spec.idempotencyKey;
    const url = this.url(spec);

    for (let attempt = 1; ; attempt++) {
      if (this.d.isClosed()) throw new GoStackError('client_closed', 'client is closed');
      let err: GoStackError;
      let delay: number | undefined;
      try {
        const raw = await this.d.transport.request(spec.method, url, headers, body, this.d.timeoutMs);
        const h: Record<string, string> = {};
        for (const k of Object.keys(raw.headers || {})) h[k.toLowerCase()] = String((raw.headers as Record<string, string>)[k]);
        const res: TransportResponse = { status: raw.status, headers: h, body: raw.body ?? '' };
        if (res.status < 400) return res;
        err = errorFromResponse(res.status, h, res.body);
        if (!RETRY_STATUS.includes(res.status)) throw err;
        if (res.status === 429 || res.status === 503) {
          const ra = retryAfterMs(h, this.d.now());
          if (ra !== undefined) {
            if (ra > retry.maxRetryAfterMs) throw err;
            delay = ra;
          }
        }
      } catch (e) {
        if (e instanceof GoStackError) {
          if (e.status !== 0) throw e;
          err = e;
        } else {
          err = new GoStackError('network_error', e instanceof Error ? e.message : 'network error');
        }
      }
      if (attempt >= max) throw err;
      if (delay === undefined) {
        const cap = Math.min(retry.maxDelayMs, retry.baseDelayMs * Math.pow(2, attempt - 1));
        delay = Math.floor(this.d.random() * cap); // full jitter
      }
      await this.d.sleep(delay);
    }
  }
}
