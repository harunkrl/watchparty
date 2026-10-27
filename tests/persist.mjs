// Persistence test: the server must restore now/queue/pos from STATE_FILE
// across a restart, and must drop files that vanished from disk out of the
// restored queue (restore filter). Self-contained: spawns server.js with a
// temp VIDEOS_DIR and a STATE_FILE inside it.
// Usage: node tests/persist.mjs

import { strict as assert } from 'node:assert';
import { spawn } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync, unlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const PORT = 18900 + Math.floor(Math.random() * 90);
const BASE = `http://127.0.0.1:${PORT}`;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const dir = mkdtempSync(join(tmpdir(), 'wp-persist-'));
writeFileSync(join(dir, 'x.mp4'), Buffer.alloc(10000, 1));
writeFileSync(join(dir, 'y.mp4'), Buffer.alloc(8000, 2));
const STATE_FILE = join(dir, 'state.json');

const start = () =>
  spawn(process.execPath, [join(ROOT, 'server.js')], {
    env: { ...process.env, PORT: String(PORT), VIDEOS_DIR: dir, STATE_FILE },
    stdio: ['ignore', 'pipe', 'pipe'],
  });

let server = start();

const post = async (obj) => {
  const res = await fetch(`${BASE}/cmd`, { method: 'POST', body: JSON.stringify(obj) });
  assert.equal(res.status, 204);
};

async function waitFor(want, ms = 5000, what = '') {
  const t0 = Date.now();
  while (Date.now() - t0 < ms) {
    try {
      if (await want()) return;
    } catch {}
    await sleep(100);
  }
  throw new Error(`timeout: ${what}`);
}
const waitUp = () => waitFor(async () => (await fetch(`${BASE}/files`)).ok, 5000, 'server up');
// "down" shows up as a refused connection (fetch throws), not as a status code
const waitDown = () =>
  waitFor(async () => {
    try {
      await fetch(`${BASE}/files`, { signal: AbortSignal.timeout(300) });
      return false; // still serving
    } catch {
      return true; // connection refused: down
    }
  }, 5000, 'server down');

// connect one SSE stream, grab the very first event (the hello), disconnect
async function grabHello() {
  const ctrl = new AbortController();
  try {
    const res = await fetch(`${BASE}/events?id=probe&nick=probe`, { signal: ctrl.signal });
    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let buf = '';
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      const i = buf.indexOf('\n\n');
      if (i >= 0) {
        const line = buf.split('\n').find((l) => l.startsWith('data: '));
        return JSON.parse(line.slice(6));
      }
    }
  } finally {
    ctrl.abort();
  }
  throw new Error('no hello event arrived');
}

const restart = async () => {
  server.kill();
  await waitDown();
  server = start();
  await waitUp();
};

const cleanup = (code) => {
  server.kill();
  rmSync(dir, { recursive: true, force: true });
  process.exit(code);
};

try {
  await waitUp();

  // build a live room: playing x.mp4 at 42s, y.mp4 queued
  await post({ type: 'load', from: 'm', name: 'x.mp4' });
  await post({ type: 'enqueue', from: 'm', name: 'y.mp4' });
  await post({ type: 'position', from: 'm', t: 42, p: false });

  // restart on the same STATE_FILE: the room must come back
  await restart();
  const h = await grabHello();
  assert.equal(h.type, 'hello');
  assert.equal(h.now, 'x.mp4', 'now not restored');
  assert.deepEqual(h.queue, ['y.mp4'], 'queue not restored');
  // pos travels with wall-clock age across the restart (tolerance ±3s)
  assert.ok(h.pos >= 42 && h.pos <= 45, `restored pos ${h.pos}, want ~42+age`);
  assert.equal(h.p, false, 'paused flag not restored');

  // second scenario: a queued file deleted from disk must not come back
  unlinkSync(join(dir, 'y.mp4'));
  await restart();
  const h2 = await grabHello();
  assert.deepEqual(h2.queue, [], 'vanished file stayed in the restored queue');
  assert.equal(h2.now, 'x.mp4', 'existing now was dropped by the restore filter');

  console.log('PASS: room state persisted across restarts, vanished files filtered');
  cleanup(0);
} catch (e) {
  console.error('FAIL:', e.message);
  cleanup(1);
}
