// Server protocol/state audit (round 7g follow-up): adversarial tests for
// the latent issues the 3-terminal session surfaced. Each scenario spawns
// its own server.js on a random test port with a temp videos dir — never
// the production port. Usage: node tests/audit_server.mjs

import { strict as assert } from 'node:assert';
import { spawn } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync, unlinkSync, symlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

let portCounter = Math.floor(19500 + Math.random() * 300);

function startServer(files) {
  const dir = mkdtempSync(join(tmpdir(), 'wp-audit-'));
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
  const stop = () => {
    proc.kill();
    rmSync(dir, { recursive: true, force: true });
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

// Minimal SSE collector (same shape as smoke.mjs)
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

// BUG: the disconnect handler of a STALE socket deleted the roster entry
// and announced a bye for a user whose fresh same-id connection was still
// live (silent drop + instant reconnect). Late joiners then see a room
// without the reconnecting user.
test('stale same-id close keeps the live connection in the roster', async () => {
  const s = startServer(['a.mkv']);
  try {
    await waitReady(s.base);
    const a = sse(s.base, 'X', 'Ann');
    const b = sse(s.base, 'X', 'Ann'); // second tab, same id
    await sleep(250); // both registered server-side
    a.close(); // the stale socket dies last
    await sleep(300); // let its close handler run
    const c = sse(s.base, 'C', 'Cee');
    await waitUntil(() => c.events[0]);
    assert.equal(c.events[0].type, 'hello');
    const roster = c.events[0].roster;
    assert.ok(roster.includes('Ann'), `roster lost the live user: ${JSON.stringify(roster)}`);
    assert.equal(roster.filter((n) => n === 'Ann').length, 1, 'duplicate nick in roster');
    c.close();
    b.close();
  } finally {
    s.stop();
  }
});

// BUG: position accepted any JSON value as t; a broken/malicious clock
// owner poisoned state.pos with a string/object and every late joiner's
// hello.pos became a string (client-side decode failure).
test('position with a non-number t cannot poison the room state', async () => {
  const s = startServer(['a.mkv']);
  try {
    await waitReady(s.base);
    await post(s.base, { type: 'load', from: 'm', name: 'a.mkv' });
    await post(s.base, { type: 'position', from: 'm', t: '5e999broken', p: false });
    const j = sse(s.base, 'J', 'Joiner');
    await waitUntil(() => j.events[0]);
    const h = j.events[0];
    assert.ok(h.pos === undefined || typeof h.pos === 'number', `hello.pos is ${JSON.stringify(h.pos)}`);
    // the honest path still works end-to-end
    await post(s.base, { type: 'position', from: 'm', t: 42, p: false });
    const k = sse(s.base, 'K', 'Joiner2');
    await waitUntil(() => k.events[0] && typeof k.events[0].pos === 'number', 3000, 'numeric hello.pos');
    assert.ok(Math.abs(k.events[0].pos - 42) < 1.5, `pos ${k.events[0].pos}`);
    j.close();
    k.close();
  } finally {
    s.stop();
  }
});

// BUG: the cmd trace printed type/from/name raw, so ANSI escapes in a cmd
// reached the server log (journald) unescaped — log injection.
test('cmd trace never emits ANSI escapes into the log', async () => {
  const s = startServer(['a.mkv']);
  try {
    await waitReady(s.base);
    await post(s.base, {
      type: 'load',
      from: '\u001b[31mred\u001b[0m',
      name: '\u001b]0;pwned\u0007.mkv',
    });
    await sleep(300);
    assert.ok(!s.out().includes('\x1b'), 'ANSI escape reached the server log');
  } finally {
    s.stop();
  }
});

// BUG: auto-advance shifted whatever the queue held at enqueue time; a
// file deleted from disk in the meantime was loaded for everyone (404 for
// every client) and became state.now.
test('auto-advance skips queued files that vanished from disk', async () => {
  const s = startServer(['a.mkv', 'b.mkv', 'c.mkv']);
  try {
    await waitReady(s.base);
    const w = sse(s.base, 'W', 'Watch');
    await post(s.base, { type: 'position', from: 'm', t: 1, p: false }); // m owns the clock
    await post(s.base, { type: 'load', from: 'm', name: 'a.mkv' });
    await post(s.base, { type: 'enqueue', from: 'm', name: 'b.mkv' });
    await post(s.base, { type: 'enqueue', from: 'm', name: 'c.mkv' });
    await waitUntil(() => w.events.some((e) => e.type === 'queue' && e.queue.length === 2), 3000, 'queue');
    unlinkSync(join(s.dir, 'b.mkv')); // vanished after enqueue
    await post(s.base, { type: 'ended', from: 'm' });
    await waitUntil(() => w.events.some((e) => e.type === 'load' && e.name === 'c.mkv'), 3000, 'load c.mkv');
    const late = sse(s.base, 'L', 'Late');
    await waitUntil(() => late.events[0]);
    assert.equal(late.events[0].now, 'c.mkv', `late joiner got now=${late.events[0].now}`);
    assert.deepEqual(late.events[0].queue, []);
    // empty queue at EOF: no crash, no phantom load
    const loadsBefore = w.events.filter((e) => e.type === 'load').length;
    await post(s.base, { type: 'ended', from: 'm' });
    await sleep(250);
    assert.equal(w.events.filter((e) => e.type === 'load').length, loadsBefore, 'phantom load on empty queue');
    late.close();
    w.close();
  } finally {
    s.stop();
  }
});

// Regression (audit checklist): two-master demotion is one-time per id and
// the claim transfers after the owner's connection dies.
test('two-master notice is one-time and the clock succeeds cleanly', async () => {
  const s = startServer(['a.mkv']);
  try {
    await waitReady(s.base);
    const w = sse(s.base, 'W', 'Watch');
    await post(s.base, { type: 'position', from: 'm1', t: 10, p: false });
    await post(s.base, { type: 'position', from: 'm2', t: 99, p: false });
    await waitUntil(() => w.events.some((e) => e.type === 'notice' && e.text.includes('two masters')), 3000, 'notice');
    assert.ok(w.events.every((e) => e.type !== 'position' || e.t !== 99), 'impostor heartbeat relayed');
    await post(s.base, { type: 'position', from: 'm2', t: 99, p: false }); // repeat
    await sleep(250);
    assert.equal(w.events.filter((e) => e.type === 'notice').length, 1, 'notice not one-time');
    // owner's connection dies -> next poster takes over without a notice
    const posBefore = w.events.filter((e) => e.type === 'position').length;
    const m1 = sse(s.base, 'm1', 'M1'); // register then drop to kill the claim
    await sleep(150);
    m1.close();
    await waitUntil(() => w.events.some((e) => e.type === 'bye'), 3000, 'bye');
    await post(s.base, { type: 'position', from: 'm2', t: 60, p: false });
    await waitUntil(() => w.events.filter((e) => e.type === 'position').length > posBefore, 3000, 'successor relay');
    assert.equal(w.events.filter((e) => e.type === 'notice').length, 1, 'spurious notice on takeover');
    w.close();
  } finally {
    s.stop();
  }
});

// Regression (audit checklist): range/HEAD edges, 0-byte files, traversal,
// cmd body caps. All must hold alongside the audit fixes.
test('HTTP hardening edges hold', async () => {
  const s = startServer(['a.mkv', 'empty.mkv']);
  try {
    await waitReady(s.base);
    writeFileSync(join(s.dir, 'empty.mkv'), ''); // startServer writes 4096 bytes; this edge needs a real 0-byte file
    // 0-byte file: 200 with content-length 0 (a Range stream would throw)
    const zero = await fetch(`${s.base}/v/empty.mkv`);
    assert.equal(zero.status, 200);
    assert.equal((await zero.arrayBuffer()).byteLength, 0);
    const zeroHead = await fetch(`${s.base}/v/empty.mkv`, { method: 'HEAD' });
    assert.equal(zeroHead.status, 200);
    // explicit end is clamped to the file size
    const clamp = await fetch(`${s.base}/v/a.mkv`, { headers: { range: 'bytes=0-999999' } });
    assert.equal(clamp.status, 206);
    assert.equal(clamp.headers.get('content-range'), `bytes 0-4095/4096`);
    // start past EOF -> 416
    assert.equal((await fetch(`${s.base}/v/a.mkv`, { headers: { range: 'bytes=99999-' } })).status, 416);
    // suffix -0 -> 416
    assert.equal((await fetch(`${s.base}/v/a.mkv`, { headers: { range: 'bytes=-0' } })).status, 416);
    // unparsable range -> full 200 body
    assert.equal((await fetch(`${s.base}/v/a.mkv`, { headers: { range: 'bytes=zz' } })).status, 200);
    // HEAD answers 206 with no body
    const head = await fetch(`${s.base}/v/a.mkv`, { method: 'HEAD', headers: { range: 'bytes=0-9' } });
    assert.equal(head.status, 206);
    assert.equal((await head.arrayBuffer()).byteLength, 0);
    // traversal, dangling symlink, malformed encoding
    assert.equal((await fetch(`${s.base}/v/${encodeURIComponent('../../etc/passwd')}`)).status, 404);
    symlinkSync('/nonexistent-target', join(s.dir, 'dangling.mp4'));
    assert.equal((await fetch(`${s.base}/v/dangling.mp4`)).status, 404);
    assert.equal((await fetch(`${s.base}/v/%`)).status, 400);
    // cmd body cap and malformed JSON
    assert.equal((await post(s.base, { type: 'chat', from: 'x', nick: 'x', text: 'y'.repeat(9000) })).status, 413);
    assert.equal(
      (await fetch(`${s.base}/cmd`, { method: 'POST', body: 'not json' })).status,
      400
    );
    assert.equal((await fetch(`${s.base}/cmd`)).status, 404);
    assert.ok((await fetch(`${s.base}/files`)).ok, 'server alive after hostile requests');
  } finally {
    s.stop();
  }
});

// GUARD (not deterministically reproducible pre-fix): statSync could throw
// when the file vanished between realpath and stat, crashing the server.
// Hammer that window; the only acceptable outcomes are 200/206/404.
test('server survives stat/realpath TOCTOU races', async () => {
  const s = startServer(['a.mkv']);
  try {
    await waitReady(s.base);
    const f = join(s.dir, 'a.mkv');
    for (let i = 0; i < 120; i++) {
      try {
        unlinkSync(f);
      } catch {}
      writeFileSync(f, Buffer.alloc(4096, 1));
      const r = await fetch(`${s.base}/v/a.mkv`, { headers: { range: 'bytes=0-9' } }).catch(() => null);
      assert.ok(r && [200, 206, 404].includes(r.status), `unexpected status ${r && r.status}`);
      if (i % 30 === 0) assert.ok((await fetch(`${s.base}/files`)).ok, 'server died under stat race');
    }
  } finally {
    s.stop();
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
  console.error(`${failed}/${tests.length} audit tests failed`);
  process.exit(1);
}
console.log(`PASS: all ${tests.length} audit tests held`);
