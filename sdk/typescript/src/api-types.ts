import type { IdempotentOptions } from './types.js';

export type Grants = Record<string, number>;

// ---- saves ----
export interface SaveRevision {
  revision: number;
  schemaVersion: number;
  checksum: string;
  size: number;
  createdAt: number;
}
export interface SaveRevisions {
  slot: string;
  currentRevision: number;
  revisions: SaveRevision[];
}
export interface RestoreSaveOptions extends IdempotentOptions {
  /** Current revision the client believes the slot is at. */
  expectedRevision: number;
}
export interface DeleteSaveOptions extends IdempotentOptions {
  expectedRevision: number;
}
export interface DeleteSaveResult {
  slot: string;
  deleted: boolean;
  revision: number;
}

// ---- boards ----
export interface Run {
  runToken: string;
  board: string;
  startedAt: number;
  expiresAt: number;
}

// ---- economy ----
export interface Inventory {
  currencies: Array<{ id: string; name?: string; balance: number }>;
  /** maxStack is absent when the item has no stack limit. */
  items: Array<{ id: string; balance: number; maxStack?: number }>;
  uncataloged: Record<string, number>;
}
export interface PurchaseResult {
  purchaseId: string;
  cost: Grants;
  grants: Grants;
  balances: Record<string, number>;
  configRevision: number;
}

// ---- ops ----
export interface Announcement {
  id: string;
  title: string;
  body: string;
  priority: number;
  startsAt: number;
  endsAt: number;
  platforms: string[];
}
export interface GameEvent {
  id: string;
  title: string;
  startsAt: number;
  endsAt: number;
  timezone: string;
  metadata: Record<string, unknown>;
}
export interface MailItem {
  id: string;
  title: string;
  body: string;
  grants: Grants;
  createdAt: number;
  expiresAt: number;
  read: boolean;
  claimed: boolean;
  claimable: boolean;
}
export interface MailClaimResult {
  mailId: string;
  grants: Grants;
}
export interface CheckinStatus {
  timezone: string;
  today: string;
  checkedInToday: boolean;
  streak: number;
  total: number;
  cycleLength: number;
  nextIndex: number;
  nextGrants: Grants;
  nextAvailableAt: number;
}
export interface CheckinResult {
  day: string;
  streak: number;
  index: number;
  grants: Grants;
}
export interface TaskItem {
  id: string;
  title: string;
  metric: string;
  target: number;
  period: 'once' | 'daily' | 'weekly';
  grants: Grants;
  periodKey: string;
  progress: number;
  completed: boolean;
  claimed: boolean;
  claimable: boolean;
}
export interface TaskClaimResult {
  taskId: string;
  periodKey: string;
  grants: Grants;
}
export interface RedeemResult {
  batchId: string;
  grants: Grants;
}

// ---- experiments ----
export interface ExperimentAssignment {
  layer: string;
  variant: string;
  params: Record<string, unknown>;
  /** Kill switch: the experiment was stopped and everyone is on control. */
  stopped?: boolean;
}
export interface ExperimentQuery {
  platform?: string;
  clientVersion?: string;
}

// ---- social ----
export interface Privacy {
  searchable: boolean;
  friendRequests: 'everyone' | 'nobody';
  showOnFriendBoards: boolean;
}
export interface SocialProfile {
  playerId: string;
  displayName: string;
  friendCode: string;
  privacy: Privacy;
}
export interface ProfileUpdate {
  displayName?: string;
  privacy?: Partial<Privacy>;
}
export interface Friend {
  playerId: string;
  displayName: string;
  at: number;
}
export interface FriendRequests {
  incoming: Friend[];
  outgoing: Friend[];
}
export type FriendTarget = { playerId: string; friendCode?: never } | { friendCode: string; playerId?: never };
export interface FriendBoardEntry {
  rank: number;
  playerId: string;
  displayName: string;
  score: number;
  achievedAt: number;
  me: boolean;
}
export interface FriendBoard {
  board: string;
  season: string;
  entries: FriendBoardEntry[];
}
export interface InviteInfo {
  code: string;
  createdAt: number;
  accepted: number;
}
export interface InviteAcceptResult {
  attributed: boolean;
  grants: Grants;
}
export interface ShareOpenResult {
  shareId: string;
  channel: string;
  payload: unknown;
  firstOpen: boolean;
}
export interface ShareInfo {
  shareId: string;
  channel: string;
  payload: unknown;
  createdAt: number;
  opens: number;
}

// ---- match ----
export type TicketStatus = 'waiting' | 'matched' | 'expired' | 'cancelled' | 'abandoned' | 'done';
export interface MatchMember {
  playerId: string;
  state: 'assigned' | 'connected' | 'left';
}
export interface MatchRoom {
  roomId: string;
  status: 'open' | 'active' | 'settled' | 'abandoned';
  leaseUntil: number;
  /** Null until a battle server claims the room; then the object the server supplied (e.g. {url}). */
  connection: Record<string, unknown> | null;
  members: MatchMember[];
}
export interface MatchTicket {
  ticketId: string;
  queue: string;
  status: TicketStatus;
  rating?: number;
  createdAt?: number;
  expiresAt: number;
  room?: MatchRoom;
}
export interface WaitForMatchOptions {
  /** First poll delay. Default 1000. */
  intervalMs?: number;
  /** Poll delay grows by 1.5x up to this cap. Default 5000. */
  maxIntervalMs?: number;
  /** Give up (GoStackError code "timeout") after this long. Default: the ticket's expires_at. */
  timeoutMs?: number;
  /** Called on every poll with the latest ticket. */
  onUpdate?(t: MatchTicket): void;
  /** Return true to stop polling (rejects with code "aborted"). */
  isAborted?(): boolean;
}

// ---- commerce ----
export type OrderState = 'created' | 'paid' | 'delivered' | 'refunded' | 'refund_compensated' | 'refund_shortfall';
export interface IapOrder {
  orderId: string;
  productId: string;
  /** Pass to StoreKit as appAccountToken when purchasing. */
  appAccountToken: string;
  providerProductIds: Record<string, string>;
  state: OrderState;
}
export interface IapTransaction {
  orderId: string;
  provider: string;
  productId: string;
  providerProductId: string;
  providerTxnId: string;
  state: OrderState;
  quantity: number;
  grants: Grants;
  outstanding: Grants;
  playerId: string;
  createdAt: number;
  updatedAt: number;
  deliveredAt?: number;
  refundedAt?: number;
}
export interface IapVerifyResult {
  order: IapTransaction;
  replayed: boolean;
}
export interface AdIntent {
  /** Pass to the ad SDK as server-side verification custom data. */
  token: string;
  placementId: string;
  expiresAt: number;
}
