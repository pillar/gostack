import { Analytics } from './analytics.js';
import { GoStackError } from './errors.js';
import { Http } from './http.js';
import type { Spec } from './http.js';
import { createApis } from './modules.js';
import type { Apis } from './modules.js';
import { memoryStorage, randomHex } from './util.js';
import type {
  BoardGetOptions,
  BoardPage,
  ClaimResult,
  ConfigGetOptions,
  ConfigResult,
  GameClientOptions,
  IdempotentOptions,
  KeyValueStorage,
  Me,
  PutSaveOptions,
  SaveRecord,
  ScoreResult,
  Session,
  SubmitScoreOptions,
  TransportResponse,
  Wallet,
  AnalyticsStats,
} from './types.js';

type J = Record<string, any>;
type AuthMode = 'required' | 'optional' | 'none';

export interface GameClient extends Apis {
  readonly sessionId: string;
  auth: {
    loginGuest(opts?: { rotate?: boolean }): Promise<Session>;
    loginPlatform(platform: string, code: string): Promise<Session>;
    bind(platform: string, code: string): Promise<{ playerId: string; platform: string }>;
    logout(opts?: { forgetGuest?: boolean }): Promise<void>;
    me(): Promise<Me>;
    /** A valid access token (refreshing a guest session if needed), e.g. for the realtime client's first auth message. */
    accessToken(): Promise<string>;
  };
  saves: Apis['saves'] & {
    get<T = unknown>(slot: string): Promise<SaveRecord<T> | null>;
    put<T = unknown>(slot: string, payload: T, opts: PutSaveOptions): Promise<SaveRecord<T>>;
  };
  boards: Apis['boards'] & {
    get(board: string, opts?: BoardGetOptions): Promise<BoardPage>;
    submit(board: string, score: number, opts?: SubmitScoreOptions): Promise<ScoreResult>;
  };
  wallet: { get(): Promise<Wallet> };
  rewards: { claim(reward: string, opts?: IdempotentOptions): Promise<ClaimResult> };
  config: { get<T = Record<string, unknown>>(opts?: ConfigGetOptions): Promise<ConfigResult<T>> };
  analytics: {
    track(name: string, props?: Record<string, unknown>): void;
    flush(): Promise<void>;
    stats(): AnalyticsStats;
  };
  /** Stops timers, lifecycle subscriptions and pending retry sleeps. Does not flush; call analytics.flush() first if needed. */
  close(): void;
}

const e = encodeURIComponent;
const newKey = (r: () => number) => 'gk_' + randomHex(16, r);

