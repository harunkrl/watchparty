// Round-2 server audit: interactions between the round-1 fixes (roster on
// hello, Number.isFinite gates, advanceQueue filter, close-handler guard),
// state-machine pins for 3+ client flows, and idle-room queue semantics.
// Each scenario spawns its own server.js on a random test port — never the
// production port. Usage: node tests/audit2_server.mjs

import { strict as assert } from 'node:assert';
import { spawn } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync, unlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { stopServer, rmWithRetry } from './helpers.mjs';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

let portCounter = Math.floor(19900 + Math.random() * 90);

function startServer(files) {
  const dir = mkdtempSync(join(tmpdir(), 'wp-audit2-'));
  for (const f of files) writeFileSync(join(dir, f), Buffer.alloc(4096, 1));
  const port = portCounter++;
  const proc = spawn(process.execPath, [join(ROOT, 'server.js')], {
    env: { ...process.env, PORT: String(port), VIDEOS_DIR: dir, STATE_FILE: join(dir, 'state.json') },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let out = '';
  proc.stdout.on('data', (d) => (out += d));
  proc.stderr.on('data', (d) => (out += d));
  const base = `http://127.0.0.1:${port}`;
  const stop = async () => {
    await stopServer(proc, dir); // kill -> wait for the exit -> rm (F6/C1)
  };
  return { dir, base, proc, out: () => out, stop };
}

async function waitReady(base) {
  for (let i = 0; i < 100; i++) {
    try {
      const r = await fetch(`${base}/files`, { signal: AbortSignal.timeout(500) });
      if (r.ok) return;
    } catch {}
    await sleep(100);
  }
  throw new Error('server did not come up');
}

function sse(base, id, nick) {
  const events = [];
  const ctrl = new AbortController();
  (async () => {
    const res = await fetch(`${base}/events?id=${encodeURIComponent(id)}&nick=${encodeURIComponent(nick)}`, {
      signal: ctrl.signal,
    });
    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let buf = '';
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      while (true) {
        const i = buf.indexOf('\n\n');
        if (i < 0) break;
        const chunk = buf.slice(0, i);
        buf = buf.slice(i + 2);
        if (chunk.startsWith('data: ')) {
          try {
            events.push(JSON.parse(chunk.slice(6)));
          } catch {}
        }
      }
    }
  })().catch(() => {});
  return { events, close: () => ctrl.abort() };
}

const post = (base, obj) =>
  fetch(`${base}/cmd`, { method: 'POST', body: JSON.stringify(obj) });

function waitUntil(fn, ms = 3000, label = 'condition') {
  return new Promise((resolve, reject) => {
    const t0 = Date.now();
    const iv = setInterval(() => {
      let v;
      try {
        v = fn();
      } catch (e) {
        clearInterval(iv);
        return reject(e);
      }
      if (v) return (clearInterval(iv), resolve(v));
      if (Date.now() - t0 > ms) return (clearInterval(iv), reject(new Error(`timeout: ${label}`)));
    }, 25);
  });
}

const tests = [];
const test = (name, fn) => tests.push({ name, fn });

// REGRESSION (round-1 filter interaction): the advanceQueue disk filter runs
// before the state.now guard, so a skip/ended while NOTHING is loaded wipes
// the whole idle queue instead of leaving it waiting. Enqueue while idle,
// skip, enqueue more — the queue must survive room-idle commands.
test('idle-room skip keeps the queued files waiting', async () => {
  const s = startServer(['a.mkv', 'b.mkv']);
  try {
    await waitReady(s.base);
    const w = sse(s.base, 'W', 'Watch');
    await post(s.base, { type: 'enqueue', from: 'u', name: 'a.mkv' });
    await waitUntil(() => w.events.some((e) => e.type === 'queue' && e.queue.length === 1), 3000, 'enqueue');
    await post(s.base, { type: 'skip', from: 'u' }); // nothing loaded: must be a no-op
    await sleep(250);
    await post(s.base, { type: 'enqueue', from: 'u', name: 'b.mkv' });
    await waitUntil(() => w.events.some((e) => e.type === 'queue' && e.queue.length === 2), 3000, 'second enqueue');
    const last = [...w.events].reverse().find((e) => e.type === 'queue');
    assert.deepEqual(last.queue, ['a.mkv', 'b.mkv'], `idle skip wiped the queue: ${JSON.stringify(last.queue)}`);
    w.close();
  } finally {
    await s.stop();
  }
});

// State-machine pin (checklist B): master crash mid-film — the owner's SSE
// dies, the claim transfers to the next poster (a restarted master), and
// meanwhile follower skip/queue traffic still flows.
test('master crash mid-film: claim transfer and follower control still work', async () => {
  const s = startServer(['a.mkv', 'b.mkv']);
  try {
    await waitReady(s.base);
    const f1 = sse(s.base, 'F1', 'Follower1');
    const f2 = sse(s.base, 'F2', 'Follower2');
    await post(s.base, { type: 'load', from: 'm', name: 'a.mkv' });
    await post(s.base, { type: 'position', from: 'm', t: 60, p: false }); // m owns the clock mid-film
    const m = sse(s.base, 'm', 'M'); // the master's SSE connection
    await sleep(200);
    m.close(); // master crashes (network drop, process death)
    await waitUntil(() => f1.events.some((e) => e.type === 'bye' && e.nick === 'M'), 3000, 'bye for master');
    // the room is not dead: a follower can still skip (dual control)
    await post(s.base, { type: 'enqueue', from: 'F1', name: 'b.mkv' });
    await waitUntil(() => f2.events.some((e) => e.type === 'queue' && e.queue.length === 1), 3000, 'queue while masterless');
    await post(s.base, { type: 'skip', from: 'F2' });
    await waitUntil(() => f1.events.some((e) => e.type === 'load' && e.name === 'b.mkv'), 3000, 'skip while masterless');
    // a restarted master posting a position takes the clock without ceremony
    await post(s.base, { type: 'position', from: 'm', t: 5, p: false });
    const late = sse(s.base, 'L', 'Late');
    await waitUntil(() => late.events[0] && late.events[0].pos >= 5, 3000, 'successor clock visible');
    assert.ok(Math.abs(late.events[0].pos - 5) < 1.5, `pos ${late.events[0].pos}`);
    assert.ok(!f1.events.some((e) => e.type === 'notice'), 'no impostor notice on clean succession');
    late.close();
    f1.close();
    f2.close();
  } finally {
    await s.stop();
  }
});

// State-machine pin (checklist B): ended while paused advances the queue —
// a paused room that hits EOF still moves to the next file.
test('ended during pause advances to the next queued file', async () => {
  const s = startServer(['a.mkv', 'b.mkv']);
  try {
    await waitReady(s.base);
    const w = sse(s.base, 'W', 'Watch');
    await post(s.base, { type: 'load', from: 'm', name: 'a.mkv' });
    await post(s.base, { type: 'position', from: 'm', t: 10, p: false }); // m owns the clock
    await post(s.base, { type: 'enqueue', from: 'm', name: 'b.mkv' });
    await post(s.base, { type: 'pause', from: 'm' });
    await post(s.base, { type: 'ended', from: 'm' });
    await waitUntil(() => w.events.some((e) => e.type === 'load' && e.name === 'b.mkv'), 3000, 'auto-advance while paused');
    // state.pos was reset with the old file: a late joiner starts clean
    const late = sse(s.base, 'L', 'Late');
    await waitUntil(() => late.events[0]);
    assert.equal(late.events[0].now, 'b.mkv');
    assert.equal(late.events[0].pos, undefined, 'stale position leaked to a late joiner');
    late.close();
    w.close();
  } finally {
    await s.stop();
  }
});

// Pin (checklist B): SSE delivery is FIFO per connection — a position and a
// seek posted in this order must arrive in this order.
test('sync events preserve wire order per connection', async () => {
  const s = startServer(['a.mkv']);
  try {
    await waitReady(s.base);
    const w = sse(s.base, 'W', 'Watch');
    await post(s.base, { type: 'position', from: 'm', t: 1, p: false });
    await post(s.base, { type: 'seek', from: 'u', t: 55 });
    await post(s.base, { type: 'position', from: 'm', t: 2, p: false });
    await waitUntil(() => w.events.filter((e) => e.type === 'position').length >= 2, 3000, 'two positions');
    const kinds = w.events.filter((e) => ['position', 'seek'].includes(e.type)).map((e) => e.type);
    assert.deepEqual(kinds, ['position', 'seek', 'position'], `wire order ${JSON.stringify(kinds)}`);
    w.close();
  } finally {
    await s.stop();
  }
});

let failed = 0;
for (const { name, fn } of tests) {
  try {
    await fn();
    console.log(`PASS ${name}`);
  } catch (e) {
    failed++;
    console.error(`FAIL ${name}: ${e.message}`);
  }
}
if (failed) {
  console.error(`${failed}/${tests.length} round-2 audit tests failed`);
  process.exit(1);
}
console.log(`PASS: all ${tests.length} round-2 audit tests held`);
