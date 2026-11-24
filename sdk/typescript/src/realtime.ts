/**
 * Optional realtime client for the GameKit WebSocket gateway (docs/api/realtime.md).
 * Separate entry point (`@pillar/gostack-game-sdk/realtime`); the core bundle does not import it.
 *
 * Protocol rules implemented here:
 * - The access token is sent in the first message ({type:"auth"}), never in the URL.
 * - Business messages carry a per-player contiguous `seq`; we track the highest processed seq and ack it (batched).
 * - After reconnecting and receiving `welcome`, we immediately send `resume` with the last epoch/seq. Business messages that
 *   arrive before `resumed`/`resume_failed` are ignored (the server replays them after `resumed`).
 * - `resume_failed` means the stream cannot be continued: we emit `resync` (re-fetch state over HTTP) and carry on.
 */

export interface RealtimeSocket {
  send(data: string): void;
  close(code?: number, reason?: string): void;
  onOpen(cb: () => void): void;
  onMessage(cb: (data: string) => void): void;
  onClose(cb: (code: number, reason: string) => void): void;
  onError(cb: (message: string) => void): void;
}
export type SocketFactory = (url: string) => RealtimeSocket;

export interface RealtimeScheduler {
  set(fn: () => void, ms: number): unknown;
  clear(handle: unknown): void;
}

export interface RealtimeOptions {
  /** Full ws(s):// URL, e.g. from realtimeURL(). Must not contain a token. */
  url: string;
  /** Called before every (re)connect so a refreshed session token is used. Use `client.auth.accessToken`. */
  getToken(): Promise<string> | string;
  socketFactory: SocketFactory;
  /** Resume an earlier stream (e.g. persisted by the app across page reloads). */
  initialState?: { epoch: string; lastSeq: number };
  /** Reconnect backoff with full jitter. Defaults 500 / 15000. */
  reconnectBaseMs?: number;
  reconnectMaxMs?: number;
  /** Application-level ping interval and pong deadline. Defaults 20000 / 10000. Server inbound limit is 10 msg/s. */
  heartbeatIntervalMs?: number;
  pongTimeoutMs?: number;
  /** Time allowed from socket open to welcome. Default 10000. */
  handshakeTimeoutMs?: number;
  /** Send an ack after this many business messages or ackDelayMs, whichever first. Defaults 8 / 500. */
  ackEvery?: number;
  ackDelayMs?: number;
  /** Consecutive 4001 closes before giving up. Default 3. */
  maxAuthFailures?: number;
  scheduler?: RealtimeScheduler;
  random?: () => number;
}

export type RealtimeState = 'idle' | 'connecting' | 'authenticating' | 'resuming' | 'ready' | 'reconnecting' | 'closed';

export interface RealtimeMessage<T = unknown> {
  type: string;
  seq: number;
  data: T;
}
export interface RealtimeCloseInfo {
  code: number;
  reason: string;
  /** True when the client will not reconnect (manual close, 4003, 4009, repeated 4001). */
  fatal: boolean;
}
export interface RealtimeEvents {
  /** Authenticated (first connect) or resumed. */
  open: { playerId: string; epoch: string; resumed: boolean };
  message: RealtimeMessage;
  /** The stream could not be continued (resume_failed or sequence gap after a fresh stream): re-fetch state over HTTP. */
  resync: { epoch: string; seq: number };
  /** Server error frame (connection stays up). */
  error: { code: string; message: string; ref?: string };
  /** Reply to an application message sent with an id. */
  ok: { ref: string };
  close: RealtimeCloseInfo;
  state: RealtimeState;
}

type Listener<K extends keyof RealtimeEvents> = (ev: RealtimeEvents[K]) => void;
type J = Record<string, any>;

const RESERVED = new Set(['auth', 'ping', 'pong', 'ack', 'resume', 'resumed', 'resume_failed', 'welcome', 'error', 'ok']);
const TYPE_RE = /^[a-z][a-z0-9_.:-]{0,63}$/;
const MAX_MESSAGE_BYTES = 4096;
/** Close codes after which reconnecting is pointless or harmful. 4009: another connection replaced us; reconnecting would flap. */
const FATAL_CODES = new Set([4003, 4009]);

/** https://host/base -> wss://host/base/v1/games/{game}/realtime */
export function realtimeURL(baseURL: string, gameId: string): string {
  return baseURL.replace(/\/+$/, '').replace(/^http/i, 'ws') + '/v1/games/' + encodeURIComponent(gameId) + '/realtime';
}

function utf8Len(s: string): number {
  let n = 0;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c < 0x80) n += 1;
    else if (c < 0x800) n += 2;
    else if (c >= 0xd800 && c <= 0xdbff) {
      n += 4;
      i++;
    } else n += 3;
  }
  return n;
}

