export class GoStackError extends Error {
  readonly code: string;
  readonly status: number;
  readonly requestId: string | undefined;
  readonly details: Record<string, unknown> | undefined;
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
  return new GoStackError(code, message, status, requestId, details);
}
