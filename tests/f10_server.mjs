// F10 worker2: the server pins for the v1.9.2 fixes (A10 Group A + B).
//
// S1 (M2-03) the seek gates: a stale client and a wrong-file poster must
//     not steer the room; a legit seek still relays.
// S2 (M2-04/B2-2) the p type gate: a typed p is neither committed nor
//     broadcast; p absent keeps the legacy behavior.
// S4 (B1-2) the follower seek persist: a late joiner's hello carries the
//     seek target, not the pre-seek position.
// S5 (B1-4) the pause reset on a file change: a newly loaded file plays.
// S6 (B1-3) PORT=0 picks a real (ephemeral) port, not 8090.
//
// Every pin here FAILS on the pre-F10 server (v1.9.1) and passes after
// the fix — the red-first evidence is in .pi-state/f10-worker2-raw.md.
// Server-only: no mpv, no client binary.
// Usage: node tests/f10_server.mjs

import { strict as assert } from 'node:assert';
import { spawn } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import http from 'node:http';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
// the worker2 port band (f6: 18500+, persist: 18900+, f2: 19300+)
let portCounter = 19100 + Math.floor(Math.random() * 100);

const waitExit = (proc) =>
  proc.exitCode === null ? new Promise((r) => proc.on('exit', r)) : Promise.resolve();

