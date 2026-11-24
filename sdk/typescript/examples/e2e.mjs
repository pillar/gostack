// Live end-to-end check. Usage: GOSTACK_URL=http://127.0.0.1:8080 GAME_ID=demo node examples/e2e.mjs
// Requires `npm run build` first and a game whose config defines board "main" and reward "daily_login".
import { createGameClient, memoryStorage, RevisionConflictError } from '../dist/index.js';
import { fetchTransport } from '../dist/browser.js';

const baseURL = process.env.GOSTACK_URL;
const gameId = process.env.GAME_ID;
if (!baseURL || !gameId) {
  console.error('GOSTACK_URL and GAME_ID are required');
  process.exit(2);
}

const steps = [];
const client = createGameClient({
  gameId,
  baseURL,
  transport: fetchTransport(),
  storage: memoryStorage(),
  clientVersion: '0.0.0-e2e',
  platform: 'web',
});

async function step(name, fn) {
  try {
    const out = await fn();
    steps.push({ step: name, ok: true, result: out });
    return out;
  } catch (e) {
    steps.push({ step: name, ok: false, error: { name: e?.name, code: e?.code, status: e?.status, requestId: e?.requestId, message: e?.message } });
    throw e;
  }
}

let failed = false;
try {
  const login = await step('auth.loginGuest', async () => {
    const s = await client.auth.loginGuest();
    return { playerId: s.playerId, created: s.created };
  });
  await step('auth.loginGuest(again)', async () => {
    const s = await client.auth.loginGuest();
    if (s.playerId !== login.playerId) throw new Error('guest login returned a different player');
    return { playerId: s.playerId, created: s.created };
  });

  const existing = await step('saves.get(before)', async () => {
    const s = await client.saves.get('main');
    return s ? { revision: s.revision } : null;
  });
  const base = existing ? existing.revision : 0;
  const saved = await step('saves.put', async () => {
    const s = await client.saves.put('main', { level: 3, e2e: true }, { expectedRevision: base });
    return { revision: s.revision };
  });
  await step('saves.get(after)', async () => {
    const s = await client.saves.get('main');
    if (!s || s.revision !== saved.revision || s.payload.level !== 3) throw new Error('saved payload mismatch');
    return { revision: s.revision };
  });
  await step('saves.put(conflict)', async () => {
    try {
      await client.saves.put('main', { level: 4 }, { expectedRevision: base });
    } catch (e) {
      if (e instanceof RevisionConflictError && e.currentRevision === saved.revision) return { currentRevision: e.currentRevision };
      throw e;
    }
    throw new Error('expected RevisionConflictError');
  });

  await step('boards.submit', () => client.boards.submit('main', 900));
  await step('boards.get', async () => {
    const b = await client.boards.get('main', { limit: 10 });
    return { season: b.season, entries: b.entries.length, me: b.me };
  });
  await step('rewards.claim(daily_login)', () => client.rewards.claim('daily_login'));
  await step('wallet.get', () => client.wallet.get());
  await step('config.get', async () => {
    const c = await client.config.get();
    return { revision: c.revision, source: c.source };
  });

  client.analytics.track('e2e_run', { ok: true });
  await step('analytics.flush', async () => {
    await client.analytics.flush();
    const st = client.analytics.stats();
    if (st.sent < 1) throw new Error('analytics event was not delivered: ' + JSON.stringify(st));
    return st;
  });
  await step('auth.logout', () => client.auth.logout());
} catch {
  failed = true;
} finally {
  client.close();
}

console.log(JSON.stringify({ ok: !failed, gameId, baseURL, steps }, null, 2));
process.exit(failed ? 1 : 0);
