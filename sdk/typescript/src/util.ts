import type { KeyValueStorage } from './types.js';

export function randomHex(bytes: number, random: () => number = Math.random): string {
  const a = new Uint8Array(bytes);
  const c = (globalThis as { crypto?: { getRandomValues?: (a: Uint8Array) => Uint8Array } }).crypto;
  if (c && typeof c.getRandomValues === 'function') c.getRandomValues(a);
  else for (let i = 0; i < bytes; i++) a[i] = Math.floor(random() * 256);
  let s = '';
  for (let i = 0; i < bytes; i++) s += (a[i] as number).toString(16).padStart(2, '0');
  return s;
}

export function utf8Length(s: string): number {
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

export function memoryStorage(): KeyValueStorage {
  const m = new Map<string, string>();
  return {
    get: async (k) => (m.has(k) ? (m.get(k) as string) : null),
    set: async (k, v) => {
      m.set(k, v);
    },
    remove: async (k) => {
      m.delete(k);
    },
  };
}
