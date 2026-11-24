// Minimal ambient declarations so tests typecheck without @types/node.
declare module 'node:test' {
  export function test(name: string, fn: () => void | Promise<void>): void;
}
declare module 'node:assert/strict' {
  interface Assert {
    (v: unknown, msg?: string): void;
    equal(a: unknown, b: unknown, msg?: string): void;
    notEqual(a: unknown, b: unknown, msg?: string): void;
    deepEqual(a: unknown, b: unknown, msg?: string): void;
    ok(v: unknown, msg?: string): void;
    rejects(p: Promise<unknown> | (() => Promise<unknown>), check?: (e: any) => boolean): Promise<void>;
  }
  const assert: Assert;
  export default assert;
}
