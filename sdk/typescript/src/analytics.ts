import { GoStackError } from './errors.js';
import { randomHex, utf8Length } from './util.js';
import type { AnalyticsOptions, AnalyticsStats, Scheduler } from './types.js';

interface Queued {
  json: string;
  bytes: number;
}

export interface AnalyticsDeps {
  opts: AnalyticsOptions;
  sessionId: string;
  clientVersion: string | undefined;
  platform: string;
  scheduler: Scheduler;
  now: () => number;
  random: () => number;
  /** POSTs the body; resolves on 2xx, rejects with GoStackError otherwise. */
  post: (body: string) => Promise<void>;
}

const MAX_PROPS_BYTES = 4096;
const MAX_EVENT_BYTES = 8192;
const WRAP_BYTES = utf8Length('{"events":[]}');

export class Analytics {
  private q: Queued[] = [];
  private bytes = 0;
  private sent = 0;
  private dropped = 0;
  private failures = 0;
  private timer: unknown = null;
  private inflight: Promise<void> | null = null;
  private closed = false;
  private readonly flushAt: number;
  private readonly interval: number;
  private readonly maxEvents: number;
  private readonly maxBody: number;
  private readonly maxBuffer: number;

  constructor(private d: AnalyticsDeps) {
    const o = d.opts;
    this.flushAt = o.flushAt ?? 50;
    this.interval = o.flushIntervalMs ?? 5000;
    this.maxEvents = Math.min(100, o.maxBatchEvents ?? 100);
    this.maxBody = Math.min(65536, o.maxBatchBytes ?? 65536);
    this.maxBuffer = o.maxBufferBytes ?? 1048576;
  }

  track(name: string, props?: Record<string, unknown>): void {
    if (this.closed) return;
    try {
      const properties = props ?? {};
      if (utf8Length(JSON.stringify(properties)) > MAX_PROPS_BYTES) return void this.dropped++;
      const ev: Record<string, unknown> = {
        event_id: randomHex(16, this.d.random),
        event_name: name,
        session_id: this.d.sessionId,
        client_time: this.d.now(),
        platform: this.d.platform,
        schema_version: 1,
        properties,
      };
      if (this.d.clientVersion) ev['client_version'] = this.d.clientVersion;
      const json = JSON.stringify(ev);
      const bytes = utf8Length(json);
      if (bytes > MAX_EVENT_BYTES || bytes > this.maxBuffer) return void this.dropped++;
      this.q.push({ json, bytes });
      this.bytes += bytes;
      this.trim();
      if (this.q.length >= this.flushAt) void this.flush();
      else this.schedule(this.interval);
    } catch {
      /* analytics never throws */
    }
  }

  stats(): AnalyticsStats {
    return { queued: this.q.length, queuedBytes: this.bytes, sent: this.sent, dropped: this.dropped };
  }

  /** Best-effort; never rejects. */
  flush(): Promise<void> {
    if (this.closed) return Promise.resolve();
    if (this.inflight) return this.inflight;
    this.unschedule();
    this.inflight = this.run().then(
      () => {
        this.inflight = null;
        if (this.q.length && !this.closed) this.schedule(this.nextDelay());
      },
      () => {
        this.inflight = null;
      },
    );
    return this.inflight;
  }

  close(): void {
    this.closed = true;
    this.unschedule();
  }

  private nextDelay(): number {
    return this.failures === 0 ? this.interval : Math.min(60000, this.interval * Math.pow(2, this.failures));
  }

  private schedule(ms: number): void {
    if (this.timer !== null || this.closed) return;
    this.timer = this.d.scheduler.set(() => {
      this.timer = null;
      void this.flush();
    }, ms);
  }

  private unschedule(): void {
    if (this.timer !== null) {
      this.d.scheduler.clear(this.timer);
      this.timer = null;
    }
  }

  private trim(): void {
    while (this.bytes > this.maxBuffer && this.q.length) {
      this.bytes -= (this.q.shift() as Queued).bytes;
      this.dropped++;
    }
  }

  private takeBatch(): { items: Queued[]; body: string } {
    let size = WRAP_BYTES;
    let n = 0;
    for (const it of this.q) {
      const add = it.bytes + (n ? 1 : 0);
      if (n >= this.maxEvents || size + add > this.maxBody) break;
      size += add;
      n++;
    }
    if (n === 0) n = 1;
    const items = this.q.splice(0, n);
    for (const it of items) this.bytes -= it.bytes;
    return { items, body: '{"events":[' + items.map((i) => i.json).join(',') + ']}' };
  }

  private async run(): Promise<void> {
    while (this.q.length && !this.closed) {
      const { items, body } = this.takeBatch();
      try {
        await this.d.post(body);
        this.sent += items.length;
        this.failures = 0;
      } catch (e) {
        if (e instanceof GoStackError && e.status >= 400 && e.status < 500 && e.status !== 429) {
          this.dropped += items.length; // permanent rejection; retrying cannot help
          continue;
        }
        this.failures++;
        this.q = items.concat(this.q);
        for (const it of items) this.bytes += it.bytes;
        this.trim();
        return;
      }
    }
  }
}
