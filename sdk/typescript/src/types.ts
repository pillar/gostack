export interface TransportResponse {
  status: number;
  headers: Record<string, string>;
  /** Raw response body text (never pre-parsed). */
  body: string;
}

/** Pluggable HTTP transport. Must resolve for any HTTP status and reject only on network/timeout errors. */
export interface Transport {
  request(
    method: string,
    url: string,
    headers: Record<string, string>,
    body: string | undefined,
    timeoutMs: number,
  ): Promise<TransportResponse>;
}

export interface KeyValueStorage {
  get(key: string): Promise<string | null>;
  set(key: string, value: string): Promise<void>;
  remove(key: string): Promise<void>;
}

export type Unsubscribe = () => void;

export interface Lifecycle {
  onHide(cb: () => void): Unsubscribe | void;
  onShow(cb: () => void): Unsubscribe | void;
}

export interface Scheduler {
  set(fn: () => void, ms: number): unknown;
  clear(handle: unknown): void;
}

export interface RetryOptions {
  /** Total attempts including the first one. Default 3. */
  maxAttempts?: number;
  /** Default 200. */
  baseDelayMs?: number;
  /** Default 5000. */
  maxDelayMs?: number;
  /** If the server asks to wait longer than this, fail instead of waiting. Default 30000. */
  maxRetryAfterMs?: number;
}

export interface AnalyticsOptions {
  /** Flush when this many events are queued. Default 50. */
  flushAt?: number;
  /** Flush this long after the first queued event. Default 5000. */
  flushIntervalMs?: number;
  /** Max events per request (server limit 100). Default 100. */
  maxBatchEvents?: number;
  /** Max request body bytes (server limit 64 KiB). Default 65536. */
  maxBatchBytes?: number;
  /** Total offline buffer bytes; oldest events are dropped beyond it. Default 1 MiB. */
  maxBufferBytes?: number;
}

export interface GameClientOptions {
  gameId: string;
  baseURL: string;
  transport: Transport;
  storage?: KeyValueStorage;
  lifecycle?: Lifecycle;
  clientVersion?: string;
  platform?: string;
  /** Per-attempt timeout. Default 10000. */
  timeoutMs?: number;
  retry?: RetryOptions;
  /** Built-in config defaults, merged under server values (shallow). */
  defaults?: Record<string, unknown>;
  analytics?: AnalyticsOptions;
  /** Injectable for tests. */
  sleep?: (ms: number) => Promise<void>;
  now?: () => number;
  random?: () => number;
  scheduler?: Scheduler;
}

export interface Session {
  playerId: string;
  guestId: string | null;
  accessToken: string;
  expiresAt: number;
  created: boolean;
}

export interface Me {
  playerId: string;
  gameId: string;
  createdAt: number;
  identities: string[];
}

export interface SaveRecord<T = unknown> {
  slot: string;
  revision: number;
  schemaVersion: number;
  checksum: string;
  payload: T;
  updatedAt: number;
  /** True when the server merged this write with a newer revision (payload is the merged value). */
  merged?: boolean;
}

export interface PutSaveOptions {
  /** 0 means "create". */
  expectedRevision: number;
  schemaVersion?: number;
  idempotencyKey?: string;
  /** Optional sha256 hex of the raw payload JSON bytes. */
  checksum?: string;
  /** Ask the server to merge on revision conflict (only if the app configured a merge function). */
  merge?: boolean;
}

export interface BoardEntry {
  rank: number;
  playerId: string;
  score: number;
  achievedAt: number;
}

export interface BoardPage {
  board: string;
  season: string;
  entries: BoardEntry[];
  nextCursor: string | null;
  me: { rank: number; score: number } | null;
}

export interface BoardGetOptions {
  limit?: number;
  cursor?: string;
  season?: string;
}

export interface ScoreResult {
  status: 'accepted' | 'quarantined';
  /** null when the submission was quarantined and the player has no accepted score yet. */
  best: number | null;
  season: string;
}

export interface Wallet {
  balances: Record<string, number>;
}

export interface ClaimResult {
  claimId: string;
  grants: Record<string, number>;
  balances: Record<string, number>;
  configRevision: number;
}

export interface IdempotentOptions {
  idempotencyKey?: string;
}

export interface ConfigGetOptions {
  defaults?: Record<string, unknown>;
  /** Overrides the client's platform for rollout targeting. */
  platform?: string;
  /** Overrides the client's clientVersion (1-4 dotted integers). */
  clientVersion?: string;
  /** Stable anonymous bucketing key (e.g. install id); the server ignores it when a session token is valid. */
  bucketKey?: string;
}

export interface SubmitScoreOptions extends IdempotentOptions {
  /** From boards.startRun(); required when the board has require_run. */
  runToken?: string;
  /** Must equal the board's rule_version when that is > 0. */
  ruleVersion?: number;
}

export interface ConfigResult<T = Record<string, unknown>> {
  revision: number;
  /** Ids of rollouts that matched this request ; absent when none matched. */
  rollouts?: string[];
  data: T;
  /** network: fresh or 304-validated; cache: last stored value; default: built-in defaults. */
  source: 'network' | 'cache' | 'default';
}

export interface AnalyticsStats {
  queued: number;
  queuedBytes: number;
  sent: number;
  dropped: number;
}
