// Room-pause e2e (F9, tests lane): the two pause paths that have no
// coverage yet.
//
// A) The LONE FOLLOWER's ctrl+space must pause the ROOM end to end: the
//    follower has no master, so it has no snapshot — before the fix the
//    engine's follower branch returned silently (a dead key) and the room
//    never learned about the pause (A9 S1: the server always relayed and
//    persisted it, so the blocker was purely client-side).
// B) The JOIN-LATER handoff (A9 S1 CASE B): a lone follower pauses the
//    room, a master joins later — the room must STAY paused. Before the
//    fix the hello carried neither pos nor p (the master started unpaused
//    and its first heartbeat p=false erased the room's pause), and the
//    follower's mpv resumed under its feet.
// C) An UNKNOWN key sequence (the kitty spelling) must leave a VISIBLE
//    reason in the system pane — a silent no-op is a bug (the round's
//    yardstick). The wording is worker1's; the matcher accepts the
//    plausible spellings.
//
// The follower runs under a real pty (script) and the suite answers the
// terminal capability queries (A5 G1) like a real terminal.
//
// Requires mpv + ffmpeg + script(1); SKIPs otherwise.
// Run: node tests/room_pause.mjs [--bin client-go/watchparty]

import { strict as assert } from 'node:assert';
import { execSync, spawn } from 'node:child_process';
import { appendFileSync, closeSync, existsSync, mkdtempSync, openSync, readFileSync, readSync, statSync } from 'node:fs';
import { createConnection } from 'node:net';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { stopServer } from './helpers.mjs';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');

function has(bin) {
  try {
    execSync(`command -v ${bin}`, { stdio: 'ignore' });
    return true;
  } catch {
    return false;
  }
}
if (!has('mpv') || !has('ffmpeg') || !has('script')) {
  console.log('SKIP: need mpv, ffmpeg and script(1) in PATH');
  process.exit(0);
}

const argv = process.argv.slice(2);
const binIdx = argv.indexOf('--bin');
const BIN = binIdx >= 0 ? argv[binIdx + 1] : join(ROOT, 'client-go/watchparty');
if (!existsSync(BIN)) {
  console.error(`missing ${BIN} — build it first: cd client-go && go build`);
  process.exit(2);
}

// port band 19950-19999: free (the CI bands stop at 18989, status_render
// takes 19500-19899)
const PORT = 19950 + Math.floor(Math.random() * 50);
const BASE = `http://127.0.0.1:${PORT}`;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const MPV_ARGS = '--vo=null --ao=null --no-config --force-window=no';
const T0 = Date.now();
const log = (...a) => console.log(`[${((Date.now() - T0) / 1000).toFixed(1)}s]`, ...a);

const dir = mkdtempSync(join(tmpdir(), 'wp-roompause-'));
execSync(
  'ffmpeg -y -loglevel error -f lavfi -i testsrc2=duration=180:size=320x240:rate=10 ' +
    '-f lavfi -i sine=frequency=440:duration=180 -c:v libx264 -preset ultrafast -c:a aac -shortest ' +
    join(dir, 'movie.mp4')
);

const server = spawn(process.execPath, [join(ROOT, 'server.js')], {
  env: { ...process.env, PORT: String(PORT), VIDEOS_DIR: dir, STATE_FILE: join(dir, 'state.json') },
  stdio: ['ignore', 'pipe', 'pipe'],
});
let serverOut = '';
server.stdout.on('data', (d) => (serverOut += d));
server.stderr.on('data', (d) => (serverOut += d));
const post = async (body) =>
  (await fetch(`${BASE}/cmd`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) })).status;
const health = async () => (await fetch(`${BASE}/health`)).json();

