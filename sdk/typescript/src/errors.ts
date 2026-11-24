export class GoStackError extends Error {
  readonly code: string;
  readonly status: number;
  readonly requestId: string | undefined;
  readonly details: Record<string, unknown> | undefined;
  /** From the Retry-After header of a 429/503 response, when present. */
  retryAfterMs: number | undefined;
  constructor(
    code: string,
    message: string,
    status = 0,
    requestId?: string,
    details?: Record<string, unknown>,
  ) {
    super(message);
    this.name = 'GoStackError';
    this.code = code;
    this.status = status;
    this.requestId = requestId;
    this.details = details;
  }
  /** True for transport failures/timeouts (no HTTP response). */
  get isNetworkError(): boolean {
    return this.code === 'network_error';
  }
}

export class RevisionConflictError extends GoStackError {
  readonly currentRevision: number;
  constructor(message: string, requestId: string | undefined, details: Record<string, unknown> | undefined) {
    super('revision_conflict', message, 409, requestId, details);
    this.name = 'RevisionConflictError';
    const r = details?.current_revision;
    this.currentRevision = typeof r === 'number' ? r : -1;
  }
}

export function errorFromResponse(status: number, headers: Record<string, string>, text: string): GoStackError {
  let code = status >= 500 ? 'internal' : 'unknown';
  let message = `HTTP ${status}`;
  let requestId: string | undefined;
  let details: Record<string, unknown> | undefined;
  try {
    const j = JSON.parse(text);
    const e = j && j.error;
    if (e && typeof e === 'object') {
      if (typeof e.code === 'string') code = e.code;
      if (typeof e.message === 'string') message = e.message;
      if (e.details && typeof e.details === 'object') details = e.details;
    }
    if (j && typeof j.request_id === 'string') requestId = j.request_id;
  } catch {
    /* non-JSON body */
  }
  requestId = requestId ?? headers['x-request-id'];
  if (code === 'revision_conflict' && status === 409) return new RevisionConflictError(message, requestId, details);
  const err = new GoStackError(code, message, status, requestId, details);
  const ra = Number(headers['retry-after']);
  if (headers['retry-after'] && isFinite(ra) && ra >= 0) err.retryAfterMs = ra * 1000;
  return err;
}

export type RedeemReason = 'invalid_code' | 'already_redeemed' | 'rate_limited' | 'other';

/** Redeem failures: 400 invalid_code (unknown/expired/exhausted, deliberately indistinguishable), 409 already_redeemed, 429 rate_limited. */
export class RedeemError extends GoStackError {
  readonly reason: RedeemReason;
  constructor(cause: GoStackError) {
    super(cause.code, cause.message, cause.status, cause.requestId, cause.details);
    this.name = 'RedeemError';
    this.retryAfterMs = cause.retryAfterMs;
    this.reason = cause.code === 'invalid_code' || cause.code === 'already_redeemed' || cause.code === 'rate_limited' ? cause.code : 'other';
  }
}
