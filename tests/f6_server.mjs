// F6 worker2: server pins for the Batch B fixes (B1..B5).
// Ports: 18500-18999 (worker2 band). Usage: node tests/f6_server.mjs
//
// Every pin here fails on the pre-F6 server (v1.5.1) and passes after the
// fix. The knobs (SSE_MAX_BUFFER, STALE_TTL_MS) exist so the behavior is
// observable in a test — they default to the production values.

import { strict as assert } from 'node:assert';
import { execFileSync, spawn } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync, readFileSync, existsSync, chmodSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import net from 'node:net';
import { stopServer } from './helpers.mjs';

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
// A prepared dir can be passed in (the FIFO pin needs the state path
// before the server writes to it).
function start(extraEnv = {}, prepared = null) {
  const dir = prepared || mkdtempSync(join(tmpdir(), 'wp-f6-'));
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
// F7: the server must still be ALIVE: a stale server on the same port
// answers /health with 200 and would send this suite into another
// server's room (the port-busy class failed as a confusing 404).
async function boot(s) {
  const t0 = Date.now();
  for (;;) {
    if (s.proc.exitCode !== null || s.proc.signalCode !== null) {
      throw new Error(`the server died (port busy?):\n${s.out()}`);
    }
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

// waitExit waits for a process that is still alive. A process that has
// ALREADY exited never emits 'exit' again -> the naive wait hangs forever
// (the port-busy class: the server died on EADDRINUSE).
async function waitExit(proc) {
  if (proc.exitCode !== null || proc.signalCode !== null) return;
  await new Promise((r) => proc.once('exit', r));
}

async function stop(s) {
  if (s.proc.exitCode === null && s.proc.signalCode === null) {
    const exited = new Promise((r) => s.proc.once('exit', r));
    s.proc.kill();
    await exited;
  }
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
    await waitExit(s.proc);
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
    await waitExit(s.proc);
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
      await boot({ proc: s2, dir: s.dir, out: () => out }); // the SECOND server: the first one is gone
      const health = await (await fetch(`${BASE}/health`)).json();
      assert.equal(health.now, 'movie one.mkv', `the backup did not restore the room: ${health.now}`);
      assert.ok(/backup/i.test(out), `the fallback was not logged loudly: ${out}`);
    } finally {
      s2.kill();
      await waitExit(s2);
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
    await waitExit(s.proc);
    assert.ok(/exit-time state write failed/i.test(s.out()), `the failed exit write was silent:\n${s.out()}`);
  } finally { rmSync(s.dir, { recursive: true, force: true }); }
});

// --- F7-1: a stuck flush chain cannot hang the exit ---------------------

// A hung fsync (a stalled disk, a hung network mount) keeps a libuv
// threadpool thread busy forever: the exit write joins that chain and
// the process used to ignore SIGTERM until the deploy timed out. The
// exit now races a drain timeout, skips the write loudly and hard-kills.
await pin('F7-1 a stuck flush chain cannot hang the exit', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'wp-f6-'));
  // the flush writes ${STATE_FILE}.tmp — a FIFO with no reader blocks the
  // write forever (the same shape as a hung fsync)
  execFileSync('mkfifo', [join(dir, 'state.json.tmp')]);
  const s = start({ EXIT_DRAIN_MS: '300' }, dir);
  try {
    await boot(s);
    await post({ type: 'load', from: 'm', name: 'movie one.mkv' });
    await sleep(2500); // the debounced flush fires and blocks on the FIFO
    const t0 = Date.now();
    const exited = new Promise((r) => s.proc.once('exit', r));
    s.proc.kill('SIGTERM');
    // a watchdog: a regression must FAIL, not hang the suite (the C1 spirit).
    // The drain timeout is driven down to 300ms, so 5s is a 16x margin that
    // survives a loaded (shared) box — the normal exit lands in ~0.3s.
    const gone = await Promise.race([exited.then(() => true), sleep(5000).then(() => false)]);
    const took = Date.now() - t0;
    assert.ok(gone, `the server did not exit within 5s (took ${took}ms) — the exit still hangs\n${s.out()}`);
    assert.ok(/exit write skipped/i.test(s.out()), `the skipped exit write was silent:\n${s.out()}`);
  } finally {
    try { s.proc.kill('SIGKILL'); } catch {} // a failed pin must not leak the port
    rmSync(dir, { recursive: true, force: true });
  }
});

