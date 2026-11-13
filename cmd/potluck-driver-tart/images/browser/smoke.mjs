// Real browser/app/CDP/lifecycle acceptance inside the selected OCI boundary.
import { chromium } from 'playwright';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { mkdtemp, readFile, readdir, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const dir = await mkdtemp(join(tmpdir(), 'potluck-browser-smoke-'));
const helper = '/opt/potluck-browser/browser.mjs';
const children = new Set();
function run(args) {
  const child = spawn(process.execPath, [helper, ...args], { env: { ...process.env, TMPDIR: dir }, stdio: ['ignore', 'pipe', 'pipe'] });
  children.add(child);
  let stdout = '', stderr = '';
  child.stdout.on('data', data => { stdout += data; });
  child.stderr.on('data', data => { stderr += data; });
  const exit = once(child, 'exit').then(([code, signal]) => {
    children.delete(child);
    return { code, signal, stdout, stderr };
  });
  const ready = new Promise((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error('helper readiness timeout')), 35000);
    child.stdout.on('data', () => { if (stdout.includes('"ready":true')) { clearTimeout(timeout); resolve(); } });
    exit.then(result => { clearTimeout(timeout); reject(new Error(`helper exited before ready: ${JSON.stringify(result)}`)); });
  });
  // Some calls intentionally fail before ready; their exit status is asserted.
  ready.catch(() => {});
  return { child, exit, ready };
}
async function gone(port) {
  await assert.rejects(fetch(`http://127.0.0.1:${port}/json/version`, { signal: AbortSignal.timeout(1000) }));
}
function png(bytes) {
  assert.deepEqual(bytes.subarray(0, 8), Buffer.from([137,80,78,71,13,10,26,10]));
}
const server = createServer((req, res) => {
  res.setHeader('Content-Type', 'text/html');
  res.end(`<button onclick="${process.env.BROKEN_APP ? '' : "this.textContent='Passed'"}">Run test</button>`);
});
server.listen(0, '127.0.0.1');
await once(server, 'listening');
let browser;
try {
  const invalid = await run(['serve', '9222', '0']).exit;
  assert.notEqual(invalid.code, 0);
  assert.match(invalid.stderr, /seconds must be/);
  const primary = run(['serve', '9222', '120']);
  await primary.ready;
  // Occupied-port startup must fail and leave the rightful browser alive.
  const collision = await run(['serve', '9222', '2']).exit;
  assert.notEqual(collision.code, 0);
  assert.ok(!collision.stdout.includes('"ready":true'));
  browser = await chromium.connectOverCDP('http://127.0.0.1:9222');
  const context = await browser.newContext();
  const page = await context.newPage();
  const url = `http://127.0.0.1:${server.address().port}`;
  await page.goto(url);
  await page.getByRole('button', { name: 'Run test', exact: true }).click();
  assert.equal(await page.locator('button').textContent(), 'Passed');
  const screenshot = process.env.SCREENSHOT_PATH || join(dir, 'app.png');
  await page.screenshot({ path: screenshot });
  png(await readFile(screenshot));
  await context.close();
  await browser.close();
  browser = undefined;
  primary.child.kill('SIGTERM');
  assert.equal((await primary.exit).code, 0);
  await gone(9222);
  const shot = join(dir, 'standalone.png');
  const captured = await run(['screenshot', url, shot]).exit;
  assert.equal(captured.code, 0, captured.stderr);
  png(await readFile(shot));
  const bounded = run(['serve', '9223', '5']);
  await bounded.ready;
  assert.equal((await bounded.exit).code, 0);
  await gone(9223);
  assert.deepEqual((await readdir(dir)).filter(name => name.startsWith('potluck-chromium-')), []);
  console.log('PASS: sandboxed Chromium, CDP, app click, PNG, port collision, SIGTERM, timeout and profile cleanup');
} finally {
  if (browser) await browser.close();
  for (const child of children) child.kill('SIGTERM');
  await Promise.all([...children].map(child => once(child, 'exit')));
  server.close();
  await rm(dir, { recursive: true, force: true });
}
