import { GoStackError } from './errors.js';
import type { KeyValueStorage, Lifecycle, Transport } from './types.js';

/** Minimal subset of the WeChat mini-game `wx` API used by the adapters. Pass the real `wx` object. */
export interface WxRequestApi {
  request(opts: {
    url: string;
    method: string;
    header: Record<string, string>;
    data?: string;
    timeout: number;
    dataType: string;
    responseType: string;
    success(res: { statusCode: number; header?: Record<string, string>; data: unknown }): void;
    fail(err: { errMsg?: string }): void;
  }): unknown;
}

export interface WxStorageApi {
  getStorage(opts: { key: string; success(res: { data: unknown }): void; fail(err: { errMsg?: string }): void }): void;
  setStorage(opts: { key: string; data: string; success(): void; fail(err: { errMsg?: string }): void }): void;
  removeStorage(opts: { key: string; success(): void; fail(err: { errMsg?: string }): void }): void;
}

export interface WxLifecycleApi {
  onHide(cb: () => void): void;
  onShow(cb: () => void): void;
  offHide?(cb: () => void): void;
  offShow?(cb: () => void): void;
}

export function wxTransport(wx: WxRequestApi): Transport {
  return {
    request: (method, url, headers, body, timeoutMs) =>
      new Promise((resolve, reject) => {
        const opts: Parameters<WxRequestApi['request']>[0] = {
          url,
          method,
          header: headers,
          timeout: timeoutMs,
          dataType: 'text', // anything but 'json' disables wx's own parsing
          responseType: 'text',
          success: (res) =>
            resolve({
              status: res.statusCode,
              headers: res.header ?? {},
              body: typeof res.data === 'string' ? res.data : res.data == null ? '' : JSON.stringify(res.data),
            }),
          fail: (err) => reject(new GoStackError('network_error', err?.errMsg ?? 'wx.request failed')),
        };
        if (body !== undefined) opts.data = body;
        wx.request(opts);
      }),
  };
}

export function wxStorage(wx: WxStorageApi): KeyValueStorage {
  return {
    // wx.getStorage fails when the key is absent; treat any failure as "not found".
    get: (key) =>
      new Promise((resolve) =>
        wx.getStorage({
          key,
          success: (r) => resolve(typeof r.data === 'string' ? r.data : null),
          fail: () => resolve(null),
        }),
      ),
    set: (key, data) =>
      new Promise((resolve, reject) => wx.setStorage({ key, data, success: () => resolve(), fail: (e) => reject(new Error(e?.errMsg ?? 'setStorage failed')) })),
    remove: (key) => new Promise((resolve) => wx.removeStorage({ key, success: () => resolve(), fail: () => resolve() })),
  };
}

export function wxLifecycle(wx: WxLifecycleApi): Lifecycle {
  return {
    onHide(cb) {
      wx.onHide(cb);
      return () => wx.offHide?.(cb);
    },
    onShow(cb) {
      wx.onShow(cb);
      return () => wx.offShow?.(cb);
    },
  };
}
