import { build } from 'esbuild';
import { gzipSync } from 'node:zlib';

const LIMIT = 30720;
const bundle = async (entry) => {
  const r = await build({
    entryPoints: [entry],
    bundle: true,
    minify: true,
    format: 'esm',
    target: 'es2020',
    write: false,
    logLevel: 'error',
  });
  const out = r.outputFiles[0].contents;
  return { raw: out.length, gz: gzipSync(out, { level: 9 }).length, text: new TextDecoder().decode(out) };
};

const core = await bundle('src/index.ts');
console.log(`core minified: ${core.raw} bytes, gzip: ${core.gz} bytes (limit ${LIMIT})`);
const rt = await bundle('src/realtime.ts');
console.log(`realtime (separate entry) minified: ${rt.raw} bytes, gzip: ${rt.gz} bytes`);
if (/WebSocket|connectSocket|resume_failed/.test(core.text)) {
  console.error('FAIL: realtime code leaked into the core bundle');
  process.exit(1);
}
if (core.gz > LIMIT) {
  console.error('FAIL: core gzip size exceeds limit');
  process.exit(1);
}