function startServer(files, extraEnv = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'wp-f10-'));
  for (const f of files) writeFileSync(join(dir, f), Buffer.alloc(4096, 1));
  const port = portCounter++;
  const proc = spawn(process.execPath, [join(ROOT, 'server.js')], {
    env: { ...process.env, PORT: String(port), VIDEOS_DIR: dir, STATE_FILE: join(dir, 'state.json'), ...extraEnv },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let out = '';
  proc.stdout.on('data', (d) => (out += d));
  proc.stderr.on('data', (d) => (out += d));
  const base = `http://127.0.0.1:${port}`;
  return {
    dir, base, port, proc,
    out: () => out,
    stop: async () => {
      if (proc.exitCode === null && proc.signalCode === null) {
        proc.kill();
        await waitExit(proc);
      }
      rmSync(dir, { recursive: true, force: true });
    },
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
  throw new Error('the server did not come up');
}

async function post(base, obj) {
  const res = await fetch(`${base}/cmd`, { method: 'POST', body: JSON.stringify(obj) });
  assert.equal(res.status, 204, `the cmd post failed: ${res.status}`);
}

async function health(base) {
  return (await fetch(`${base}/health`)).json();
}

// rawSse: an observer that collects every room event it is sent
function rawSse(base, id, nick) {
  const events = [];
  let res = null;
  let buf = '';
  const q = nick ? `&nick=${encodeURIComponent(nick)}` : '';
  http.get(`${base}/events?id=${encodeURIComponent(id)}${q}`, (r) => {
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
  res && res.on('error', () => {});
  return { events, close: () => res && res.destroy() };
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

// firstHello: the hello a fresh client sees (the room state it inherits)
async function firstHello(base, id) {
  const w = rawSse(base, id, id);
  await waitUntil(() => w.events.length > 0, 3000, 'the hello');
  w.close();
  const hello = w.events.find((e) => e.type === 'hello');
  assert.ok(hello, 'no hello event arrived');
  return hello;
}

const results = [];
async function scenario(name, fn) {
  try {
    await fn();
    results.push(['PASS', name]);
    console.log(`PASS ${name}`);
  } catch (e) {
    results.push(['FAIL', `${name} — ${e.message}`]);
    console.error(`FAIL ${name}: ${e.message}`);
  }
}

// --- S1: the seek gates (M2-03) ------------------------------------------

// A stale client (marked by the wrong-file gate) kept seeking the room:
// seek was the only steering type with no isStale gate.
await scenario('[S1] a stale client cannot seek the room', async () => {
  const s = startServer(['a.mkv', 'b.mkv'], { STALE_TTL_MS: '400' });
  try {
    await waitReady(s.base);
    const w = rawSse(s.base, 'obs', 'Obs');
    await waitUntil(() => w.events.length > 0, 3000, 'the observer hello');
    await post(s.base, { type: 'load', from: 'm', name: 'a.mkv' });
    await post(s.base, { type: 'position', from: 'm', t: 1, p: false, file: 'a.mkv' }); // m owns the clock
    // an old master still driving the previous film: the wrong-file gate
    // marks it stale
    await post(s.base, { type: 'position', from: 'old', t: 2, p: false, file: 'b.mkv' });
    // its seek must not reach the room
    await post(s.base, { type: 'seek', from: 'old', t: 99 });
    await sleep(400);
    assert.ok(!w.events.some((e) => e.type === 'seek'), `a stale client seeked the room: ${JSON.stringify(w.events)}`);
    const hello = await firstHello(s.base, 'late1');
    assert.ok(!(hello.pos >= 90), `the stale seek moved the room's position: ${hello.pos}`);
    w.close();
  } finally { await s.stop(); }
});

// A seek that names another file came from a client still on the old film:
// it must be dropped AND the poster must lose its steering rights.
await scenario('[S1] a seek that names another file is dropped and marks the poster stale', async () => {
  const s = startServer(['a.mkv', 'b.mkv']);
  try {
    await waitReady(s.base);
    const w = rawSse(s.base, 'obs', 'Obs');
    await waitUntil(() => w.events.length > 0, 3000, 'the observer hello');
    await post(s.base, { type: 'load', from: 'm', name: 'a.mkv' });
    await post(s.base, { type: 'seek', from: 'u', t: 42, file: 'b.mkv' }); // the wrong file
    await sleep(300);
    assert.ok(!w.events.some((e) => e.type === 'seek'), `the wrong-file seek was relayed: ${JSON.stringify(w.events)}`);
    // the poster is stale now: its pause must not steer the room either
    await post(s.base, { type: 'pause', from: 'u' });
    assert.equal((await health(s.base)).paused, false, 'the wrong-file poster still steered the room');
    // the room says why (the position gate's shape: a rate-limited notice)
    assert.ok(
      w.events.some((e) => e.type === 'notice' && /moved on/.test(e.text)),
      `the drop was silent: ${JSON.stringify(w.events)}`
    );
    w.close();
  } finally { await s.stop(); }
});

// The gates must not touch a legit seek: the current file (named) and the
// file-less shape (the official client's) both relay.
await scenario('[S1] a seek on the current file still relays', async () => {
  const s = startServer(['a.mkv', 'b.mkv']);
  try {
    await waitReady(s.base);
    const w = rawSse(s.base, 'obs', 'Obs');
    await waitUntil(() => w.events.length > 0, 3000, 'the observer hello');
    await post(s.base, { type: 'load', from: 'm', name: 'a.mkv' });
    await post(s.base, { type: 'seek', from: 'u', t: 42, file: 'a.mkv' }); // names the CURRENT file
    await post(s.base, { type: 'seek', from: 'v', t: 7 }); // no file at all (the official client's shape)
    await waitUntil(() => w.events.filter((e) => e.type === 'seek').length >= 2, 3000, 'the two seeks');
    const seeks = w.events.filter((e) => e.type === 'seek');
    assert.ok(seeks.some((e) => e.t === 42), 'the named seek was dropped');
    assert.ok(seeks.some((e) => e.t === 7), 'the file-less seek was dropped');
    w.close();
  } finally { await s.stop(); }
});

// --- S2: the p type gate (M2-04/B2-2) ------------------------------------

// p:"no" committed paused:true and broadcast the raw body; the Go client
// then dropped the WHOLE event on the type error — the room and the
// clients diverged.
await scenario('[S2] a typed p is neither committed nor broadcast', async () => {
  const s = startServer(['a.mkv']);
  try {
    await waitReady(s.base);
    const w = rawSse(s.base, 'obs', 'Obs');
    await waitUntil(() => w.events.length > 0, 3000, 'the observer hello');
    await post(s.base, { type: 'load', from: 'm', name: 'a.mkv' });
    await post(s.base, { type: 'position', from: 'm', t: 1, p: true }); // the room is paused
    await post(s.base, { type: 'position', from: 'm', t: 5, p: 'no' }); // typed garbage
    await sleep(300);
    assert.ok(!w.events.some((e) => e.type === 'position' && e.t === 5), 'the garbage p was broadcast');
    assert.equal((await health(s.base)).paused, true, "the garbage p moved the room's pause");
    const hello = await firstHello(s.base, 'late');
    assert.ok(!(hello.pos >= 5), `the garbage p was committed: ${hello.pos}`);
    w.close();
  } finally { await s.stop(); }
});

// The gate is only for a WRONG type: p absent keeps the legacy behavior and
// a boolean p works as before.
await scenario('[S2] p absent keeps the legacy behavior and a boolean p works', async () => {
  const s = startServer(['a.mkv']);
  try {
    await waitReady(s.base);
    const w = rawSse(s.base, 'obs', 'Obs');
    await waitUntil(() => w.events.length > 0, 3000, 'the observer hello');
    await post(s.base, { type: 'load', from: 'm', name: 'a.mkv' });
    await post(s.base, { type: 'position', from: 'm', t: 6 }); // p absent: the legacy path
    await waitUntil(() => w.events.some((e) => e.type === 'position' && e.t === 6), 3000, 'the position without p');
    await post(s.base, { type: 'position', from: 'm', t: 7, p: true }); // a boolean p
    await waitUntil(() => w.events.some((e) => e.type === 'position' && e.t === 7), 3000, 'the boolean position');
    assert.equal((await health(s.base)).paused, true, `the boolean p did not pause the room: ${(await health(s.base)).paused}`);
    w.close();
  } finally { await s.stop(); }
});

// --- S2b (master-2): the CLASS — every known field the client decodes ---

// The Go client decodes EVERY event into room.Event: a known field with
// the wrong JSON type makes it drop the WHOLE event (room.go), so the
// room advances and the clients miss it. The p gate closed ONE instance;
// the class is every known field the client decodes (dur/file/from/name).
await scenario('[S2] a typed known field is neither committed nor broadcast', async () => {
  const s = startServer(['a.mkv', 'b.mkv']);
  try {
    await waitReady(s.base);
    const w = rawSse(s.base, 'obs', 'Obs');
    await waitUntil(() => w.events.length > 0, 3000, 'the observer hello');
    await post(s.base, { type: 'load', from: 'm', name: 'a.mkv' });
    await post(s.base, { type: 'position', from: 'm', t: 1, p: false }); // the room's position
    // the poison vectors: a known field with the wrong JSON type
    await post(s.base, { type: 'position', from: 'm', t: 5, dur: '60' }); // dur as a string
    await post(s.base, { type: 'position', from: 'm', t: 6, file: 7 }); // file as a number
    await post(s.base, { type: 'chat', from: {}, text: 'hi' }); // from as an object
    await post(s.base, { type: 'chat', from: 'm', text: 'hi', name: {} }); // name as an object
    await sleep(300);
    assert.ok(
      !w.events.some((e) => e.type === 'position' && e.t >= 5),
      `a typed known field was broadcast: ${JSON.stringify(w.events)}`
    );
    assert.ok(
      !w.events.some((e) => e.type === 'chat'),
      `a typed known field was broadcast: ${JSON.stringify(w.events)}`
    );
    // and the room did not move
    const hello = await firstHello(s.base, 'late');
    assert.ok(!(hello.pos >= 5), `a typed known field was committed: ${hello.pos}`);
    // the legit types still work: the dur lands (the hello clamps to dur-1)
    await post(s.base, { type: 'position', from: 'm', t: 100, dur: 60 });
    await waitUntil(() => w.events.some((e) => e.type === 'position' && e.t === 100), 3000, 'the legit position');
    const hello2 = await firstHello(s.base, 'late2');
    assert.equal(hello2.pos, 59, `the legit dur was not committed: ${JSON.stringify(hello2)}`);
    // an UNKNOWN field still relays as-is (the wire law)
    await post(s.base, { type: 'position', from: 'm', t: 101, p: false, foo: 'bar' });
    await waitUntil(() => w.events.some((e) => e.type === 'position' && e.t === 101), 3000, 'the extra-field position');
    assert.equal(w.events.find((e) => e.type === 'position' && e.t === 101).foo, 'bar', 'the unknown field was stripped');
    w.close();
  } finally { await s.stop(); }
});

// --- S2c: the known-field matrix (master-2) ------------------------------

// Every field the Go client decodes: a wrong JSON type must DROP the
// event (the client would drop it anyway — the room would advance and the
// client would miss it) and the RIGHT type must RELAY (the over-blocking
// regression). The two fields the server normalizes itself (the nick, the
// chat text) are the exception: the server rewrites them to a string, so
// the relay stays decodable and dropping it would be over-blocking.
const KNOWN_FIELDS = [
  { field: 'type', right: 'chat', wrong: 7, drops: true },
  { field: 'name', right: 'a.mkv', wrong: 7, drops: true },
  { field: 'now', right: 'a.mkv', wrong: 7, drops: true },
  { field: 'invite', right: 'http://x', wrong: 7, drops: true },
  { field: 'inviteAlt', right: 'http://x', wrong: 7, drops: true },
  { field: 'file', right: 'a.mkv', wrong: 7, drops: true },
  { field: 'from', right: 'probe2', wrong: {}, drops: true },
  { field: 't', right: 12, wrong: 'x', drops: true },
  { field: 'dur', right: 60, wrong: 'x', drops: true },
  { field: 'pos', right: 12, wrong: 'x', drops: true },
  { field: 'p', right: false, wrong: 'x', drops: true },
  { field: 'mechanical', right: false, wrong: 'x', drops: true },
  { field: 'queue', right: [], wrong: 'x', drops: true },
  { field: 'roster', right: [], wrong: 'x', drops: true },
  { field: 'nick', right: 'Probe', wrong: {}, drops: false }, // the server normalizes it
  { field: 'text', right: 'hi', wrong: {}, drops: false }, // the server cleans it (chat)
];

await scenario('[S2] the known-field matrix: a wrong type drops, the right type relays', async () => {
  const s = startServer(['a.mkv', 'b.mkv']);
  try {
    await waitReady(s.base);
    const w = rawSse(s.base, 'obs', 'Obs');
    await waitUntil(() => w.events.length > 0, 3000, 'the observer hello');
    const chats = () => w.events.filter((e) => e.type === 'chat').length;
    for (const f of KNOWN_FIELDS) {
      const before = chats();
      await post(s.base, { type: 'chat', from: 'probe', text: 'hi', [f.field]: f.wrong });
      await sleep(120);
      if (f.drops) {
        assert.equal(chats(), before, `a typed "${f.field}" was relayed: ${JSON.stringify(w.events.slice(-2))}`);
      } else {
        assert.equal(chats(), before + 1, `a normalized "${f.field}" was dropped (over-blocking): ${JSON.stringify(w.events.slice(-2))}`);
      }
      // the RIGHT type must relay (the over-blocking regression)
      const beforeOk = chats();
      await post(s.base, { type: 'chat', from: 'probe', text: 'hi', [f.field]: f.right });
      await waitUntil(() => chats() > beforeOk, 3000, `the legit "${f.field}" was dropped`);
    }
    // the drop is visible in the server log (the rate limit keeps the rest quiet)
    assert.ok(s.out().includes('dropped a'), `the drop was not logged:\n${s.out()}`);
    // a typed field on a cmd the server does NOT normalize is dropped
    const beforePos = w.events.length;
    await post(s.base, { type: 'position', from: 'm', t: 3, p: false, text: {} });
    await sleep(150);
    assert.equal(w.events.length, beforePos, 'a typed text on a position was relayed');
    w.close();
  } finally { await s.stop(); }
});

// A hostile client must not fill the server's stderr: the drop log is
// rate-limited (the notice rate-limit's shape).
await scenario('[S2] a flood of typed cmds does not spam the drop log', async () => {
  const s = startServer(['a.mkv']);
  try {
    await waitReady(s.base);
    for (let i = 0; i < 30; i++) await post(s.base, { type: 'position', from: 'm', t: i, dur: 'x' });
    await sleep(300);
    const lines = s.out().split('\n').filter((l) => l.includes('dropped a'));
    assert.ok(lines.length >= 1, `the drop was not logged at all:\n${s.out()}`);
    assert.ok(lines.length <= 2, `the drop log was not rate-limited: ${lines.length} lines in a burst`);
  } finally { await s.stop(); }
});

// --- S4: the follower seek persist (B1-2) --------------------------------

// The seek was relayed but never persisted: a late joiner's hello carried
// the pre-seek position.
await scenario('[S4] a follower seek lands in the room state', async () => {
  const s = startServer(['a.mkv']);
  try {
    await waitReady(s.base);
    await post(s.base, { type: 'load', from: 'm', name: 'a.mkv' });
    await post(s.base, { type: 'seek', from: 'f', t: 42 }); // a follower scrubs
    const hello = await firstHello(s.base, 'late');
    assert.equal(hello.pos, 42, `the late joiner got ${hello.pos}, want the seek target 42`);
  } finally { await s.stop(); }
});

// --- S5: the pause reset on a file change (B1-4) --------------------------

// The paused flag rode into the next file: ~2s of a wrong hello.p until
// the master's first heartbeat. A newly loaded file starts playing.
await scenario('[S5] a file change resets the room pause', async () => {
  const s = startServer(['a.mkv', 'b.mkv']);
  try {
    await waitReady(s.base);
    await post(s.base, { type: 'load', from: 'm', name: 'a.mkv' });
    await post(s.base, { type: 'pause', from: 'm' });
    assert.equal((await health(s.base)).paused, true, 'the room did not pause');
    await post(s.base, { type: 'enqueue', from: 'm', name: 'b.mkv' });
    await post(s.base, { type: 'ended', from: 'm' }); // the room moves on
    const hello = await firstHello(s.base, 'joiner');
    assert.equal(hello.now, 'b.mkv', `the room did not move on: ${hello.now}`);
    assert.equal(hello.p, false, `the room is still paused after the file change: ${JSON.stringify(hello)}`);
    // the load path resets it too
    await post(s.base, { type: 'pause', from: 'm' });
    await post(s.base, { type: 'load', from: 'm', name: 'a.mkv' });
    const hello2 = await firstHello(s.base, 'joiner2');
    assert.equal(hello2.p, false, `the room is still paused after the load: ${JSON.stringify(hello2)}`);
  } finally { await s.stop(); }
});

// --- S6: PORT=0 is a real port (B1-3) ------------------------------------

// Number(env)||8090 turned PORT=0 into 8090: "pick a port for me" became
// "use the production port".
await scenario('[S6] PORT=0 picks an ephemeral port, not 8090', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'wp-f10-'));
  writeFileSync(join(dir, 'a.mkv'), Buffer.alloc(4096, 1));
  const proc = spawn(process.execPath, [join(ROOT, 'server.js')], {
    env: { ...process.env, PORT: '0', VIDEOS_DIR: dir, STATE_FILE: join(dir, 'state.json') },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let out = '';
  proc.stdout.on('data', (d) => (out += d));
  proc.stderr.on('data', (d) => (out += d));
  try {
    const m = await waitUntil(() => /on http:\/\/0\.0\.0\.0:(\d+)/.exec(out), 8000, 'the boot log');
    const port = Number(m[1]);
    assert.notEqual(port, 8090, `PORT=0 still picked 8090 (boot log: ${out.trim()})`);
    const h = await (await fetch(`http://127.0.0.1:${port}/health`)).json();
    assert.equal(h.now, null, 'the server did not answer on the picked port');
  } finally {
    if (proc.exitCode === null && proc.signalCode === null) {
      proc.kill();
      await waitExit(proc);
    }
    rmSync(dir, { recursive: true, force: true });
  }
});

const failed = results.filter(([r]) => r === 'FAIL');
if (failed.length) {
  console.error(`\n${failed.length}/${results.length} pin(s) failed:`);
  for (const [, name] of failed) console.error(`  - ${name}`);
  process.exit(1);
}
console.log(`\nPASS: all ${results.length} F10 server pins held`);