export class RealtimeClient {
  private o: Required<Omit<RealtimeOptions, 'initialState'>> & Pick<RealtimeOptions, 'initialState'>;
  private ls: { [K in keyof RealtimeEvents]?: Array<Listener<K>> } = {};
  private st: RealtimeState = 'idle';
  private sock: RealtimeSocket | null = null;
  private gen = 0;
  private epoch: string | undefined;
  private playerId = '';
  private lastSeq = 0;
  private ackedSeq = 0;
  private unacked = 0;
  private attempt = 0;
  private authFailures = 0;
  private stopped = false;
  private hbTimer: unknown;
  private pongTimer: unknown;
  private hsTimer: unknown;
  private ackTimer: unknown;
  private retryTimer: unknown;

  constructor(opts: RealtimeOptions) {
    if (/[?&](token|access_token|authorization|auth|bearer)=/i.test(opts.url)) {
      throw new Error('realtime: never put the token in the URL; it is sent as the first message');
    }
    this.o = {
      reconnectBaseMs: 500,
      reconnectMaxMs: 15000,
      heartbeatIntervalMs: 20000,
      pongTimeoutMs: 10000,
      handshakeTimeoutMs: 10000,
      ackEvery: 8,
      ackDelayMs: 500,
      maxAuthFailures: 3,
      random: Math.random,
      scheduler: {
        set: (fn, ms) => setTimeout(fn, ms),
        clear: (h) => clearTimeout(h as ReturnType<typeof setTimeout>),
      },
      ...opts,
    };
    if (opts.initialState) {
      this.epoch = opts.initialState.epoch;
      this.lastSeq = this.ackedSeq = opts.initialState.lastSeq;
    }
  }

  get state(): RealtimeState {
    return this.st;
  }
  /** Stream position, for persisting across reloads (initialState). */
  get position(): { epoch: string | undefined; lastSeq: number } {
    return { epoch: this.epoch, lastSeq: this.lastSeq };
  }

  on<K extends keyof RealtimeEvents>(ev: K, cb: Listener<K>): () => void {
    const l = (this.ls[ev] ??= []) as Array<Listener<K>>;
    l.push(cb);
    return () => {
      const i = l.indexOf(cb);
      if (i >= 0) l.splice(i, 1);
    };
  }

  private emit<K extends keyof RealtimeEvents>(ev: K, data: RealtimeEvents[K]): void {
    for (const cb of [...((this.ls[ev] ?? []) as Array<Listener<K>>)]) {
      try {
        cb(data);
      } catch {
        /* listener errors must not break the protocol loop */
      }
    }
  }

  private setState(s: RealtimeState): void {
    if (this.st === s) return;
    this.st = s;
    this.emit('state', s);
  }

  connect(): void {
    if (this.stopped || this.sock || this.retryTimer !== undefined) return;
    void this.open();
  }

  /** Sends an application message. Returns false (and sends nothing) unless the connection is ready. */
  send(type: string, data?: unknown, id?: string): boolean {
    if (!TYPE_RE.test(type) || RESERVED.has(type)) throw new Error('realtime: invalid or reserved message type');
    if (this.st !== 'ready' || !this.sock) return false;
    const frame = JSON.stringify(id === undefined ? { type, data } : { type, id, data });
    if (utf8Len(frame) > MAX_MESSAGE_BYTES) throw new Error('realtime: message exceeds 4096 bytes');
    this.sock.send(frame);
    return true;
  }

  close(): void {
    if (this.stopped) return;
    this.stopped = true;
    this.clearTimers();
    this.cancelRetry();
    const s = this.sock;
    this.sock = null;
    this.gen++;
    try {
      s?.close(1000, 'client close');
    } catch {
      /* already closed */
    }
    this.setState('closed');
    this.emit('close', { code: 1000, reason: 'client close', fatal: true });
  }

  // ---- internals ----
  private async open(): Promise<void> {
    const gen = ++this.gen;
    this.setState(this.attempt === 0 ? 'connecting' : 'reconnecting');
    let token: string;
    try {
      token = await this.o.getToken();
    } catch {
      if (gen === this.gen && !this.stopped) this.scheduleReconnect();
      return;
    }
    if (gen !== this.gen || this.stopped) return;
    let s: RealtimeSocket;
    try {
      s = this.o.socketFactory(this.o.url);
    } catch {
      this.scheduleReconnect();
      return;
    }
    this.sock = s;
    const live = () => gen === this.gen && this.sock === s;
    s.onOpen(() => {
      if (!live()) return;
      this.setState('authenticating');
      s.send(JSON.stringify({ type: 'auth', token })); // first message; the URL never carries the token
      this.hsTimer = this.o.scheduler.set(() => live() && s.close(4000, 'handshake timeout'), this.o.handshakeTimeoutMs);
    });
    s.onMessage((d) => live() && this.onFrame(s, d));
    s.onError(() => undefined); // a close always follows
    s.onClose((code, reason) => live() && this.onClosed(code, reason));
  }