export function createGameClient(o: GameClientOptions): GameClient {
  const random = o.random ?? Math.random;
  const now = o.now ?? Date.now;
  const storage: KeyValueStorage = o.storage ?? memoryStorage();
  const platform = o.platform ?? 'web';
  const base = `${o.baseURL.replace(/\/+$/, '')}/v1/games/${e(o.gameId)}`;
  const guestKey = `gostack:${o.gameId}:guest`;
  const configKey = `gostack:${o.gameId}:config`;
  let closed = false;
  const sleepers = new Set<{ t: ReturnType<typeof setTimeout>; rej: (e: unknown) => void }>();

  const defaultSleep = (ms: number) =>
    new Promise<void>((res, rej) => {
      const s = {
        t: setTimeout(() => {
          sleepers.delete(s);
          res();
        }, ms),
        rej,
      };
      sleepers.add(s);
    });

  const http = new Http({
    baseURL: base,
    transport: o.transport,
    timeoutMs: o.timeoutMs ?? 10000,
    retry: {
      maxAttempts: o.retry?.maxAttempts ?? 3,
      baseDelayMs: o.retry?.baseDelayMs ?? 200,
      maxDelayMs: o.retry?.maxDelayMs ?? 5000,
      maxRetryAfterMs: o.retry?.maxRetryAfterMs ?? 30000,
    },
    sleep: o.sleep ?? defaultSleep,
    now,
    random,
    isClosed: () => closed,
  });

  // ---- session ----
  let token = '';
  let expiresAt = 0;
  let via: 'guest' | 'platform' | '' = '';
  let refreshing: Promise<Session> | null = null;

  const safe = async <T>(f: () => Promise<T>): Promise<T | undefined> => {
    try {
      return await f();
    } catch {
      return undefined;
    }
  };
  const parse = (t: string): J => {
    try {
      return JSON.parse(t) as J;
    } catch {
      throw new GoStackError('invalid_response', 'response is not valid JSON');
    }
  };
  const readGuest = async (): Promise<{ guest_id: string; guest_secret: string } | null> => {
    const raw = await safe(() => storage.get(guestKey));
    if (!raw) return null;
    try {
      const j = JSON.parse(raw);
      return j && j.guest_id && j.guest_secret ? j : null;
    } catch {
      return null;
    }
  };

  const setSession = (j: J, v: 'guest' | 'platform'): Session => {
    token = j.access_token;
    expiresAt = j.expires_at;
    via = v;
    return {
      playerId: j.player_id,
      guestId: j.guest_id ?? null,
      accessToken: j.access_token,
      expiresAt: j.expires_at,
      created: !!j.created,
    };
  };

  const loginGuest = (opts?: { rotate?: boolean }): Promise<Session> => {
    if (refreshing) return refreshing;
    const p = (async () => {
      const stored = await readGuest();
      const body: J = stored ? { guest_id: stored.guest_id, guest_secret: stored.guest_secret } : {};
      if (stored && opts?.rotate) body['rotate'] = true;
      // Logging in with existing credentials is idempotent, so it may be retried; creating a guest is not.
      const res = await http.send({ method: 'POST', path: '/auth/guest', body, retry: !!stored });
      const j = parse(res.body);
      if (j.guest_secret) {
        await safe(() => storage.set(guestKey, JSON.stringify({ guest_id: j.guest_id, guest_secret: j.guest_secret })));
      }
      return setSession(j, 'guest');
    })();
    refreshing = p;
    const clear = () => {
      refreshing = null;
    };
    p.then(clear, clear);
    return p;
  };

  const ensureSession = async (): Promise<void> => {
    if (token && expiresAt - 30000 > now()) return;
    if (via !== 'platform' && (await readGuest())) {
      await loginGuest();
      return;
    }
    if (token && expiresAt > now()) return;
    throw new GoStackError('not_logged_in', 'no active session; call auth.loginGuest() first', 0);
  };

  const callRes = async (spec: Spec, auth: AuthMode = 'required', refresh = true): Promise<TransportResponse> => {
    if (auth === 'required') await ensureSession();
    const useToken = auth !== 'none' && token && (auth === 'required' || expiresAt > now());
    try {
      return await http.send(spec, useToken ? token : undefined);
    } catch (err) {
      if (!(err instanceof GoStackError) || err.status !== 401 || auth === 'none') throw err;
      if (auth === 'optional') return await http.send(spec); // stale token: retry anonymously
      if (!refresh || via === 'platform' || !(await readGuest())) throw err;
      await loginGuest(); // 401 means rejected before processing, so replaying is safe
      return await http.send(spec, token);
    }
  };
  const call = async (spec: Spec, auth: AuthMode = 'required', refresh = true): Promise<string> => (await callRes(spec, auth, refresh)).body;

  // ---- analytics ----
  const scheduler = o.scheduler ?? {
    set: (fn: () => void, ms: number) => {
      const t = setTimeout(fn, ms) as unknown as { unref?: () => void };
      if (t && typeof t.unref === 'function') t.unref();
      return t;
    },
    clear: (h: unknown) => clearTimeout(h as ReturnType<typeof setTimeout>),
  };
  const sessionId = randomHex(16, random);
  const analytics = new Analytics({
    opts: o.analytics ?? {},
    sessionId,
    clientVersion: o.clientVersion,
    platform,
    scheduler,
    now,
    random,
    post: async (body) => {
      // event_id dedup on the server makes replays safe, so this POST may be retried.
      await call({ method: 'POST', path: '/events/batch', rawBody: body, retry: true }, 'optional');
    },
  });

  const apis = createApis({
    call,
    parse,
    key: () => newKey(random),
    sleep: o.sleep ?? defaultSleep,
    now,
    random,
    scheduler,
    platform,
    clientVersion: o.clientVersion,
  });

  const unsubs: Array<() => void> = [];
  if (o.lifecycle) {
    const a = o.lifecycle.onHide(() => {
      void analytics.flush();
      void apis.experiments.flush().catch(() => undefined);
    });
    const b = o.lifecycle.onShow(() => void analytics.flush());
    if (a) unsubs.push(a);
    if (b) unsubs.push(b);
  }

  // ---- config ----
  const readCache = async (params: string): Promise<{ etag: string; revision: number; data: J; rollouts: string[] } | null> => {
    const raw = await safe(() => storage.get(configKey));
    if (!raw) return null;
    try {
      const j = JSON.parse(raw);
      if (!(j && typeof j.revision === 'number' && j.data && typeof j.data === 'object' && j.params === params)) return null;
      return { ...j, rollouts: Array.isArray(j.rollouts) ? j.rollouts : [] };
    } catch {
      return null;
    }
  };

  return {
    ...apis,
    sessionId,
    auth: {
      loginGuest,
      async loginPlatform(p, code) {
        const res = await http.send({ method: 'POST', path: '/auth/platform', body: { platform: p, code } });
        return setSession(parse(res.body), 'platform');
      },
      async bind(p, code) {
        const j = parse(await call({ method: 'POST', path: '/auth/bind', body: { platform: p, code } }));
        return { playerId: j.player_id, platform: j.platform };
      },
      async logout(opts) {
        const had = token;
        token = '';
        expiresAt = 0;
        via = '';
        if (had) {
          try {
            await http.send({ method: 'POST', path: '/auth/logout' }, had);
          } catch (err) {
            if (!(err instanceof GoStackError) || err.status !== 401) throw err; // already revoked
          }
        }
        if (opts?.forgetGuest) await safe(() => storage.remove(guestKey));
      },
      async me() {
        const j = parse(await call({ method: 'GET', path: '/me' }));
        return { playerId: j.player_id, gameId: j.game_id, createdAt: j.created_at, identities: j.identities ?? [] };
      },
      async accessToken() {
        await ensureSession();
        return token;
      },
    },
    saves: {
      ...apis.saves,
      async get<T>(slot: string) {
        try {
          const j = parse(await call({ method: 'GET', path: '/saves/' + e(slot) }));
          return toSave<T>(j);
        } catch (err) {
          if (err instanceof GoStackError && err.status === 404) return null;
          throw err;
        }
      },
      async put<T>(slot: string, payload: T, opts: PutSaveOptions) {
        const j = parse(
          await call({
            method: 'PUT',
            path: '/saves/' + e(slot),
            body: {
              expected_revision: opts.expectedRevision,
              schema_version: opts.schemaVersion ?? 1,
              payload,
              ...(opts.checksum ? { checksum: opts.checksum } : {}),
              ...(opts.merge ? { merge: true } : {}),
            },
            idempotencyKey: opts.idempotencyKey ?? newKey(random),
          }),
        );
        // The server echoes the payload only when it merged; otherwise what we sent is what it stored.
        return toSave<T>('payload' in j ? j : { ...j, payload });
      },
    },
    boards: {
      ...apis.boards,
      async get(board, opts) {
        const j = parse(
          await call(
            { method: 'GET', path: '/boards/' + e(board), query: { limit: opts?.limit, cursor: opts?.cursor, season: opts?.season } },
            'optional',
          ),
        );
        return {
          board: j.board,
          season: j.season,
          entries: (j.entries ?? []).map((x: J) => ({ rank: x.rank, playerId: x.player_id, score: x.score, achievedAt: x.achieved_at })),
          nextCursor: j.next_cursor ?? null,
          me: j.me ?? null,
        };
      },
      async submit(board, score, opts) {
        const j = parse(
          await call({
            method: 'POST',
            path: '/boards/' + e(board) + '/scores',
            body: {
              score,
              ...(opts?.ruleVersion !== undefined ? { rule_version: opts.ruleVersion } : {}),
              ...(opts?.runToken ? { run_token: opts.runToken } : {}),
            },
            idempotencyKey: opts?.idempotencyKey ?? newKey(random),
          }),
        );
        return { status: j.status, best: j.best, season: j.season };
      },
    },
    wallet: {
      async get() {
        return { balances: parse(await call({ method: 'GET', path: '/wallet' })).balances ?? {} };
      },
    },
    rewards: {
      async claim(reward, opts) {
        const j = parse(
          await call({
            method: 'POST',
            path: '/rewards/' + e(reward) + '/claim',
            body: {},
            idempotencyKey: opts?.idempotencyKey ?? newKey(random),
          }),
        );
        return { claimId: j.claim_id, grants: j.grants ?? {}, balances: j.balances ?? {}, configRevision: j.config_revision };
      },
    },
    config: {
      async get<T = Record<string, unknown>>(opts?: ConfigGetOptions): Promise<ConfigResult<T>> {
        const defaults: J = { ...o.defaults, ...opts?.defaults };
        const query = {
          client_version: opts?.clientVersion ?? o.clientVersion,
          platform: opts?.platform ?? platform,
          bucket_key: opts?.bucketKey,
        };
        // The cache is only valid for the request parameters that produced it (rollouts depend on them).
        const params = JSON.stringify([query.client_version, query.platform, query.bucket_key]);
        const cached = await readCache(params);
        const fallback = (): ConfigResult<T> =>
          cached
            ? { revision: cached.revision, data: { ...defaults, ...cached.data } as T, source: 'cache', ...(cached.rollouts.length ? { rollouts: cached.rollouts } : {}) }
            : { revision: 0, data: { ...defaults } as T, source: 'default' };
        try {
          const headers: Record<string, string> = {};
          if (cached?.etag) headers['If-None-Match'] = cached.etag;
          const res = await callRes({ method: 'GET', path: '/config', query, headers }, 'optional');
          if (res.status === 304) {
            const f = fallback();
            return f.source === 'cache' ? { ...f, source: 'network' } : f;
          }
          const j = parse(res.body);
          const data: J = j.data && typeof j.data === 'object' ? j.data : {};
          const rollouts: string[] = Array.isArray(j.rollouts) ? j.rollouts : [];
          const etag = res.headers['etag'] ?? `"${j.revision}"`;
          await safe(() => storage.set(configKey, JSON.stringify({ etag, revision: j.revision, data, rollouts, params })));
          return { revision: j.revision, data: { ...defaults, ...data } as T, source: 'network', ...(rollouts.length ? { rollouts } : {}) };
        } catch {
          return fallback();
        }
      },
    },
    analytics: {
      track: (n, p) => analytics.track(n, p),
      flush: () => analytics.flush(),
      stats: () => analytics.stats(),
    },
    close() {
      if (closed) return;
      closed = true;
      analytics.close();
      apis.close();
      for (const u of unsubs.splice(0)) safe(async () => u());
      for (const s of sleepers) {
        clearTimeout(s.t);
        s.rej(new GoStackError('client_closed', 'client is closed'));
      }
      sleepers.clear();
    },
  };
}

function toSave<T>(j: J): SaveRecord<T> {
  return {
    slot: j.slot,
    revision: j.revision,
    schemaVersion: j.schema_version,
    checksum: j.checksum,
    payload: j.payload as T,
    updatedAt: j.updated_at,
    ...(j.merged ? { merged: true } : {}),
  };
}
