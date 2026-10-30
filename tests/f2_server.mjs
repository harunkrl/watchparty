// F2 phase-2 pins (worker1): w1-3 clock-claim freshness + mechanical relay
// guard + a real-client silent-death E2E (SIGSTOP = silent master whose SSE
// socket never closes — the half-open case SSE close cannot cover).
// Each server scenario spawns its own server.js on a random test port —
// never the production port. Usage: node tests/f2_server.mjs
import { strict as assert } from 'node:assert';
import { execSync, spawn } from 'node:child_process';
import { existsSync, mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import http from 'node:http';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let portCounter = Math.floor(19300 + Math.random() * 90);

const GO_BIN = join(ROOT, 'client-go/watchparty');
function has(bin) {
  try { execSync(`command -v ${bin}`, { stdio: 'ignore' }); return true; } catch { return false; }
}
const canE2E = has('mpv') && has('ffmpeg') && existsSync(GO_BIN);

// 'exit' fires once; a kill() on an already-exited child would hang the await
const waitExit = (proc) =>
  proc.exitCode === null ? new Promise((r) => proc.on('exit', r)) : Promise.resolve();

function startServer(files, extraEnv = {}) {
  const dir = mkdtempSync(join(tmpdir(), 'wp-f2-'));
  for (const f of files) writeFileSync(join(dir, f), Buffer.alloc(4096, 1));
  const port = portCounter++;
  const proc = spawn(process.execPath, [join(ROOT, 'server.js')], {
    env: { ...process.env, PORT: String(port), VIDEOS_DIR: dir, STATE_FILE: join(dir, 'state.json'), ...extraEnv },
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

async function post(base, obj) {
  const res = await fetch(`${base}/cmd`, { method: 'POST', body: JSON.stringify(obj) });
  return res.status;
}

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

const positions = (events) => events.filter((e) => e.type === 'position');
const twoMasterNotices = (events) => events.filter((e) => e.type === 'notice' && e.text.includes('two masters'));

const results = [];
async function scenario(name, fn) {
  try {
    await fn();
    results.push(['PASS', name]);
  } catch (e) {
    if (e && e.skip) results.push(['SKIP', name]);
    else results.push(['FAIL', `${name} — ${e.message}`]);
  }
}

// [w1-3] the claim must also die when the owner stops POSTING positions
// (half-open death: SSE close never fires). Before the TTL the fresh claim
// still repels a second master; after it, takeover is silent; the old owner
// returning is answered with exactly one impostor notice.
await scenario('[w1-3] stale claim released silently, fresh claim preserved', async () => {
  const s = startServer(['a.mkv'], { CLAIM_FRESH_MS: '2000' });
  try {
    await waitReady(s.base);
    const w = rawSse(s.base, 'obs', 'obs');
    await sleep(150);
    await post(s.base, { type: 'position', from: 'm1', t: 10, p: false });
    await waitUntil(() => positions(w.events).some((e) => e.t === 10), 3000, 'owner heartbeat relayed');
    // fresh claim: a second master is still repelled (one-time notice, drop)
    await sleep(400);
    await post(s.base, { type: 'position', from: 'm2', t: 99, p: false });
    await waitUntil(() => twoMasterNotices(w.events).length === 1, 3000, 'fresh-claim impostor notice');
    await sleep(250);
    assert.ok(!positions(w.events).some((e) => e.t === 99), 'impostor heartbeat relayed while claim fresh');
    // past the TTL the claim is stale: takeover is silent and complete
    await sleep(2200); // owner's last post is now >2s old
    await post(s.base, { type: 'position', from: 'm2', t: 50, p: false });
    await waitUntil(() => positions(w.events).some((e) => e.t === 50), 3000, 'takeover heartbeat relayed');
    assert.equal(twoMasterNotices(w.events).length, 1, 'stale takeover must be silent (no new notice)');
    // the OLD owner returning is now the impostor: one notice, dropped
    await post(s.base, { type: 'position', from: 'm1', t: 123, p: false });
    await waitUntil(() => twoMasterNotices(w.events).length === 2, 3000, 'old-owner impostor notice');
    await sleep(250);
    assert.ok(!positions(w.events).some((e) => e.t === 123), 'old owner heartbeat relayed after takeover');
    // the new owner keeps the clock
    await post(s.base, { type: 'position', from: 'm2', t: 51, p: false });
    await waitUntil(() => positions(w.events).some((e) => e.t === 51), 3000, 'successor keeps posting');
    w.close();
  } finally {
    await s.stop();
  }
});

// [mechanical guard] the /cmd relay passes the cmd object through verbatim:
// the mechanical pause flag (client f29c3a2, room.Event.Mechanical) must
// survive every relay-side touch (nick stamping, claim checks).
await scenario('[mechanical] relay keeps the mechanical flag on pause', async () => {
  const s = startServer(['a.mkv']);
  try {
    await waitReady(s.base);
    const w = rawSse(s.base, 'obs', 'obs');
    await sleep(150);
    await post(s.base, { type: 'pause', from: 'm', mechanical: true });
    await waitUntil(() => w.events.some((e) => e.type === 'pause'), 3000, 'pause relayed');
    const pause = w.events.find((e) => e.type === 'pause');
    assert.equal(pause.mechanical, true, `mechanical flag lost on relay: ${JSON.stringify(pause)}`);
    w.close();
  } finally {
    await s.stop();
  }
});

// [w1-3 E2E] silent-death end to end with real clients: SIGSTOP freezes the
// master client (posts stop, SSE socket stays OPEN — no close, no RST), the
// server's claim goes stale, a restarted master takes over silently, and the
// woken master is demoted by its own impostor notice. Skipped without
// mpv/ffmpeg/the built client binary.
await scenario('[w1-3 E2E] frozen master: takeover, silent claim, self-demotion on wake', async () => {
  if (!canE2E) throw Object.assign(new Error('needs mpv, ffmpeg, client binary'), { skip: true });
  const MPV_ARGS = '--vo=null --ao=null --no-config --force-window=no';
  const s = startServer(['test.mp4'], { CLAIM_FRESH_MS: '2500' });
  const dir = s.dir;
  let m1, m2;
  const reap = (c) => {
    try { execSync(`pkill -TERM -P ${c.pid}`); } catch {}
    c.kill();
  };
  try {
    execSync(
      'ffmpeg -y -loglevel error -f lavfi -i testsrc2=duration=90:size=320x240:rate=10 ' +
        '-f lavfi -i sine=frequency=440:duration=90 -c:v libx264 -preset ultrafast -c:a aac -shortest ' +
        join(dir, 'test.mp4')
    );
    await waitReady(s.base);
    const w = rawSse(s.base, 'obs', 'obs');
    await sleep(150);
    const client = (name) => {
      const c = spawn(GO_BIN, ['--headless', '--master', '--name', name, s.base], {
        env: { ...process.env, WATCHPARTY_MPV: MPV_ARGS },
        stdio: ['pipe', 'pipe', 'pipe'],
      });
      c.out = '';
      c.stdout.on('data', (d) => (c.out += d));
      c.stderr.on('data', (d) => (c.out += d));
      return c;
    };
    m1 = client('M1');
    await waitUntil(() => m1.out.includes('commands:'), 15000, 'M1 up');
    m1.stdin.write('play test.mp4\n');
    // the master is live: heartbeats flow
    await waitUntil(() => positions(w.events).length >= 2, 15000, 'M1 heartbeats');
    const before = positions(w.events).length;
    // silent death: posts stop, the SSE socket stays open (SIGSTOP = no close)
    m1.kill('SIGSTOP');
    // ISSUE-F (master-2): sleep must outlast the whole 2.5s TTL — M2's FIRST
    // position post has to land on an already-stale claim regardless of the
    // client's boot time and heartbeat poll phase, or the takeover fires an
    // impostor notice and the silent-takeover assert goes flaky
    await sleep(3000);
    // a restarted master appears while the old one is silent
    m2 = client('M2');
    // takeover: M2's heartbeats must relay after the claim went stale, with
    // NO impostor notice for M2 (silence, not a duel)
    await waitUntil(() => positions(w.events).length >= before + 3, 20000, 'M2 heartbeats after takeover');
    assert.equal(twoMasterNotices(w.events).length, 0, `takeover was not silent: ${JSON.stringify(twoMasterNotices(w.events).map((n) => n.text))}`);
    // the woken master posts once, gets the impostor notice, demotes itself
    m1.kill('SIGCONT');
    await waitUntil(() => m1.out.includes('two masters'), 10000, 'woken master demoted by notice');
    // the room keeps one clock: M2's heartbeats continue after M1's return
    const after = positions(w.events).length;
    await sleep(4000);
    assert.ok(positions(w.events).length > after, 'room clock died after the woken master returned');
    w.close();
  } finally {
    if (m1) { m1.kill('SIGCONT'); reap(m1); }
    if (m2) reap(m2);
    try { execSync(`pkill -f 'watchparty.*${s.port}'`); } catch {}
    await s.stop();
  }
});

for (const [st, name] of results) console.log(`${st}  ${name}`);
const fails = results.filter(([st]) => st === 'FAIL').length;
console.log(`f2: ${results.length - fails}/${results.length} held`);
process.exit(fails ? 1 : 0);
