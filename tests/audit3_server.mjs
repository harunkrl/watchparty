// Adversarial audit round 3 evidence (worker1): server.js S1-S6 surface,
// post-C7 wire realities, SSE paths, idle-notice flood. Scenarios marked
// [BUG] encode DESIRED behavior and are expected to FAIL on the current
// tree (failing-first evidence for the findings report); [PIN] scenarios
// are regression pins expected to PASS. Isolated servers, random test
// ports, temp dirs — never the production port.
// Usage: node tests/audit3_server.mjs

import { strict as assert } from 'node:assert';
import { spawn } from 'node:child_process';
import http from 'node:http';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let portCounter = Math.floor(19700 + Math.random() * 200);

function startServer(files, opts = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'wp-audit3-'));
  for (const f of files) writeFileSync(join(dir, f), Buffer.alloc(4096, 1));
  if (opts.state) writeFileSync(join(dir, 'state.json'), JSON.stringify(opts.state));
  const port = portCounter++;
  const proc = spawn(process.execPath, [join(ROOT, 'server.js')], {
    env: { ...process.env, PORT: String(port), VIDEOS_DIR: dir, STATE_FILE: join(dir, 'state.json') },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let err = '';
  proc.stderr.on('data', (d) => (err += d));
  const base = `http://127.0.0.1:${port}`;
  return {
    base,
    stop: async () => {
      proc.kill();
      // wait for exit: the SIGTERM handler may still write state.json —
      // removing the dir before that races it (ENOTEMPTY)
      await new Promise((r) => proc.on('exit', r));
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

async function post(base, obj) {
  const res = await fetch(`${base}/cmd`, { method: 'POST', body: JSON.stringify(obj) });
  return res.status;
}

// SSE collector over raw http so the socket can be hard-destroyed (RST)
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
    hardKill: () => res && res.socket.destroy(), // RST, no FIN
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

const results = [];
async function scenario(name, fn) {
  try {
    await fn();
    results.push(['PASS', name]);
  } catch (e) {
    results.push(['FAIL', `${name} — ${e.message}`]);
  }
}

// [PIN] S4 pairing matrix: dotted stems, double extensions, the m/m2
// prefix trap, case+space stems, unpaired subs.
await scenario('pin: /library pairing matrix', async () => {
  const s = startServer([
    'movie.mp4.mkv', 'movie.mkv', 'm.mkv', 'm2.mkv', 'Movie (2023).mkv',
    'movie.mp4.srt', 'movie.srt', 'movie.tr.srt', 'm2.tr.srt', 'Movie (2023).tr.srt', 'zz.srt',
  ]);
  try {
    await waitReady(s.base);
    const lib = await (await fetch(`${s.base}/library`)).json();
    const by = Object.fromEntries(lib.map((e) => [e.name, e.subs]));
    assert.deepEqual(by['movie.mp4.mkv'], ['movie.mp4.srt'], 'double-ext stem');
    // documented client parity quirk: the double-ext sub also matches the
    // shorter stem ("movie." is a prefix of "movie.mp4.srt")
    assert.deepEqual(by['movie.mkv'], ['movie.mp4.srt', 'movie.srt', 'movie.tr.srt'], 'short stem');
    assert.deepEqual(by['m.mkv'], [], 'dot separator must block the m/m2 prefix trap');
    assert.deepEqual(by['m2.mkv'], ['m2.tr.srt']);
    assert.deepEqual(by['Movie (2023).mkv'], ['Movie (2023).tr.srt'], 'case + spaces literal');
    assert.ok(!lib.some((e) => e.name === 'zz.srt'), 'subs are not videos');
  } finally {
    await s.stop();
  }
});

// [PIN] S5: /health exposes only now/queueLen/listeners — no roster, no
// nicks, no filesystem paths.
await scenario('pin: /health leaks no room/identity detail', async () => {
  const s = startServer(['x.mp4']);
  try {
    await waitReady(s.base);
    await post(s.base, { type: 'load', from: 'm', name: 'x.mp4' });
    const h = await (await fetch(`${s.base}/health`)).json();
    assert.deepEqual(Object.keys(h).sort(), ['listeners', 'now', 'queueLen']);
    assert.equal(h.now, 'x.mp4', 'bare filename, not a path');
  } finally {
    await s.stop();
  }
});

// [PIN] S3 restore path: dur restores only as a positive number; hello
// clamps across the restart; a garbage dur must not clamp.
await scenario('pin: dur persist/restore validation', async () => {
  const a = startServer(['x.mp4'], { state: { now: 'x.mp4', queue: [], pos: 500, paused: true, dur: 50, wall: Date.now() } });
  try {
    await waitReady(a.base);
    const h = await firstHello(a.base, 'l1');
    assert.equal(h.pos, 49, `clamped to dur-1 across restart, got ${h.pos}`);
  } finally {
    await a.stop();
  }
  const b = startServer(['x.mp4'], { state: { now: 'x.mp4', queue: [], pos: 500, paused: true, dur: '50', wall: Date.now() } });
  try {
    await waitReady(b.base);
    const h = await firstHello(b.base, 'l2');
    assert.equal(h.pos, 500, `string dur must be ignored, got ${h.pos}`);
  } finally {
    await b.stop();
  }
});

// [PIN] S3 clamp math: dur=0.5 (sub-second file) floors the join at 0 —
// degenerate but still "before EOF"; documented, not clamped to negative.
await scenario('pin: dur=0.5 clamps to 0, never negative', async () => {
  const s = startServer(['x.mp4']);
  try {
    await waitReady(s.base);
    await post(s.base, { type: 'load', from: 'm', name: 'x.mp4' });
    await post(s.base, { type: 'position', from: 'm', t: 10, p: true, dur: 0.5 });
    await sleep(150);
    const h = await firstHello(s.base, 'l');
    assert.equal(h.pos, 0, `sub-second dur floors at 0, got ${h.pos}`);
  } finally {
    await s.stop();
  }
});

// [PIN] N1: an abrupt socket destroy (RST) racing a broadcast burst must
// never take the server down (res.write to a destroyed stream).
await scenario('pin: destroy-during-broadcast does not crash the server', async () => {
  const s = startServer(['x.mp4']);
  try {
    await waitReady(s.base);
    const a = rawSse(s.base, 'victim');
    const b = rawSse(s.base, 'survivor');
    await sleep(250);
    a.hardKill();
    for (let i = 0; i < 50; i++) {
      await post(s.base, { type: 'chat', from: 'b', nick: 'b', text: `burst ${i}` });
    }
    await sleep(500);
    const r = await fetch(`${s.base}/health`, { signal: AbortSignal.timeout(1000) });
    assert.equal(r.status, 200, `server died after RST-during-broadcast; stderr: ${s.stderr()}`);
    assert.ok(b.events.some((e) => e.type === 'chat'), 'survivor stopped receiving');
    b.close();
  } finally {
    await s.stop();
  }
});

// [BUG A1] `ended` must advance the queue WITHOUT claiming the clock —
// only position heartbeats claim. Current code routes ended through
// isClockOwner, so a follower's EOF in an ownerless room grabs ownership;
// the real master's first heartbeat is then answered with a "two masters"
// impostor notice and dropped. The client demotes itself on that notice →
// room clock dead until the follower restarts.
await scenario('[A1] ended advances the queue but never claims the clock', async () => {
  const s = startServer(['x.mp4', 'y.mp4']);
  try {
    await waitReady(s.base);
    const obs = rawSse(s.base, 'obs');
    await sleep(200);
    // ownerless room (master gone): the follower is playing x, y queued
    await post(s.base, { type: 'load', from: 'follower', name: 'x.mp4' });
    await post(s.base, { type: 'enqueue', from: 'follower', name: 'y.mp4' });
    await sleep(150);
    // the follower's mpv hits EOF and posts ended
    await post(s.base, { type: 'ended', from: 'follower' });
    await sleep(200);
    // the queue advanced for everyone...
    assert.ok(obs.events.some((e) => e.type === 'load' && e.name === 'y.mp4'), 'ended did not advance the queue');
    // ...but the clock stayed unclaimed: the real master's first heartbeat
    // must own the room without any impostor notice
    await post(s.base, { type: 'position', from: 'master', t: 5, p: false });
    await sleep(200);
    const notices = obs.events.filter((e) => e.type === 'notice' && e.text.includes('two masters'));
    assert.equal(notices.length, 0, `ended claimed the clock; master demoted by: ${notices.map((n) => n.text)}`);
    assert.ok(obs.events.some((e) => e.type === 'position' && e.from === 'master'), 'master heartbeat must relay');
    obs.close();
  } finally {
    await s.stop();
  }
});

// [BUG A2] S2 idle notice has no rate limit: every idle skip/ended is a
// broadcast amplifier (N tiny POSTs → N×listeners SSE lines + TUI spam).
// Desired: per-room cooldown so a stuck key / hostile loop degrades to a
// trickle instead of a flood.
await scenario('[A2] idle notice must be rate-limited', async () => {
  const s = startServer(['x.mp4']);
  try {
    await waitReady(s.base);
    const obs = rawSse(s.base, 'obs');
    await sleep(200);
    for (let i = 0; i < 50; i++) await post(s.base, { type: 'skip', from: 'spam' });
    await sleep(400);
    const notices = obs.events.filter((e) => e.type === 'notice' && e.text.includes('nothing playing'));
    assert.ok(notices.length <= 5, `amplifier: ${notices.length} notices for 50 idle skips`);
    obs.close();
  } finally {
    await s.stop();
  }
});

// [BUG A3] The position/seek gates accept negative t (finite check only).
// With dur unknown (undecoded file) nothing floors hello.pos, so an
// adversarial/stale heartbeat poisons every late joiner with a negative
// position; negative seeks relay to all players too.
await scenario('[A3] negative t must be rejected at the gates', async () => {
  const s = startServer(['x.mp4']);
  try {
    await waitReady(s.base);
    const obs = rawSse(s.base, 'obs');
    await sleep(200);
    // load first: hello only carries pos for a live file (this is also the
    // undecoded-file case — no dur known, so the clamp cannot mask it)
    await post(s.base, { type: 'load', from: 'm', name: 'x.mp4' });
    await post(s.base, { type: 'position', from: 'm', t: -5, p: true }); // no dur: clamp inactive
    await post(s.base, { type: 'seek', from: 'm', t: -3 });
    await sleep(300);
    const h = await firstHello(s.base, 'late');
    assert.ok(h.pos === undefined || h.pos >= 0, `negative pos leaked to hello: ${h.pos}`);
    assert.ok(!obs.events.some((e) => e.type === 'seek' && e.t < 0), 'negative seek relayed to players');
    obs.close();
  } finally {
    await s.stop();
  }
});

for (const [st, name] of results) console.log(`${st}  ${name}`);
const fails = results.filter(([st]) => st === 'FAIL').length;
console.log(`audit3: ${results.length - fails}/${results.length} held (${fails} failing-first evidence)`);
process.exit(fails ? 1 : 0);
