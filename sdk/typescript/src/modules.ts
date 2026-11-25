import type {
  AdIntent,
  Announcement,
  CheckinResult,
  CheckinStatus,
  DeleteSaveOptions,
  DeleteSaveResult,
  ExperimentAssignment,
  ExperimentQuery,
  Friend,
  FriendBoard,
  FriendRequests,
  FriendTarget,
  GameEvent,
  IapOrder,
  IapVerifyResult,
  InviteAcceptResult,
  InviteInfo,
  Inventory,
  MailClaimResult,
  MailItem,
  MatchTicket,
  ProfileUpdate,
  PurchaseResult,
  RedeemResult,
  RestoreSaveOptions,
  Run,
  SaveRevisions,
  ShareInfo,
  ShareOpenResult,
  SocialProfile,
  TaskClaimResult,
  TaskItem,
  WaitForMatchOptions,
} from './api-types.js';
import { GoStackError, RedeemError } from './errors.js';
import type { Spec } from './http.js';
import type { IdempotentOptions, SaveRecord, Scheduler } from './types.js';

type J = Record<string, any>;
const e = encodeURIComponent;
const cz = (j: J): any => {
  const o: J = {};
  for (const k of Object.keys(j)) o[k.replace(/_([a-z])/g, (_, c: string) => c.toUpperCase())] = j[k];
  return o;
};

export interface ApiCtx {
  call(spec: Spec, auth?: 'required' | 'optional' | 'none'): Promise<string>;
  parse(text: string): J;
  /** Fresh Idempotency-Key. */
  key(): string;
  sleep(ms: number): Promise<void>;
  now(): number;
  random(): number;
  scheduler: Scheduler;
  platform: string;
  clientVersion: string | undefined;
}

export interface Apis {
  saves: {
    revisions(slot: string, opts?: { limit?: number }): Promise<SaveRevisions>;
    restore<T = unknown>(slot: string, revision: number, opts: RestoreSaveOptions): Promise<SaveRecord<T>>;
    delete(slot: string, opts: DeleteSaveOptions): Promise<DeleteSaveResult>;
  };
  boards: {
    /** Issues a one-time run token (never retried automatically: every call issues a new token). */
    startRun(board: string): Promise<Run>;
  };
  economy: {
    inventory(): Promise<Inventory>;
    purchase(offer: string, opts?: IdempotentOptions): Promise<PurchaseResult>;
  };
  ops: {
    announcements(opts?: { platform?: string }): Promise<Announcement[]>;
    events(): Promise<GameEvent[]>;
    mail: {
      list(): Promise<MailItem[]>;
      read(id: string): Promise<void>;
      claim(id: string, opts?: IdempotentOptions): Promise<MailClaimResult>;
      /** Not retried automatically. Mail with an unclaimed attachment needs `discard: true` (409 unclaimed_attachments otherwise). */
      delete(id: string, opts?: { discard?: boolean }): Promise<void>;
    };
    checkin: {
      /** Null until the game configures check-in (404). */
      get(): Promise<CheckinStatus | null>;
      claim(opts?: IdempotentOptions): Promise<CheckinResult>;
    };
    tasks: {
      list(): Promise<TaskItem[]>;
      claim(id: string, opts?: IdempotentOptions): Promise<TaskClaimResult>;
    };
    /** Throws RedeemError (reason: invalid_code | already_redeemed | rate_limited | other). */
    redeem(code: string, opts?: IdempotentOptions): Promise<RedeemResult>;
  };
  experiments: {
    assignments(q?: ExperimentQuery): Promise<Record<string, ExperimentAssignment>>;
    /** Queues an exposure; deduped per client instance, sent in batches of up to 50. */
    expose(experimentId: string): void;
    /** Sends queued exposures now. Rejects (and keeps the queue) on failure. */
    flush(): Promise<void>;
    pending(): number;
  };
  social: {
    profile: { get(): Promise<SocialProfile>; update(p: ProfileUpdate, opts?: IdempotentOptions): Promise<SocialProfile> };
    friends: {
      list(): Promise<Friend[]>;
      request(target: FriendTarget, opts?: IdempotentOptions): Promise<{ status: 'pending' | 'accepted' }>;
      requests(): Promise<FriendRequests>;
      accept(playerId: string, opts?: IdempotentOptions): Promise<{ status: string }>;
      decline(playerId: string, opts?: IdempotentOptions): Promise<void>;
      cancel(playerId: string, opts?: IdempotentOptions): Promise<void>;
      remove(playerId: string, opts?: IdempotentOptions): Promise<void>;
      board(board: string, opts?: { season?: string }): Promise<FriendBoard>;
    };
    blocks: {
      list(): Promise<Array<{ playerId: string; at: number }>>;
      add(playerId: string, opts?: IdempotentOptions): Promise<void>;
      remove(playerId: string, opts?: IdempotentOptions): Promise<void>;
    };
    invites: {
      create(opts?: IdempotentOptions): Promise<{ code: string }>;
      list(): Promise<InviteInfo[]>;
      accept(code: string, opts?: IdempotentOptions): Promise<InviteAcceptResult>;
    };
    shares: {
      create(channel: string, payload: unknown, opts?: IdempotentOptions): Promise<{ shareId: string }>;
      open(id: string, opts?: IdempotentOptions): Promise<ShareOpenResult>;
      get(id: string): Promise<ShareInfo>;
    };
  };
  match: {
    createTicket(queue: string, attributes?: { rating?: number }, opts?: IdempotentOptions): Promise<MatchTicket>;
    getTicket(id: string): Promise<MatchTicket>;
    /** Cancelling is repeatable on the server, so this is retried. 409 already_matched / ticket_closed are errors. */
    cancelTicket(id: string): Promise<void>;
    /** Polls until the ticket leaves "waiting" (matched, expired, cancelled, ...), with growing, jittered delays. */
    waitForMatch(id: string, opts?: WaitForMatchOptions): Promise<MatchTicket>;
  };
  commerce: {
    iap: {
      /** Call before purchasing; pass the returned appAccountToken to StoreKit. */
      createOrder(productId: string, opts?: IdempotentOptions): Promise<IapOrder>;
      /** Idempotent by transaction on the server, so it is retried. */
      verify(provider: string, signedTransaction: string): Promise<IapVerifyResult>;
    };
    ads: {
      /** Not retried automatically (each call opens a new single-use token). */
      intent(placement: string): Promise<AdIntent>;
    };
  };
  /** @internal */
  close(): void;
}

