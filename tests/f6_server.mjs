// F6 worker2: server pins for the Batch B fixes (B1..B5).
// Ports: 18500-18999 (worker2 band). Usage: node tests/f6_server.mjs
//
// Every pin here fails on the pre-F6 server (v1.5.1) and passes after the
// fix. The knobs (SSE_MAX_BUFFER, STALE_TTL_MS) exist so the behavior is
// observable in a test — they default to the production values.

import { strict as assert } from 'node:assert';
import { spawn } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import net from 'node:net';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const PORT = 18500 + Math.floor(Math.random() * 400);
const BASE = `http://127.0.0.1:${PORT}`;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function waitUntil(fn, ms = 5000, label = 'condition') {
  return new Promise((resolve, reject) => {
    const t0 = Date.now();
    const iv = setInterval(async () => {
      let v;
      try { v = await fn(); } catch (e) { clearInterval(iv); return reject(e); }
      if (v) return (clearInterval(iv), resolve(v));
      if (Date.now() - t0 > ms) return (clearInterval(iv), reject(new Error(`timeout: ${label}`)));
    }, 25);
  });
}

// start boots a server with the given env overrides.
function start(extraEnv = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'wp-f6-'));
  writeFileSync(join(dir, 'movie one.mkv'), Buffer.alloc(4096, 1));
  writeFileSync(join(dir, 'movie two.mkv'), Buffer.alloc(4096, 2));
  const proc = spawn(process.execPath, [join(ROOT, 'server.js')], {
    env: { ...process.env, PORT: String(PORT), VIDEOS_DIR: dir, STATE_FILE: join(dir, 'state.json'), ...extraEnv },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let out = '';
  proc.stdout.on('data', (d) => (out += d));
  proc.stderr.on('data', (d) => (out += d));
  return { dir, proc, out: () => out };
}

// boot waits for the server to answer and shows its output when it does not.
// A transient connection error is retried (the first fetch right after the
// listen can fail) — it is not a test failure.
async function boot(s) {
  const t0 = Date.now();
  for (;;) {
    try {
      const r = await fetch(`${BASE}/health`);
      if (r.ok) return;
    } catch { /* not up yet */ }
    if (Date.now() - t0 > 8000) break;
    await sleep(100);
  }
  try {
    throw new Error('the server did not come up');
  } catch (e) {
    throw new Error(`${e.message}\nserver output:\n${s.out()}`);
  }
}

async function post(obj) {
  const res = await fetch(`${BASE}/cmd`, { method: 'POST', body: JSON.stringify(obj) });
  assert.equal(res.status, 204);
}

async function stop(s) {
  s.proc.kill();
  await new Promise((r) => s.proc.on('exit', r));
  rmSync(s.dir, { recursive: true, force: true });
}

const failures = [];
async function pin(name, fn) {
  try { await fn(); console.log(`ok   ${name}`); }
  catch (e) { failures.push(`${name}: ${e.message}`); console.log(`FAIL ${name}: ${e.message}`); }
}

// --- B1: a client that never drains gets dropped -------------------------

// The server buffered every room event in that socket: the only
// unbounded memory path on the server.
await pin('B1 a client that stops reading is dropped', async () => {
  const s = start({ SSE_MAX_BUFFER: '4096' });
  try {
    await boot(s);
    // a raw socket that never reads: the kernel buffers fill, then the
    // server's own buffer grows past the threshold
    const stuck = net.connect(PORT, '127.0.0.1');
    await new Promise((r) => stuck.on('connect', r));
    stuck.write('GET /events?id=stuck&nick=Stuck HTTP/1.1\r\nHost: x\r\n\r\n');
    let closed = false;
    stuck.on('close', () => (closed = true));
    stuck.on('error', () => {});
    await waitUntil(async () => (await (await fetch(`${BASE}/health`)).json()).listeners >= 1, 5000, 'the stuck client joined');
    // flood the room over ONE keep-alive socket (the kernel buffers hold a
    // few MB, so the server-side buffer only grows past the threshold
    // after several MB): every event is broadcast to the stuck client
    const flood = net.connect(PORT, '127.0.0.1');
    await new Promise((r) => flood.on('connect', r));
    flood.on('data', () => {}); // drain the responses, the SSE socket stays stuck
    const body = JSON.stringify({ type: 'chat', nick: 'x', text: 'y'.repeat(450), from: 'flood' });
    const head = `POST /cmd HTTP/1.1\r\nHost: x\r\nContent-Length: ${body.length}\r\n\r\n`;
    // the backpressure of the flood socket itself is irrelevant here: the
    // point is the VOLUME the server tries to hand to the stuck client
    for (let i = 0; i < 60000; i++) flood.write(head + body);
    await waitUntil(() => s.out().includes('stopped reading'), 20000, 'the stuck client was dropped');
    flood.destroy();
    const health = await (await fetch(`${BASE}/health`)).json();
    assert.equal(health.listeners, 0, `the dropped client is still counted: ${health.listeners}`);
    // the server survived and still serves
    assert.ok((await fetch(`${BASE}/files`)).ok, 'the server did not survive');
    assert.ok(s.out().includes('stopped reading'), 'the drop was not logged');
  } finally { await stop(s); }
});

// --- B2: the wrong-file gate closes for steering posts too ---------------

// The stale master could still drag the room back with a load, or pause
// a room that moved on.
await pin('B2 a stale client cannot steer the room', async () => {
  const s = start({ STALE_TTL_MS: '400' });
  try {
    await boot(s);
    // the master owns the clock and loads the first film
    await post({ type: 'position', from: 'm', t: 1, p: false, file: 'movie one.mkv' });
    await post({ type: 'load', from: 'm', name: 'movie one.mkv' });
    let health = await (await fetch(`${BASE}/health`)).json();
    assert.equal(health.now, 'movie one.mkv', 'the film did not load');
    // an old master still driving the previous film: its position names a
    // different file -> the gate hits and it becomes stale
    await post({ type: 'position', from: 'old', t: 2, p: false, file: 'movie two.mkv' });
    // its steering posts must be dropped
    await post({ type: 'pause', from: 'old', p: true });
    await post({ type: 'load', from: 'old', name: 'movie two.mkv' });
    health = await (await fetch(`${BASE}/health`)).json();
    assert.equal(health.now, 'movie one.mkv', `the stale client moved the room: ${health.now}`);
    assert.equal(health.paused, false, `the stale client paused the room: ${health.paused}`);
    // the ACTIVE master is unaffected
    await post({ type: 'pause', from: 'm', p: true });
    health = await (await fetch(`${BASE}/health`)).json();
    assert.equal(health.paused, true, 'the active master lost its pause');
    // the TTL restores the stale client (no clearing exception)
    await sleep(700);
    await post({ type: 'pause', from: 'old', p: true });
    health = await (await fetch(`${BASE}/health`)).json();
    assert.equal(health.paused, true, `the TTL did not restore the stale client: ${health.paused}`);
  } finally { await stop(s); }
});

// --- B3: the exit write joins the flush chain ----------------------------

// The exit-time write raced the async chain on the same .tmp: the room
// state could be lost or corrupted.
await pin('B3 the exit write lands and the state file stays valid', async () => {
  const s = start();
  try {
    await boot(s);
    await post({ type: 'position', from: 'm', t: 1, p: false, file: 'movie one.mkv' });
    await post({ type: 'load', from: 'm', name: 'movie one.mkv' });
    await post({ type: 'enqueue', from: 'm', name: 'movie two.mkv' });
    // kill right away: the exit-time write must land on its own
    s.proc.kill();
    await new Promise((r) => s.proc.on('exit', r));
    const raw = readFileSync(join(s.dir, 'state.json'), 'utf8');
    const saved = JSON.parse(raw); // must be valid JSON: no interleaving
    assert.equal(saved.now, 'movie one.mkv', `the exit write lost the film: ${raw}`);
    assert.deepEqual(saved.queue, ['movie two.mkv'], `the exit write lost the queue: ${raw}`);
  } finally { rmSync(s.dir, { recursive: true, force: true }); }
});

// --- B4: a corrupted state.json falls back to the backup ------------------

await pin('B4 a corrupted state.json falls back to the backup', async () => {
  const s = start();
  try {
    await boot(s);
    await post({ type: 'position', from: 'm', t: 1, p: false, file: 'movie one.mkv' });
    await post({ type: 'load', from: 'm', name: 'movie one.mkv' });
    await sleep(2500); // the debounced flush lands
    await post({ type: 'position', from: 'm', t: 2, p: false, file: 'movie one.mkv' });
    await sleep(2500); // the second flush backs up the first snapshot
    s.proc.kill();
    await new Promise((r) => s.proc.on('exit', r));
    const file = join(s.dir, 'state.json');
    writeFileSync(file, '{ broken');
    // a second boot must recover the room from the backup
    const s2 = spawn(process.execPath, [join(ROOT, 'server.js')], {
      env: { ...process.env, PORT: String(PORT), VIDEOS_DIR: s.dir, STATE_FILE: file },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let out = '';
    s2.stdout.on('data', (d) => (out += d));
    s2.stderr.on('data', (d) => (out += d));
    try {
      await boot(s);
      const health = await (await fetch(`${BASE}/health`)).json();
      assert.equal(health.now, 'movie one.mkv', `the backup did not restore the room: ${health.now}`);
      assert.ok(/backup/i.test(out), `the fallback was not logged loudly: ${out}`);
    } finally {
      s2.kill();
      await new Promise((r) => s2.on('exit', r));
    }
  } finally { rmSync(s.dir, { recursive: true, force: true }); }
});

// --- C1: the exit-time write failure is never silent ---------------------

await pin('C1 a failed exit-time write is logged', async () => {
  const s = start();
  try {
    await boot(s);
    await post({ type: 'position', from: 'm', t: 1, p: false, file: 'movie one.mkv' });
    await post({ type: 'load', from: 'm', name: 'movie one.mkv' });
    rmSync(s.dir, { recursive: true, force: true }); // the dir vanishes under the server
    s.proc.kill();
    await new Promise((r) => s.proc.on('exit', r));
    assert.ok(/exit-time state write failed/i.test(s.out()), `the failed exit write was silent:\n${s.out()}`);
  } finally { rmSync(s.dir, { recursive: true, force: true }); }
});

if (failures.length) {
  console.error(`\n${failures.length} pin(s) failed:`);
  for (const f of failures) console.error(`  - ${f}`);
  process.exit(1);
}
console.log('\nall server pins passed');