// --- F7-2: stopServer reaps a server that ignores SIGTERM ---------------

// The wait for the exit must be BOUNDED: a stubborn server hung the
// suite forever (8 of the suites have no watchdog of their own).
await pin('F7-2 stopServer reaps a server that ignores SIGTERM', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'wp-f6-'));
  const stub = spawn(process.execPath, ['-e', "process.on('SIGTERM', () => {}); console.log('ready'); setInterval(() => {}, 1000);"], { stdio: ['ignore', 'pipe', 'inherit'] });
  try {
    await new Promise((r) => stub.stdout.once('data', r)); // the handler is armed
    const t0 = Date.now();
    await stopServer(stub, dir);
    const took = Date.now() - t0;
    assert.ok(took < 9000, `stopServer waited ${took}ms — a stubborn server hangs the suite`);
    assert.equal(stub.signalCode, 'SIGKILL', `the stubborn server was not SIGKILLed: ${stub.signalCode}`);
    assert.ok(!existsSync(dir), 'the temp dir survived the stop');
  } finally {
    try { stub.kill('SIGKILL'); } catch {}
    rmSync(dir, { recursive: true, force: true });
  }
});

// --- F7-3: a failed flush is retried when the disk recovers -------------

// The failure used to latch persistence off for the process lifetime:
// the room state was lost until the next restart.
await pin('F7-3 a failed flush is retried when the disk recovers', async () => {
  const s = start();
  const file = join(s.dir, 'state.json');
  try {
    await boot(s);
    chmodSync(s.dir, 0o500); // the state dir is read-only: the flush fails
    await post({ type: 'load', from: 'm', name: 'movie one.mkv' });
    await sleep(3000);
    assert.ok(!existsSync(file), 'the read-only dir somehow took the write');
    assert.ok(/state persistence failed/i.test(s.out()), `the failed persist was silent:\n${s.out()}`);
    chmodSync(s.dir, 0o700); // the disk recovers
    await waitUntil(() => existsSync(file), 20000, 'the retried flush wrote the state');
    const saved = JSON.parse(readFileSync(file, 'utf8'));
    assert.equal(saved.now, 'movie one.mkv', `the retried flush wrote the wrong state: ${saved.now}`);
  } finally {
    chmodSync(s.dir, 0o700);
    await stop(s); // the port must be free for the next pin
  }
});

// --- F7-4: every cmd type normalizes the nick ---------------------------

// The position event was the odd one out: a client-supplied nick was
// relayed verbatim (ANSI escapes included).
await pin('F7-4 every cmd type normalizes the nick', async () => {
  const s = start();
  try {
    await boot(s);
    // an observer: a poster never receives its own event
    const obs = net.connect(PORT, '127.0.0.1');
    await new Promise((r) => obs.on('connect', r));
    obs.write('GET /events?id=obs7&nick=obs7 HTTP/1.1\r\nHost: x\r\n\r\n');
    let buf = '';
    obs.on('data', (d) => { buf += d.toString(); });
    obs.on('error', () => {});
    await sleep(300);
    await post({ type: 'position', from: 'm', t: 1, p: false, file: 'movie one.mkv', nick: 'evil\u001b[2Jn' });
    await waitUntil(() => buf.includes('"position"'), 5000, 'the position event was relayed');
    const line = buf.split('\n').find((l) => l.includes('"position"'));
    // the PARSED nick must be clean: the wire carries the ESC as the six
    // chars "\u001b", so a raw-byte check would pass even unfixed
    const ev = JSON.parse(line.slice(line.indexOf('data: ') + 6));
    assert.ok(typeof ev.nick === 'string', `the position event lost its nick: ${JSON.stringify(ev)}`);
    assert.ok(!ev.nick.includes('\u001b'), `the ANSI escape survived the relay: ${JSON.stringify(ev.nick)}`);
    assert.ok(ev.nick.includes('evil'), `the nick was dropped instead of cleaned: ${JSON.stringify(ev.nick)}`);
    obs.destroy();
  } finally { await stop(s); }
});

if (failures.length) {
  console.error(`\n${failures.length} pin(s) failed:`);
  for (const f of failures) console.error(`  - ${f}`);
  process.exit(1);
}
console.log('\nall server pins passed');
