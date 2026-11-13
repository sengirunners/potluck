import { chromium } from 'playwright';
import { pathToFileURL } from 'node:url';
import { spawn } from 'node:child_process';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { once } from 'node:events';

export function options() { return { headless: true, chromiumSandbox: true }; }

async function serve(port, seconds) {
  const profile = await mkdtemp(join(tmpdir(), 'potluck-chromium-'));
  // Invoke bundled Chromium directly for CDP: its startup message identifies
  // this process's endpoint. An unrelated listener must never count as ready.
  // Omitting --no-sandbox retains Chromium's supported Linux sandbox.
  const child = spawn(chromium.executablePath(), [
    '--headless', '--no-first-run', '--no-default-browser-check',
    '--disable-dev-shm-usage', '--remote-debugging-address=127.0.0.1',
    `--remote-debugging-port=${port}`, `--user-data-dir=${profile}`, 'about:blank',
  ], { detached: true, stdio: ['ignore', 'ignore', 'pipe'] });
  const exited = once(child, 'exit').catch(error => [null, error.message]);
  // Register before readiness, including the initial browser-start window.
  const killGroup = signal => {
    if (!child.pid) return;
    try { process.kill(-child.pid, signal); }
    catch (error) { if (error.code !== 'ESRCH') throw error; }
  };
  let stopping;
  const stop = () => stopping ??= (async () => {
    if (!child.pid) return;
    // Only this launch's new process group: descendants cannot survive a
    // crashed browser parent or keep using its temporary profile.
    killGroup('SIGTERM');
    const force = setTimeout(() => killGroup('SIGKILL'), 5000);
    try {
      if (child.exitCode === null && child.signalCode === null) await exited;
    } finally {
      clearTimeout(force);
      killGroup('SIGKILL');
    }
  })();
  const signal = () => { void stop(); };
  process.once('SIGTERM', signal);
  process.once('SIGINT', signal);
  const lifetime = setTimeout(signal, seconds * 1000);
  let stderr = '';
  try {
    const endpoint = await new Promise((resolve, reject) => {
      const timeout = setTimeout(() => reject(new Error('Chromium readiness timed out')), 30000);
      const finish = (error, value) => {
        clearTimeout(timeout);
        child.stderr.removeListener('data', data);
        error ? reject(error) : resolve(value);
      };
      const data = chunk => {
        stderr = (stderr + chunk.toString()).slice(-16384);
        const match = stderr.match(/DevTools listening on (ws:\/\/127\.0\.0\.1:\d+\/devtools\/browser\/[a-zA-Z0-9-]+)/);
        if (match) finish(null, match[1]);
      };
      child.stderr.on('data', data);
      child.once('error', error => finish(error));
      child.once('exit', (code, signal) => finish(new Error(`Chromium exited before readiness (${code ?? signal}); check runtime user-namespace support`)));
    });
    const expected = new URL(endpoint);
    if (Number(expected.port) !== port) throw new Error('Chromium bound an unexpected port');
    const response = await fetch(`http://127.0.0.1:${port}/json/version`, { signal: AbortSignal.timeout(5000) });
    if (!response.ok || (await response.json()).webSocketDebuggerUrl !== endpoint) {
      throw new Error('CDP port belongs to another process');
    }
    if (stopping) throw new Error('Chromium stopped during startup');
    console.log(JSON.stringify({ ready: true, host: '127.0.0.1', port, access: 'control' }));
    const [code, signal] = await exited;
    if (!stopping && code !== 0) throw new Error(`Chromium exited unexpectedly (${code ?? signal})`);
  } finally {
    clearTimeout(lifetime);
    process.removeListener('SIGTERM', signal);
    process.removeListener('SIGINT', signal);
    await stop();
    await rm(profile, { recursive: true, force: true });
  }
}

export async function main(args) {
  if (process.getuid?.() === 0) throw new Error('run as the sandbox user; Chromium sandboxing requires a non-root user');
  const [command, ...rest] = args;
  if (command === 'screenshot' && rest.length === 2) {
    const url = new URL(rest[0]);
    if (!['http:', 'https:'].includes(url.protocol)) throw new Error('screenshot needs an HTTP(S) URL');
    const browser = await chromium.launch(options());
    const stop = () => { void browser.close(); };
    process.once('SIGTERM', stop);
    process.once('SIGINT', stop);
    try {
      const page = await browser.newPage({ viewport: { width: 1280, height: 720 } });
      await page.goto(url.href, { waitUntil: 'networkidle', timeout: 30000 });
      await page.screenshot({ path: rest[1], fullPage: true, timeout: 30000 });
    } finally {
      process.removeListener('SIGTERM', stop);
      process.removeListener('SIGINT', stop);
      await browser.close();
    }
    return;
  }
  if (command !== 'serve' || rest.length > 2) throw new Error('usage: potluck-browser serve [port=9222] [seconds=3600] | screenshot <url> <path.png>');
  const port = rest[0] === undefined ? 9222 : Number(rest[0]);
  const seconds = rest[1] === undefined ? 3600 : Number(rest[1]);
  if (!Number.isInteger(port) || port < 1024 || port > 65535) throw new Error('port must be 1024..65535');
  if (!Number.isInteger(seconds) || seconds < 1 || seconds > 86400) throw new Error('seconds must be 1..86400');
  await serve(port, seconds);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main(process.argv.slice(2)).catch(error => {
    // Chromium stderr is deliberately not copied: profile paths and visited
    // URLs are not helper diagnostics. Runtime failures stay explicit.
    console.error(error.message);
    process.exitCode = 1;
  });
}