// ---------------------------------------------------------------- the pty
const QUERIES = [
  ['\x1b]11;?', '\x1b]11;rgb:0000/0000/0000\x1b\\'],
  ['\x1b]11;?\x07', '\x1b]11;rgb:0000/0000/0000\x1b\\'],
  ['\x1b[6n', '\x1b[1;1R'],
  ['\x1b[?2026$p', '\x1b[?2026;2$y'],
  ['\x1b[>0;q', '\x1bP>|probe(1.0)\x1b\\'],
  ['\x1b[?u', '\x1b[?0u'],
  ['\x1b[c', '\x1b[?62;1;6c'],
  ['\x1b[>c', '\x1b[>0;276;0c'],
];

// startFollower runs the REAL client (no --headless) under a pty and
// captures everything it paints.
function startFollower(outFile, name = 'F') {
  const cmd = `stty rows 50 cols 200; stty -echo; exec ${BIN} --name ${name} ${BASE}`;
  const p = spawn('script', ['-qec', cmd, '/dev/null'], {
    env: { ...process.env, WATCHPARTY_MPV: MPV_ARGS },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  p.text = '';
  p.bytes = 0;
  p.answered = new Set();
  p.stdout.on('data', (d) => {
    p.text += d.toString('utf8');
    appendFileSync(outFile, d);
    p.bytes += d.length;
    for (const [q, a] of QUERIES) {
      let i = p.text.indexOf(q);
      while (i >= 0) {
        if (!p.answered.has(i + q)) {
          p.answered.add(i + q);
          try {
            p.stdin.write(a);
          } catch {}
        }
        i = p.text.indexOf(q, i + 1);
      }
    }
  });
  p.stderr.on('data', (d) => {
    p.text += d.toString('utf8');
  });
  p.stdin.on('error', () => {}); // the script may exit before we finish writing
  p.on('exit', (code, sig) => log(`the follower client exited code=${code} sig=${sig}`));
  return p;
}

const stripAnsi = (s) =>
  s
    .replace(/\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)/g, '')
    .replace(/\x1b\[[0-9;?$>< ]*[A-Za-z]/g, '')
    .replace(/\x1b[()][A-Z0-9]/g, '')
    .replace(/\x1b[=>]/g, '');
const flat = (s) => stripAnsi(s).replace(/\r?\n/g, '');

function readFrom(file, pos) {
  const fd = openSync(file, 'r');
  try {
    const size = statSync(file).size;
    const len = Math.max(0, size - pos);
    const buf = Buffer.alloc(len);
    if (len) readSync(fd, buf, 0, len, pos);
    return buf.toString('utf8');
  } finally {
    closeSync(fd);
  }
}

// ------------------------------------------------------- the mpv plumbing
function mpvSocket(clientPid) {
  let pids = [];
  try {
    pids = execSync(`pgrep -P ${clientPid}`).toString().trim().split('\n').filter(Boolean);
  } catch {
    pids = []; // the mpv may not be spawned yet — the caller retries
  }
  for (const pid of pids) {
    let cmdline;
    try {
      cmdline = readFileSync(`/proc/${pid}/cmdline`, 'utf8').split('\0');
    } catch {
      continue;
    }
    const arg = cmdline.find((a) => a.startsWith('--input-ipc-server='));
    if (arg) return arg.slice('--input-ipc-server='.length);
  }
  throw new Error('no ipc socket');
}

function mpvCall(sockPath, command) {
  return new Promise((resolve, reject) => {
    const s = createConnection(sockPath, () => {
      s.write(JSON.stringify({ command, request_id: 42 }) + '\n');
    });
    let buf = '';
    s.on('data', (d) => {
      buf += d;
      const i = buf.indexOf('\n');
      if (i < 0) return;
      try {
        const msg = JSON.parse(buf.slice(0, i));
        if (msg.request_id === 42) {
          s.end();
          if (msg.error === 'success') resolve(msg.data);
          else reject(new Error(msg.error));
        }
      } catch {
        reject(new Error('bad ipc line: ' + buf));
      }
    });
    s.on('error', reject);
  });
}

async function waitUntil(fn, ms, what) {
  const t0 = Date.now();
  let last = null;
  for (;;) {
    let v;
    try {
      v = await fn();
    } catch (e) {
      last = e; // the target may not exist yet (a spawn race) — retry
      v = null;
    }
    if (v) return v;
    if (Date.now() - t0 > ms) throw new Error(`timeout: ${what}${last ? ` (${last.message})` : ''}`);
    await sleep(150);
  }
}

// childPid waits for the client's own process (script forks it a moment
// after the spawn — pgrep finds nothing until then)
function childPid(parentPid, ms = 15000) {
  return waitUntil(() => {
    try {
      const pids = execSync(`pgrep -P ${parentPid}`).toString().trim().split('\n').filter(Boolean);
      return pids.length ? pids[0] : null;
    } catch {
      return null;
    }
  }, ms, 'the client process');
}

// the cleanup: kill -> WAIT for the exits -> rm (the F8 master_restart
// lesson: a raw rm raced the server's exit-time state flush)
const procs = [];
const cleanup = async (code) => {
  for (const p of procs) {
    try {
      execSync(`pkill -TERM -P ${p.pid}`);
    } catch {}
    try {
      p.kill();
    } catch {}
  }
  for (const p of procs) {
    if (p.exitCode !== null || p.signalCode !== null) continue;
    const exited = new Promise((r) => p.once('exit', r));
    await Promise.race([exited, sleep(5000)]);
    if (p.exitCode === null && p.signalCode === null) {
      const killed = new Promise((r) => p.once('exit', r));
      p.kill('SIGKILL');
      await Promise.race([killed, sleep(2000)]);
    }
  }
  await stopServer(server, dir);
  process.exit(code);
};

const results = [];
async function scenario(name, fn) {
  try {
    await fn();
    results.push(['PASS', name]);
  } catch (e) {
    results.push(['FAIL', `${name} — ${e.message}`]);
  }
}

// the room has a file (the follower joins into it) but NO master yet
for (let i = 0; i < 100; i++) {
  try {
    if ((await fetch(`${BASE}/files`, { signal: AbortSignal.timeout(500) })).ok) break;
  } catch {
    await sleep(100);
  }
}
await post({ type: 'load', from: 'F-0', nick: 'f', name: 'movie.mp4' });

// ---------------------------------------------------------------- A
await scenario('[F9-1] a lone follower ctrl+space pauses the ROOM', async () => {
  const outFile = join(dir, 'followerA.tui');
  const follower = startFollower(outFile, 'F');
  procs.push(follower);
  try {
    // the warm-up rule (A9 S2): the room must be LIVE first — the follower
    // must actually play the film before a key means anything
    const clientPid = await childPid(follower.pid);
    const fSock = await waitUntil(() => mpvSocket(clientPid), 15000, 'the follower mpv socket');
    await waitUntil(async () => (await mpvCall(fSock, ['get_property', 'time-pos'])) > 0.5, 20000, 'the follower plays');
    // F9 gate: the ENGINE polls every 2s — mpv's time-pos crossing 0.5s does
    // NOT mean the engine has a polled position yet, and the key must not
    // arrive before it (the honest refusal would fire and the room would
    // never learn the pause). Settle past one poll tick (master-2's F1
    // harness does the same and passes 10/10).
    await sleep(3000);
    log('A: the follower is playing (the room is live)');

    follower.stdin.write('\x00'); // ctrl+space (the NUL spelling)
    await sleep(2500);
    const h = await health();
    const localPaused = await mpvCall(fSock, ['get_property', 'pause']);
    assert.equal(h.paused, true, `the room did not learn about the follower's pause: ${JSON.stringify(h)}`);
    assert.equal(localPaused, true, 'the follower did not pause its own player');
  } finally {
    for (const p of [follower]) {
      try {
        execSync(`pkill -TERM -P ${p.pid}`);
      } catch {}
      p.kill();
    }
  }
});

// ---------------------------------------------------------------- B
await scenario('[F9-2] a paused room STAYS paused when a master joins later', async () => {
  // F9 gate: scenario A leaves the room PAUSED (the lone follower's pause
  // is a room action now) — a follower that joins a paused room adopts the
  // pause and never crosses 0.5s, so the warm-up would time out. Resume the
  // room FIRST; B pauses it again itself, below.
  await post({ type: 'play', from: 'F-1', nick: 'f' });
  const outFile = join(dir, 'followerB.tui');
  const follower = startFollower(outFile, 'F');
  procs.push(follower);
  let master = null;
  let fSock = null;
  try {
    const clientPid = await childPid(follower.pid);
    fSock = await waitUntil(() => mpvSocket(clientPid), 15000, 'the follower mpv socket');
    await waitUntil(async () => (await mpvCall(fSock, ['get_property', 'time-pos'])) > 0.5, 20000, 'the follower plays');
    log('B: the follower is playing');

    // the follower's pause, landed: the room state via the room protocol
    // (the server relays and persists ANY client's pause — A9 S1) and the
    // local effect via the mpv. The KEY path is scenario A's job.
    await post({ type: 'pause', from: 'F-1', nick: 'f' });
    await mpvCall(fSock, ['set_property', 'pause', true]);
    assert.equal((await health()).paused, true, 'the room did not take the pause');
    log('B: the room is paused');

    // a master joins LATER
    master = spawn(BIN, ['--headless', '--master', '--name', 'M', BASE], {
      env: { ...process.env, WATCHPARTY_MPV: MPV_ARGS },
      stdio: ['pipe', 'pipe', 'pipe'],
    });
    procs.push(master);
    let mOut = '';
    master.stdout.on('data', (d) => (mOut += d));
    master.stderr.on('data', (d) => (mOut += d));

    // the master's first heartbeat must NOT erase the room's pause
    await sleep(6000); // the master's poll ticks every 2s — three heartbeats
    const h = await health();
    const fPaused = await mpvCall(fSock, ['get_property', 'pause']).catch(() => 'n/a');
    assert.equal(h.paused, true, `the room reverted to playing after the master joined: ${JSON.stringify(h)}`);
    assert.equal(fPaused, true, `the follower's mpv resumed when the master joined: pause=${fPaused}`);
  } finally {
    for (const p of [follower, master]) {
      if (!p) continue;
      try {
        execSync(`pkill -TERM -P ${p.pid}`);
      } catch {}
      p.kill();
    }
  }
});

// ---------------------------------------------------------------- C
await scenario('[F9-3] an unknown key sequence leaves a VISIBLE reason', async () => {
  // F9 gate: scenario B legitimately leaves the room PAUSED (the no-steamroll
  // behaviour IS the fix) — a follower that joins a paused room adopts the
  // pause and never crosses 0.5s, so the warm-up would time out. Resume the
  // room FIRST (the play relay unpauses the newcomer) -> the scenarios stay
  // independent of B's outcome.
  await post({ type: 'play', from: 'F-2', nick: 'f' });
  const outFile = join(dir, 'followerC.tui');
  const follower = startFollower(outFile, 'F');
  procs.push(follower);
  try {
    const clientPid = await childPid(follower.pid);
    const fSock = await waitUntil(() => mpvSocket(clientPid), 15000, 'the follower mpv socket');
    await waitUntil(async () => (await mpvCall(fSock, ['get_property', 'time-pos'])) > 0.5, 20000, 'the follower plays');
    await sleep(1000);

    const from = follower.bytes;
    follower.stdin.write('\x1b[1;6;2D'); // the kitty ctrl+shift+left spelling
    await sleep(2500);
    const fresh = flat(readFrom(outFile, from));
    assert.ok(
      /unknown|csi|sequence|unmapped|escape|not recognized/i.test(fresh),
      `the unknown key sequence left no visible reason:\n${fresh.slice(-600)}`
    );
  } finally {
    for (const p of [follower]) {
      try {
        execSync(`pkill -TERM -P ${p.pid}`);
      } catch {}
      p.kill();
    }
  }
});

for (const [st, name] of results) console.log(`${st}  ${name}`);
const fails = results.filter(([st]) => st === 'FAIL').length;
console.log(`room_pause: ${results.length - fails}/${results.length} held`);
await cleanup(fails ? 1 : 0);
