// F1 round server pins (worker1): w1-1 crash guard, w1-2 notice sanitize,
// w1-4+I2 end-of-queue broadcast, m2-S6 SIGINT persist, inviteHosts ranking
// unit test (#5 gap), wanlink silence-mode knob (C-prep).
// Each scenario spawns its own server.js on a random test port — never the
// production port. Usage: node tests/f1_server.mjs
import { strict as assert } from 'node:assert';
import { spawn } from 'node:child_process';
import { createRequire } from 'node:module';
import http from 'node:http';
import { mkdtempSync, writeFileSync, rmSync, unlinkSync, mkdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { Proxy } from './wanlink.mjs';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let portCounter = Math.floor(19100 + Math.random() * 90);

// 'exit' fires once; a kill() on an already-exited child would hang the await
const waitExit = (proc) =>
  proc.exitCode === null ? new Promise((r) => proc.on('exit', r)) : Promise.resolve();

function startServer(files) {
  const dir = mkdtempSync(join(tmpdir(), 'wp-f1-'));
  for (const f of files) writeFileSync(join(dir, f), Buffer.alloc(4096, 1));
  const port = portCounter++;
  const proc = spawn(process.execPath, [join(ROOT, 'server.js')], {
    env: { ...process.env, PORT: String(port), VIDEOS_DIR: dir, STATE_FILE: join(dir, 'state.json') },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let err = '';
  proc.stderr.on('data', (d) => (err += d));
  const base = `http://127.0.0.1:${port}`;
  return {
    dir, base, port, proc,
    stop: async () => {
      proc.kill();
      await waitExit(proc);
      rmSync(dir, { recursive: true, force: true });
    },
    stderr: () => err,
  };
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

async function waitDown(base) {
  for (let i = 0; i < 50; i++) {
    try {
      await fetch(`${base}/files`, { signal: AbortSignal.timeout(300) });
    } catch { return; }
    await sleep(100);
  }
  throw new Error('server still up');
}

async function post(base, obj) {
  const res = await fetch(`${base}/cmd`, { method: 'POST', body: JSON.stringify(obj) });
  return res.status;
}

// SSE collector over raw http so the socket can be hard-destroyed
function rawSse(base, id) {
  const events = [];
  let res = null;
  let buf = '';
  http.get(`${base}/events?id=${encodeURIComponent(id)}`, (r) => {
    res = r;
    r.setEncoding('utf8');
    r.on('data', (c) => {
      buf += c;
      while (true) {
        const i = buf.indexOf('\n\n');
        if (i < 0) break;
        const chunk = buf.slice(0, i);
        buf = buf.slice(i + 2);
        if (chunk.startsWith('data: ')) events.push(JSON.parse(chunk.slice(6)));
      }
    });
  });
  return {
    events,
    first: async () => {
      for (let t = 0; t < 60 && !events.length; t++) await sleep(50);
      return events[0];
    },
    close: () => res && res.destroy(),
  };
}

async function firstHello(base, id) {
  const c = rawSse(base, id);
  const h = await c.first();
  c.close();
  if (!h) throw new Error('no hello arrived');
  return h;
}

function waitUntil(fn, ms = 3000, label = 'condition') {
  return new Promise((resolve, reject) => {
    const t0 = Date.now();
    const iv = setInterval(() => {
      let v;
      try { v = fn(); } catch (e) { clearInterval(iv); return reject(e); }
      if (v) return (clearInterval(iv), resolve(v));
      if (Date.now() - t0 > ms) return (clearInterval(iv), reject(new Error(`timeout: ${label}`)));
    }, 25);
  });
}

const results = [];
async function scenario(name, fn) {
  try {
    await fn();
    results.push(['PASS', name]);
  } catch (e) {
    results.push(['FAIL', `${name} — ${e.message}`]);
  }
}

// [w1-1 + I-3] VIDEOS_DIR vanishing at runtime must not kill the server:
// every listing path (/files /subs /library, load/enqueue gates, advanceQueue
// via skip/ended) guards the readdir. list() answers null on failure: the
// HTTP listings map null -> [], while advanceQueue KEEPS the queue on a
// transient read failure (no filter, no end-of-queue notice) — the room must
// not lose its queue because one readdir hiccupped. The dir may also come
// back (remount) without a restart.
await scenario('[w1-1+I-3] vanished videos dir: empty listings, queue kept, server alive', async () => {
  const s = startServer(['a.mkv', 'b.mkv']);
  try {
    await waitReady(s.base);
    const w = rawSse(s.base, 'w');
    await sleep(150);
    await post(s.base, { type: 'position', from: 'm', t: 1, p: false }); // m owns the clock
    await post(s.base, { type: 'load', from: 'm', name: 'a.mkv' });
    await post(s.base, { type: 'enqueue', from: 'm', name: 'b.mkv' });
    await waitUntil(() => w.events.some((e) => e.type === 'queue' && e.queue.length === 1), 3000, 'enqueue');
    rmSync(s.dir, { recursive: true, force: true }); // unmount/delete simulation
    const files = await (await fetch(`${s.base}/files`)).json();
    assert.deepEqual(files, [], '/files must answer [] after the dir vanished');
    const subs = await (await fetch(`${s.base}/subs`)).json();
    assert.deepEqual(subs, []);
    const lib = await (await fetch(`${s.base}/library`)).json();
    assert.deepEqual(lib, []);
    // skip on an unreadable dir: the queue is KEPT (null turn skips the
    // filter, no end-of-queue notice) and advances into the queued file
    assert.equal(await post(s.base, { type: 'skip', from: 'm' }), 204);
    await waitUntil(() => w.events.some((e) => e.type === 'load' && e.name === 'b.mkv'), 3000, 'queue kept across readdir failure');
    await sleep(250);
    assert.ok(!w.events.some((e) => e.type === 'notice' && e.text.includes('end of queue')),
      'a transient readdir failure must not announce end of queue');
    assert.equal(await post(s.base, { type: 'enqueue', from: 'm', name: 'a.mkv' }), 204); // gate must not throw
    await sleep(200);
    const h = await (await fetch(`${s.base}/health`)).json(); // must still be alive
    assert.ok(h.listeners >= 1, 'health must answer after dir loss');
    // recovery: the dir can come back (remount) without a restart
    mkdirSync(s.dir, { recursive: true });
    writeFileSync(join(s.dir, 'a.mkv'), Buffer.alloc(10, 1));
    await waitUntil(async () => (await (await fetch(`${s.base}/files`)).json()).length === 1, 2000, 'recovery');
    w.close();
  } finally {
    await s.stop();
  }
});

// [w1-2] the impostor notice embeds client-supplied ids in its text: both
// fallbacks must pass through clean() — raw ANSI/OSC must never reach a TUI.
// The `from` FIELD stays raw (the client compares it to its own id to demote).
await scenario('[w1-2] impostor notice text is sanitized (no ESC/OSC)', async () => {
  const s = startServer(['a.mkv']);
  try {
    await waitReady(s.base);
    const w = rawSse(s.base, 'w');
    await sleep(150);
    await post(s.base, { type: 'position', from: 'owner', t: 5, p: false });
    await sleep(150);
    await post(s.base, { type: 'position', from: '\u001b]0;pwned-title\u0007ghost', t: 99, p: false });
    await waitUntil(() => w.events.some((e) => e.type === 'notice' && e.text.includes('ghost')), 3000, 'notice');
    const notice = w.events.find((e) => e.type === 'notice' && e.text.includes('ghost'));
    assert.ok(!/\x1b/.test(notice.text), `raw escape in notice text: ${JSON.stringify(notice.text)}`);
    assert.ok(notice.text.includes('two masters'), `notice must stay informative: ${notice.text}`);
    assert.equal(notice.from, '\u001b]0;pwned-title\u0007ghost', 'the from FIELD stays raw for self-demotion');
    w.close();
  } finally {
    await s.stop();
  }
});

// [w1-4 + I2] end of queue must be visible to CONNECTED clients: the disk
// filter emptying the queue broadcasts the empty queue + a notice; a skip on
// a finished file with an empty queue is never a silent no-op. The idle-room
// "nothing playing" branch keeps its own text and queue (audit2 pins it).
await scenario('[w1-4+I2] end of queue: empty-queue broadcast + notice, no silent skip', async () => {
  const s = startServer(['a.mkv', 'b.mkv']);
  try {
    await waitReady(s.base);
    const w = rawSse(s.base, 'w');
    await sleep(150);
    await post(s.base, { type: 'position', from: 'm', t: 1, p: false }); // m owns the clock
    await post(s.base, { type: 'load', from: 'm', name: 'a.mkv' });
    await post(s.base, { type: 'enqueue', from: 'm', name: 'b.mkv' });
    await waitUntil(() => w.events.some((e) => e.type === 'queue' && e.queue.length === 1), 3000, 'enqueue');
    const loadsBefore = w.events.filter((e) => e.type === 'load').length;
    unlinkSync(join(s.dir, 'b.mkv')); // the queued file vanishes from disk
    await post(s.base, { type: 'ended', from: 'm' }); // EOF: filter empties the queue
    await waitUntil(() => w.events.some((e) => e.type === 'queue' && e.queue.length === 0), 3000, 'empty queue broadcast');
    await waitUntil(() => w.events.some((e) => e.type === 'notice' && e.text.includes('end of queue')), 3000, 'end-of-queue notice');
    // no phantom advance, state.now unchanged
    await sleep(300);
    assert.equal(w.events.filter((e) => e.type === 'load').length, loadsBefore, 'end of queue must not load anything');
    // a later skip on the finished file + ALREADY empty queue stays quiet
    // (I-4: the notice rides the emptying transition only — no repeated
    // messages for a queue that is already empty)
    const noticesAfterEmpty = w.events.filter((e) => e.type === 'notice' && e.text.includes('end of queue')).length;
    await sleep(2200); // let the shared notice cooldown expire
    await post(s.base, { type: 'skip', from: 'm' });
    await sleep(400);
    assert.equal(
      w.events.filter((e) => e.type === 'notice' && e.text.includes('end of queue')).length,
      noticesAfterEmpty, 'skip on an already-empty queue must not repeat the notice'
    );
    assert.equal(w.events.filter((e) => e.type === 'load').length, loadsBefore, 'skip on empty queue must not load anything');
    w.close();
  } finally {
    await s.stop();
  }
});

// idle-room regression guard for the same fall-through: the "nothing playing"
// branch keeps its own text and must NOT wipe the queue (audit2 re-pin).
await scenario('[w1-4+I2 guard] idle-room skip keeps "nothing playing" + the queue', async () => {
  const s = startServer(['c.mkv']);
  try {
    await waitReady(s.base);
    const w = rawSse(s.base, 'w2');
    await sleep(150);
    await post(s.base, { type: 'enqueue', from: 'u', name: 'c.mkv' });
    await waitUntil(() => w.events.some((e) => e.type === 'queue' && e.queue.length === 1), 3000, 'idle enqueue');
    await post(s.base, { type: 'skip', from: 'u' });
    await waitUntil(() => w.events.some((e) => e.type === 'notice' && e.text.includes('nothing playing')), 3000, 'idle notice');
    const lastQ = [...w.events].reverse().find((e) => e.type === 'queue');
    assert.deepEqual(lastQ.queue, ['c.mkv'], 'idle skip must not wipe the queue');
    w.close();
  } finally {
    await s.stop();
  }
});

// [m2-S6] Ctrl-C (SIGINT) on a manual run must persist the room like SIGTERM:
// kill within the 2s write-behind window, restart, the room must be back.
await scenario('[m2-S6] SIGINT persists the room state', async () => {
  const s = startServer(['x.mp4']);
  let proc = s.proc;
  try {
    await waitReady(s.base);
    await post(s.base, { type: 'load', from: 'm', name: 'x.mp4' });
    await post(s.base, { type: 'position', from: 'm', t: 42, p: false });
    // SIGINT while the debounced flush is still pending (<2s): only the
    // signal handler's synchronous write can save this state
    proc.kill('SIGINT');
    await waitExit(proc);
    await waitDown(s.base);
    proc = spawn(process.execPath, [join(ROOT, 'server.js')], {
      env: { ...process.env, PORT: String(s.port), VIDEOS_DIR: s.dir, STATE_FILE: join(s.dir, 'state.json') },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    await waitReady(s.base);
    const h = await firstHello(s.base, 'l');
    assert.equal(h.now, 'x.mp4', `room lost across SIGINT: hello.now=${h.now}`);
    // I-1: age only ever grows (wall-clock travel); a loose sanity cap keeps
    // the pin honest without a tight upper bound that flakes on slow hosts
    assert.ok(h.pos >= 42 && h.pos < 42 + 120, `restored pos ${h.pos}, want 42 + small age`);
  } finally {
    proc.kill();
    await waitExit(proc);
    rmSync(s.dir, { recursive: true, force: true });
  }
});

// [gap #5] inviteHosts ranking is unit-testable via an optional interfaces
// parameter: tailscale first, RFC1918 second, docker only as a single
// address (never inviteAlt), loopback last resort. Omitted param = the real
// os.networkInterfaces() value.
await scenario('[gap#5] inviteHosts ranking incl. docker exclusion', async () => {
  const unitDir = mkdtempSync(join(tmpdir(), 'wp-f1-unit-'));
  process.env.VIDEOS_DIR = unitDir;
  process.env.STATE_FILE = join(unitDir, 'state.json');
  const require = createRequire(import.meta.url);
  const sigTerm = process.listenerCount('SIGTERM');
  const sigInt = process.listenerCount('SIGINT');
  const { inviteHosts } = require(join(ROOT, 'server.js'));
  // I-2: requiring the module must not leak persistAndExit signal handlers
  // into the requiring process (an EXIT(0) on OUR SIGINT/SIGTERM mid-suite!)
  assert.equal(process.listenerCount('SIGTERM'), sigTerm, 'SIGTERM handler leaked to the requirer');
  assert.equal(process.listenerCount('SIGINT'), sigInt, 'SIGINT handler leaked to the requirer');
  const v4 = (addr, internal = false) => ({ family: 'IPv4', address: addr, internal });
  // tailscale + LAN: best tailscale, alt the LAN address
  assert.deepEqual(
    inviteHosts({ ts: [v4('100.77.1.2')], eth: [v4('192.168.1.50')] }),
    ['100.77.1.2', '192.168.1.50']
  );
  // docker + public: docker wins best (only private-ish), alt must stay empty
  assert.deepEqual(
    inviteHosts({ dock: [v4('172.17.0.1')], pub: [v4('203.0.113.9')] }),
    ['172.17.0.1', undefined]
  );
  // docker + real RFC1918: the LAN address wins, docker must NOT become alt (#31)
  assert.deepEqual(
    inviteHosts({ dock: [v4('172.17.0.1')], lan: [v4('10.1.2.3')] }),
    ['10.1.2.3', undefined]
  );
  // CGNAT + LAN: the documented tailscale+LAN pair
  assert.deepEqual(
    inviteHosts({ ts: [v4('100.100.1.1')], lan: [v4('192.168.0.5')] }),
    ['100.100.1.1', '192.168.0.5']
  );
  // loopback/internal only: last-resort constant
  assert.deepEqual(inviteHosts({ lo: [v4('127.0.0.1', true)] }), ['127.0.0.1', undefined]);
  // undefined interface entries (platform quirk) must not crash (H)
  assert.deepEqual(inviteHosts({ x: [undefined, v4('10.0.0.9')] }), ['10.0.0.9', undefined]);
  // IPv6-only non-internal interfaces are skipped
  assert.deepEqual(
    inviteHosts({ v6: [{ family: 'IPv6', address: 'fe80::1', internal: false }] }),
    ['127.0.0.1', undefined]
  );
  rmSync(unitDir, { recursive: true, force: true });
});

// [C-prep] wanlink silence mode: black-hole server->client data on /events
// connections WITHOUT destroying the sockets — the SSE client just goes
// deaf (no reconnect fires); releasing the knob restores delivery on the
// SAME connection.
await scenario('[C-prep] wanlink silenceEvents black-holes SSE without RST', async () => {
  const s = startServer(['a.mkv']);
  const proxyPort = 19200 + Math.floor(Math.random() * 90);
  const proxy = new Proxy(proxyPort, s.port, 0, 'f1-silence');
  await proxy.listen();
  try {
    await waitReady(s.base);
    const res = await fetch(`http://127.0.0.1:${proxyPort}/events?id=sil&nick=sil`);
    assert.equal(res.status, 200);
    const events = [];
    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let buf = '';
    (async () => {
      while (true) {
        const { done, value } = await reader.read();
        if (done) return;
        buf += dec.decode(value, { stream: true });
        let i;
        while ((i = buf.indexOf('\n\n')) >= 0) {
          const chunk = buf.slice(0, i);
          buf = buf.slice(i + 2);
          if (chunk.startsWith('data: ')) events.push(JSON.parse(chunk.slice(6)));
        }
      }
    })().catch(() => {});
    await waitUntil(() => events.some((e) => e.type === 'hello'), 3000, 'hello through proxy');
    proxy.silenceEvents(true);
    await post(s.base, { type: 'chat', from: 'x', nick: 'x', text: 'dropped 1' });
    await post(s.base, { type: 'chat', from: 'x', nick: 'x', text: 'dropped 2' });
    await sleep(1000);
    assert.ok(!events.some((e) => e.type === 'chat'), 'chat must be black-holed while silent');
    proxy.silenceEvents(false);
    // delivery resumes on the SAME connection: no RST, no reconnect happened
    await post(s.base, { type: 'chat', from: 'x', nick: 'x', text: 'after release' });
    await waitUntil(() => events.some((e) => e.type === 'chat' && e.text === 'after release'), 3000, 'delivery after release');
    reader.cancel().catch(() => {});
  } finally {
    proxy.close();
    await s.stop();
  }
});

for (const [st, name] of results) console.log(`${st}  ${name}`);
const fails = results.filter(([st]) => st === 'FAIL').length;
console.log(`f1: ${results.length - fails}/${results.length} held`);
process.exit(fails ? 1 : 0);
