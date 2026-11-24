import { build } from 'esbuild';
import { gzipSync } from 'node:zlib';

const LIMIT = 30720;
const r = await build({
  entryPoints: ['src/index.ts'],
  bundle: true,
  minify: true,
  format: 'esm',
  target: 'es2020',
  write: false,
  logLevel: 'error',
});
const out = r.outputFiles[0].contents;
const gz = gzipSync(out, { level: 9 }).length;
console.log(`core minified: ${out.length} bytes, gzip: ${gz} bytes (limit ${LIMIT})`);
if (gz > LIMIT) {
  console.error('FAIL: core gzip size exceeds limit');
  process.exit(1);
}