const ticket = (j: J): MatchTicket => {
  const t = cz(j) as MatchTicket;
  if (j.room) t.room = { ...cz(j.room), members: (j.room.members ?? []).map(cz) };
  return t;
};

export function createApis(c: ApiCtx): Apis {
  const get = async (path: string, query?: Spec['query'], auth: 'required' | 'optional' = 'required'): Promise<J> =>
    c.parse(await c.call({ method: 'GET', path, query }, auth));
  /** Mutation that the server makes idempotent: one key per logical operation, reused by every retry. */
  const keyed = async (method: string, path: string, body: unknown, o?: IdempotentOptions, query?: Spec['query']): Promise<string> =>
    c.call({ method, path, body, query, idempotencyKey: o?.idempotencyKey ?? c.key() });
  const keyedJ = async (method: string, path: string, body: unknown, o?: IdempotentOptions, query?: Spec['query']): Promise<J> =>
    c.parse(await keyed(method, path, body, o, query));

  // ---- experiments exposure queue ----
  const seen = new Set<string>();
  let queue: string[] = [];
  let timer: unknown;
  let inflight: Promise<void> | null = null;
  let expQuery: ExperimentQuery = {};
  const eq = () => ({ platform: expQuery.platform ?? c.platform, client_version: expQuery.clientVersion ?? c.clientVersion });
  const flushExposures = (): Promise<void> => {
    if (timer !== undefined) {
      c.scheduler.clear(timer);
      timer = undefined;
    }
    if (inflight) return inflight;
    const run = async () => {
      while (queue.length) {
        const batch = queue.slice(0, 50);
        // Exposure recording is naturally idempotent on the server (first exposure wins), so retries are safe.
        await c.call({ method: 'POST', path: '/experiments/exposures', query: eq(), body: { experiment_ids: batch }, retry: true });
        queue = queue.slice(batch.length);
      }
    };
    const p = run().finally(() => {
      inflight = null;
    });
    inflight = p;
    return p;
  };

  const social = {
    async mut(method: string, path: string, body?: unknown, o?: IdempotentOptions): Promise<J> {
      const t = await keyed(method, path, body, o);
      return t ? c.parse(t) : {};
    },
  };
  const none = async (p: Promise<unknown>): Promise<void> => {
    await p;
  };

  return {
    saves: {
      async revisions(slot, opts) {
        const j = await get('/saves/' + e(slot) + '/revisions', { limit: opts?.limit });
        return {
          slot: j.slot,
          currentRevision: j.current_revision,
          revisions: (j.revisions ?? []).map((r: J) => ({ revision: r.revision, schemaVersion: r.schema_version, checksum: r.checksum, size: r.size, createdAt: r.created_at })),
        };
      },
      async restore<T>(slot: string, revision: number, opts: RestoreSaveOptions) {
        const j = await keyedJ('POST', '/saves/' + e(slot) + '/restore', { revision, expected_revision: opts.expectedRevision }, opts);
        return {
          slot: j.slot,
          revision: j.revision,
          schemaVersion: j.schema_version,
          checksum: j.checksum,
          payload: j.payload as T,
          updatedAt: j.updated_at,
        };
      },
      async delete(slot, opts) {
        return keyedJ('DELETE', '/saves/' + e(slot), undefined, opts, { expected_revision: opts.expectedRevision }) as Promise<DeleteSaveResult>;
      },
    },
    boards: {
      async startRun(board) {
        return cz(c.parse(await c.call({ method: 'POST', path: '/boards/' + e(board) + '/runs', body: {} })));
      },
    },
    economy: {
      async inventory() {
        const j = await get('/inventory');
        return {
          currencies: j.currencies ?? [],
          items: (j.items ?? []).map(cz),
          uncataloged: j.uncataloged ?? {},
        };
      },
      async purchase(offer, opts) {
        return cz(await keyedJ('POST', '/offers/' + e(offer) + '/purchase', {}, opts));
      },
    },
    ops: {
      async announcements(opts) {
        const j = await get('/announcements', { platform: opts?.platform ?? c.platform }, 'optional');
        return (j.announcements ?? []).map(cz);
      },
      async events() {
        const j = await get('/events', undefined, 'optional');
        return (j.events ?? []).map(cz);
      },
      mail: {
        async list() {
          return ((await get('/mail')).mail ?? []).map(cz);
        },
        read: (id) => none(c.call({ method: 'POST', path: '/mail/' + e(id) + '/read', retry: true })),
        async claim(id, opts) {
          return cz(await keyedJ('POST', '/mail/' + e(id) + '/claim', undefined, opts));
        },
        delete: (id, opts) => none(c.call({ method: 'DELETE', path: '/mail/' + e(id), query: opts?.discard ? { discard: 'true' } : undefined })),
      },
      checkin: {
        async get() {
          try {
            return cz(await get('/checkin'));
          } catch (err) {
            if (err instanceof GoStackError && err.status === 404) return null;
            throw err;
          }
        },
        async claim(opts) {
          return cz(await keyedJ('POST', '/checkin', undefined, opts));
        },
      },
      tasks: {
        async list() {
          return ((await get('/tasks')).tasks ?? []).map(cz);
        },
        async claim(id, opts) {
          return cz(await keyedJ('POST', '/tasks/' + e(id) + '/claim', undefined, opts));
        },
      },
      async redeem(code, opts) {
        try {
          return cz(await keyedJ('POST', '/redeem', { code }, opts));
        } catch (err) {
          throw err instanceof GoStackError ? new RedeemError(err) : err;
        }
      },
    },
    experiments: {
      async assignments(q) {
        expQuery = q ?? {};
        const j = await get('/experiments', eq());
        return j.assignments ?? {};
      },
      expose(id) {
        if (seen.has(id)) return;
        seen.add(id);
        queue.push(id);
        if (queue.length >= 50) void flushExposures().catch(() => undefined);
        else if (timer === undefined) {
          timer = c.scheduler.set(() => {
            timer = undefined;
            void flushExposures().catch(() => undefined);
          }, 5000);
        }
      },
      flush: flushExposures,
      pending: () => queue.length,
    },
    social: {
      profile: {
        async get() {
          return toProfile(await get('/social/profile'));
        },
        async update(p, opts) {
          const body: J = {};
          if (p.displayName !== undefined) body['display_name'] = p.displayName;
          if (p.privacy) {
            const v: J = {};
            if (p.privacy.searchable !== undefined) v['searchable'] = p.privacy.searchable;
            if (p.privacy.friendRequests !== undefined) v['friend_requests'] = p.privacy.friendRequests;
            if (p.privacy.showOnFriendBoards !== undefined) v['show_on_friend_boards'] = p.privacy.showOnFriendBoards;
            body['privacy'] = v;
          }
          return toProfile(await keyedJ('PUT', '/social/profile', body, opts));
        },
      },
      friends: {
        async list() {
          return ((await get('/friends')).friends ?? []).map(cz);
        },
        async request(t, o) {
          const body = t.playerId !== undefined ? { player_id: t.playerId } : { friend_code: t.friendCode };
          return { status: (await social.mut('POST', '/friends/requests', body, o))['status'] };
        },
        async requests() {
          const j = await get('/friends/requests');
          return { incoming: (j.incoming ?? []).map(cz), outgoing: (j.outgoing ?? []).map(cz) };
        },
        async accept(id, o) {
          return { status: (await social.mut('POST', '/friends/requests/' + e(id) + '/accept', undefined, o))['status'] };
        },
        decline: (id, o) => none(social.mut('POST', '/friends/requests/' + e(id) + '/decline', undefined, o)),
        cancel: (id, o) => none(social.mut('DELETE', '/friends/requests/' + e(id), undefined, o)),
        remove: (id, o) => none(social.mut('DELETE', '/friends/' + e(id), undefined, o)),
        async board(board, opts) {
          const j = await get('/friends/boards/' + e(board), { season: opts?.season });
          return { board: j.board, season: j.season, entries: (j.entries ?? []).map(cz) };
        },
      },
      blocks: {
        async list() {
          return ((await get('/blocks')).blocks ?? []).map(cz);
        },
        add: (id, o) => none(social.mut('POST', '/blocks/' + e(id), undefined, o)),
        remove: (id, o) => none(social.mut('DELETE', '/blocks/' + e(id), undefined, o)),
      },
      invites: {
        async create(o) {
          return { code: (await social.mut('POST', '/invites', undefined, o))['code'] };
        },
        async list() {
          return ((await get('/invites')).invites ?? []).map(cz);
        },
        async accept(code, o) {
          const j = await social.mut('POST', '/invites/' + e(code) + '/accept', undefined, o);
          return { attributed: !!j['attributed'], grants: j['grants'] ?? {} };
        },
      },
      shares: {
        async create(channel, payload, o) {
          return { shareId: (await social.mut('POST', '/shares', { channel, payload }, o))['share_id'] };
        },
        async open(id, o) {
          return cz(await social.mut('POST', '/shares/' + e(id) + '/open', undefined, o));
        },
        async get(id) {
          return cz(await get('/shares/' + e(id)));
        },
      },
    },
    match: {
      async createTicket(queue, attributes, o) {
        return ticket(await keyedJ('POST', '/match/tickets', { queue, attributes: attributes ?? {} }, o));
      },
      async getTicket(id) {
        return ticket(await get('/match/tickets/' + e(id)));
      },
      cancelTicket: (id) => none(c.call({ method: 'DELETE', path: '/match/tickets/' + e(id), retry: true })),
      async waitForMatch(id, o) {
        const start = c.now();
        const max = o?.maxIntervalMs ?? 5000;
        let delay = o?.intervalMs ?? 1000;
        for (;;) {
          const t = ticket(await get('/match/tickets/' + e(id)));
          o?.onUpdate?.(t);
          if (t.status !== 'waiting') return t;
          if (o?.isAborted?.()) throw new GoStackError('aborted', 'waitForMatch aborted');
          if (o?.timeoutMs !== undefined && c.now() - start + delay > o.timeoutMs) throw new GoStackError('timeout', 'match not found in time');
          await c.sleep(Math.round(delay * (0.5 + c.random() / 2)));
          delay = Math.min(max, Math.round(delay * 1.5));
        }
      },
    },
    commerce: {
      iap: {
        async createOrder(productId, o) {
          return cz(await keyedJ('POST', '/iap/orders', { product_id: productId }, o));
        },
        async verify(provider, signedTransaction) {
          const j = c.parse(await c.call({ method: 'POST', path: '/iap/' + e(provider) + '/verify', body: { signed_transaction: signedTransaction }, retry: true }));
          return { order: cz(j.order), replayed: !!j.replayed };
        },
      },
      ads: {
        async intent(placement) {
          return cz(c.parse(await c.call({ method: 'POST', path: '/ads/' + e(placement) + '/intent', body: {} })));
        },
      },
    },
    close() {
      if (timer !== undefined) c.scheduler.clear(timer);
      timer = undefined;
    },
  };
}

function toProfile(j: J): SocialProfile {
  const p = j.privacy ?? {};
  return {
    playerId: j.player_id,
    displayName: j.display_name,
    friendCode: j.friend_code,
    privacy: { searchable: !!p.searchable, friendRequests: p.friend_requests, showOnFriendBoards: !!p.show_on_friend_boards },
  };
}