  private onFrame(s: RealtimeSocket, raw: string): void {
    let m: J;
    try {
      m = JSON.parse(raw);
    } catch {
      return;
    }
    if (!m || typeof m.type !== 'string') return;
    this.armHeartbeat(s); // any inbound frame proves liveness
    switch (m.type) {
      case 'welcome':
        this.clearHandshake();
        this.attempt = 0;
        this.authFailures = 0;
        this.playerId = m.player_id ?? this.playerId;
        if (this.epoch === undefined) {
          this.epoch = m.epoch;
          this.lastSeq = this.ackedSeq = m.seq;
          this.setState('ready');
          this.emit('open', { playerId: m.player_id, epoch: m.epoch, resumed: false });
        } else {
          // Resume immediately; business frames are ignored until resumed/resume_failed.
          this.setState('resuming');
          s.send(JSON.stringify({ type: 'resume', epoch: this.epoch, last_seq: this.lastSeq }));
        }
        return;
      case 'resumed':
        if (this.st !== 'resuming') return;
        this.epoch = m.epoch;
        this.setState('ready');
        this.emit('open', { playerId: this.playerId, epoch: m.epoch, resumed: true });
        return;
      case 'resume_failed':
        if (this.st !== 'resuming') return;
        this.epoch = m.epoch;
        this.lastSeq = this.ackedSeq = m.seq;
        this.unacked = 0;
        this.setState('ready');
        this.emit('resync', { epoch: m.epoch, seq: m.seq });
        return;
      case 'pong':
        return;
      case 'ok':
        this.emit('ok', { ref: m.ref });
        return;
      case 'error':
        this.emit('error', { code: m.code, message: m.message, ...(m.ref !== undefined ? { ref: m.ref } : {}) });
        return;
    }
    if (typeof m.seq !== 'number') return; // unknown control frame
    if (this.st !== 'ready') return; // between reconnect and resumed/resume_failed
    if (m.seq <= this.lastSeq) return; // duplicate
    if (m.seq !== this.lastSeq + 1) {
      // Gap on an ordered stream: reconnect; resume replays what is still buffered or reports resume_failed.
      s.close(4000, 'sequence gap');
      return;
    }
    this.lastSeq = m.seq;
    this.emit('message', { type: m.type, seq: m.seq, data: m.data });
    if (++this.unacked >= this.o.ackEvery) this.sendAck(s);
    else if (this.ackTimer === undefined) this.ackTimer = this.o.scheduler.set(() => this.sendAck(s), this.o.ackDelayMs);
  }

  private sendAck(s: RealtimeSocket): void {
    if (this.ackTimer !== undefined) this.o.scheduler.clear(this.ackTimer);
    this.ackTimer = undefined;
    this.unacked = 0;
    if (this.sock !== s || this.lastSeq <= this.ackedSeq) return;
    this.ackedSeq = this.lastSeq;
    s.send(JSON.stringify({ type: 'ack', seq: this.lastSeq }));
  }

  private armHeartbeat(s: RealtimeSocket): void {
    const sch = this.o.scheduler;
    if (this.pongTimer !== undefined) sch.clear(this.pongTimer);
    this.pongTimer = undefined;
    if (this.hbTimer !== undefined) sch.clear(this.hbTimer);
    this.hbTimer = sch.set(() => {
      this.hbTimer = undefined;
      if (this.sock !== s) return;
      s.send(JSON.stringify({ type: 'ping' }));
      this.pongTimer = sch.set(() => {
        this.pongTimer = undefined;
        if (this.sock === s) s.close(4000, 'pong timeout'); // dead connection: reconnect + resume
      }, this.o.pongTimeoutMs);
    }, this.o.heartbeatIntervalMs);
  }

  private onClosed(code: number, reason: string): void {
    this.sock = null;
    this.gen++;
    this.clearTimers();
    if (code === 4001) this.authFailures++;
    else this.authFailures = 0;
    const fatal = this.stopped || FATAL_CODES.has(code) || this.authFailures >= this.o.maxAuthFailures;
    if (fatal) {
      this.stopped = true;
      this.setState('closed');
      this.emit('close', { code, reason, fatal: true });
      return;
    }
    this.emit('close', { code, reason, fatal: false });
    this.scheduleReconnect();
  }

  private scheduleReconnect(): void {
    this.setState('reconnecting');
    const cap = Math.min(this.o.reconnectMaxMs, this.o.reconnectBaseMs * Math.pow(2, this.attempt));
    this.attempt++;
    this.retryTimer = this.o.scheduler.set(() => {
      this.retryTimer = undefined;
      if (!this.stopped) void this.open();
    }, Math.floor(this.o.random() * cap));
  }

  private cancelRetry(): void {
    if (this.retryTimer !== undefined) this.o.scheduler.clear(this.retryTimer);
    this.retryTimer = undefined;
  }

  private clearHandshake(): void {
    if (this.hsTimer !== undefined) this.o.scheduler.clear(this.hsTimer);
    this.hsTimer = undefined;
  }

  private clearTimers(): void {
    const sch = this.o.scheduler;
    for (const t of [this.hbTimer, this.pongTimer, this.hsTimer, this.ackTimer]) if (t !== undefined) sch.clear(t);
    this.hbTimer = this.pongTimer = this.hsTimer = this.ackTimer = undefined;
    this.unacked = 0;
  }
}

export function createRealtimeClient(opts: RealtimeOptions): RealtimeClient {
  return new RealtimeClient(opts);
}
