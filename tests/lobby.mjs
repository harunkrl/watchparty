// Lobby-first e2e (L1, tests lane): an IDLE room must NOT auto-load.
//
// The room liveness is decided CLIENT-side from the hello roster (the
// frozen contract, preamble §1-7):
//   - a LIVE room (>= 2 unique nicks) auto-loads and adopts the position
//     (the core — P2 pins it: it must not move),
//   - an IDLE room (<= 1 nick: that one entry is always us) shows a
//     "last watched" card and waits for an explicit resume (P1), and a
//     load event hands the room over and clears the card (P3).
// P4 pins the invite line, the /invite re-print and the two visible
// refusals.
//
// The clients run under a REAL pty (script, util-linux) and the suite
// answers the terminal capability queries (OSC 11, CSI 6n, ...) like a
// real terminal — without an answer the client stalls 5s on the query
// and eats its first line (A5 G1; the FREEZE-2 pty pattern).
//
// Requires mpv + ffmpeg + script(1) in PATH; SKIPs otherwise.
// Run: node tests/lobby.mjs [--bin client-go/watchparty]

import { strict as assert } from 'node:assert';
import { execSync, spawn } from 'node:child_process';
import { appendFileSync, closeSync, existsSync, mkdtempSync, openSync, readFileSync, readSync, statSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:http';
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
  console.error(`missing ${BIN} — build it first: cd client-go && go build -o watchparty .`);
  process.exit(2);
}

// port band 20100-20399: free (status_render takes 19500-19899,
// room_pause 19950-19999; the CI bands stop at 18989)
let portCounter = 20100 + Math.floor(Math.random() * 100);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const MPV_ARGS = '--vo=null --ao=null --no-config --force-window=no';
const T0 = Date.now();
const log = (...a) => console.log(`[${((Date.now() - T0) / 1000).toFixed(1)}s]`, ...a);

// the strings worker1 owns (the frozen contract, preamble §5-6):
//   card line 1: "last watched: <name> · mm:ss[ ⏸] — enter resume · /play pick another"
//   invite line:  "invite: <url>" / "invite: <url> or <urlAlt>"
//   refusals:     "nothing to resume (no last-watched lobby)"
//                "no invite address yet — waiting for the server hello"
const CARD_RE = /last watched/i;
const INVITE_RE = /invite:\s*https?:\/\//gi;
// the re-print can land as the WRAPPED CONTINUATION of the same line (the
// system pane wraps "invite: <url> or <urlAlt>" onto a second row), so
// the re-print check accepts the continuation too.
const INVITE_ANY_RE = /invite:\s*https?:\/\/|or https?:\/\//gi;
const NO_RESUME_RE = /nothing to resume|no last-watched/i;
const NO_INVITE_RE = /no invite address|invite address|waiting for the server hello/i;

const countMatches = (s, re) => (s.match(new RegExp(re.source, 'gi')) || []).length;

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

// startClient runs the REAL client (no --headless) under a pty and
// captures everything it paints.
function startClient(base, outFile, name, extraArgs = []) {
  const cmd =
    `stty rows 50 cols 200; stty -echo; ` +
    `exec ${BIN} --name ${name} ${extraArgs.join(' ')} ${base}`;
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
  p.on('exit', (code, sig) => log(`the ${name} client exited code=${code} sig=${sig}`));
  return p;
}

// startHeadless runs the client in headless mode (stdin commands): the
// master legs need no screen, only the room.
function startHeadless(base, name, extraArgs = []) {
  const p = spawn(BIN, ['--headless', ...extraArgs, '--name', name, base], {
    env: { ...process.env, WATCHPARTY_MPV: MPV_ARGS },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  p.out = '';
  p.stdout.on('data', (d) => (p.out += d.toString('utf8')));
  p.stderr.on('data', (d) => (p.out += d.toString('utf8')));
  p.stdin.on('error', () => {});
  return p;
}

const stripAnsi = (s) =>
  s
    .replace(/\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)/g, '')
    .replace(/\x1b\[[0-9;?$>< ]*[A-Za-z]/g, '')
    .replace(/\x1b[()][A-Z0-9]/g, '')
    .replace(/\x1b[=>]/g, '');
const flat = (s) => stripAnsi(s).replace(/\r?\n/g, '');

// readFrom reads the file from a BYTE offset: only the frames painted
// after the offset count.
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
  // scan every child: the pty command also runs a background stty loop
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
    // the mpv IPC answers a get_property in milliseconds: a call that
    // does not answer in 3s is a wedged player, not a slow one. Without
    // the bound the whole suite would hang on one unanswered command.
    const timer = setTimeout(() => {
      try {
        s.destroy();
      } catch {}
      reject(new Error('mpv ipc timeout: ' + JSON.stringify(command)));
    }, 3000);
    const done = (fn, v) => {
      clearTimeout(timer);
      fn(v);
    };
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
          if (msg.error === 'success') done(resolve, msg.data);
          else done(reject, new Error(msg.error));
        }
      } catch {
        done(reject, new Error('bad ipc line: ' + buf));
      }
    });
    s.on('error', (e) => done(reject, e));
  });
}

// loadedFile: the file mpv plays right now ("" = nothing loaded).
async function loadedFile(sock) {
  const p = await mpvCall(sock, ['get_property', 'path']).catch(() => null);
  if (!p || p === 'none') return '';
  return p;
}
const mpvPos = (sock) => mpvCall(sock, ['get_property', 'time-pos']).catch(() => null);
const mpvPaused = async (sock) => {
  const v = await mpvCall(sock, ['get_property', 'pause']).catch(() => null);
  return v === true;
};

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

// childPid waits for the client's own process: script forks it a moment
// after the spawn, so pgrep finds nothing until then.
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

// ------------------------------------------------------------- the server
const procs = [];

// startServer boots server.js on its own port with its own state file.
// state: the persisted room state (null = a fresh room).
async function startServer(state) {
  const dir = mkdtempSync(join(tmpdir(), 'wp-lobby-'));
  const stateFile = join(dir, 'state.json');
  if (state) writeFileSync(stateFile, JSON.stringify(state));
  const port = portCounter++;
  const base = `http://127.0.0.1:${port}`;
  const proc = spawn(process.execPath, [join(ROOT, 'server.js')], {
    env: { ...process.env, PORT: String(port), VIDEOS_DIR: VIDEOS, STATE_FILE: stateFile },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let out = '';
  proc.stdout.on('data', (d) => (out += d));
  proc.stderr.on('data', (d) => (out += d));
  procs.push(proc);
  await waitUntil(async () => (await fetch(`${base}/files`).catch(() => null))?.ok, 15000, 'the server came up');
  return {
    base,
    dir,
    stateFile,
    proc,
    post: async (body) =>
      (
        await fetch(`${base}/cmd`, {
          method: 'POST',
          headers: { 'content-type': 'application/json' },
          body: JSON.stringify(body),
        })
      ).status,
    health: async () => (await fetch(`${base}/health`)).json(),
    roomState: () => {
      try {
        return JSON.parse(readFileSync(stateFile, 'utf8'));
      } catch {
        return null;
      }
    },
    stop: async () => {
      await stopServer(proc, dir);
    },
  };
}

// the videos (one set, shared: the room state is per scenario, the media
// is not)
const VIDEOS = mkdtempSync(join(tmpdir(), 'wp-lobby-media-'));
const makeVideo = (name, secs) =>
  execSync(
    `ffmpeg -y -loglevel error -f lavfi -i testsrc2=duration=${secs}:size=320x240:rate=10 ` +
      `-f lavfi -i sine=frequency=440:duration=${secs} -c:v libx264 -preset ultrafast -c:a aac -shortest ` +
      join(VIDEOS, name)
  );
makeVideo('movie.mp4', 60);
makeVideo('other.mp4', 20);

const results = [];
async function scenario(name, fn) {
  try {
    await fn();
    results.push(['PASS', name]);
    log(`PASS ${name}`);
  } catch (e) {
    results.push(['FAIL', `${name} — ${e.message}`]);
    log(`FAIL ${name} — ${e.message}`);
  }
}

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
  try {
    execSync(`rm -rf ${VIDEOS}`);
  } catch {}
  process.exit(code);
};

// =========================================================== P1: the lobby
// The idle room: a persisted room state, ONE client (the roster holds
// only us) -> no auto-load, the lobby card, and an explicit empty-enter
// resume.
//
// The late-joiner leg is the core of the round: the resumed point must
// live in the ROOM state, or the next joiner lands at 0:00 (the master
// ruling #86a: the joiner must also land in the room's paused state).
async function idleResume(paused) {
  const label = paused ? 'paused' : 'playing';
  await scenario(`[L1-1${paused ? 'a' : 'b'}] an idle room waits for a resume (${label})`, async () => {
    const srv = await startServer({
      now: 'movie.mp4',
      queue: [],
      pos: 42,
      paused,
      dur: 60,
      wall: Date.now(),
    });
    const outFile = join(srv.dir, 'a.tui');
    const a = startClient(srv.base, outFile, 'Ada');
    procs.push(a);
    const aPid = await childPid(a.pid);
    const aSock = await waitUntil(() => mpvSocket(aPid), 15000, 'the mpv socket');

    // the hello arrived: the room registers the client BEFORE it writes
    // the hello, so the listener count is the earliest sound of it
    await waitUntil(async () => (await srv.health()).listeners >= 1, 15000, 'the hello');
    await sleep(2500); // the engine had its chance to auto-load

    // THE gate: an idle room must NOT auto-load the persisted file
    const loaded = await loadedFile(aSock);
    assert.equal(
      loaded,
      '',
      `the idle room auto-loaded the persisted file (mpv plays ${loaded}) — the lobby gate did not fire`
    );

    // the lobby card is on screen (the chat pane, above the input line)
    await waitUntil(() => CARD_RE.test(flat(readFileSync(outFile, 'utf8'))), 15000, 'the lobby card');
    const card = flat(readFileSync(outFile, 'utf8'));
    assert.ok(
      /0?0:42/.test(card),
      `the lobby card does not show the remembered position (42s)\n--- card ---\n${card.slice(-1200)}`
    );
    log(`the lobby card is on screen (${paused ? 'paused' : 'playing'} lobby)`);

    // the explicit resume: an EMPTY enter on the chat input
    a.stdin.write('\r');
    await waitUntil(async () => (await loadedFile(aSock)).endsWith('movie.mp4'), 30000, 'the resume loaded the file');
    const aPos = await waitUntil(async () => {
      const p = await mpvPos(aSock);
      return p != null && p > 38 ? p : null;
    }, 30000, 'the resume seeked to the remembered position');
    assert.ok(Math.abs(aPos - 42) < 6, `the resume landed at ${aPos}s, want ~42s`);
    assert.equal(await mpvPaused(aSock), paused, `the resumed player is not ${paused ? 'paused' : 'playing'}`);
    log(`the resume landed at ${aPos.toFixed(1)}s (${paused ? 'paused' : 'playing'})`);

    // the resume point must live in the ROOM state: a follower resume
    // posts seek(pos) once the file is live (else the next joiner lands
    // at 0:00)
    const room = await waitUntil(async () => {
      const s = srv.roomState();
      return s && typeof s.pos === 'number' && Math.abs(s.pos - 42) < 3 ? s : null;
    }, 25000, 'the room state carries the resume point');
    assert.equal(room.paused, paused, `the room state lost the pause: ${JSON.stringify(room)}`);
    log(`the room state carries the resume point (pos ${room.pos}, paused ${room.paused})`);

    // the late joiner: a plain follower lands on the resume point
    const bFile = join(srv.dir, 'b.tui');
    const b = startClient(srv.base, bFile, 'Bob');
    procs.push(b);
    const bPid = await childPid(b.pid);
    const bSock = await waitUntil(() => mpvSocket(bPid), 15000, 'the joiner mpv socket');
    await waitUntil(async () => (await loadedFile(bSock)).endsWith('movie.mp4'), 30000, 'the joiner auto-loaded');
    const bPos = await waitUntil(async () => {
      const p = await mpvPos(bSock);
      return p != null && p > 5 ? p : null;
    }, 30000, 'the joiner landed on a position');
    assert.ok(
      Math.abs(bPos - 42) < 8,
      `the late joiner landed at ${bPos}s, want ~42s — the resume point did not reach the room`
    );
    assert.equal(
      await mpvPaused(bSock),
      paused,
      `the late joiner is not ${paused ? 'paused' : 'playing'} (the room state did not carry the pause)`
    );
    log(`the late joiner landed at ${bPos.toFixed(1)}s (${paused ? 'paused' : 'playing'})`);
  });
}
await idleResume(true);
await idleResume(false);

// ================================================ P2: the live room (core)
// A live room (two nicks) must behave EXACTLY as before: auto-load and
// position adoption. The core must not move.
await scenario('[L1-2] a live room still auto-loads and adopts the position', async () => {
  const srv = await startServer(null);
  const master = startHeadless(srv.base, 'Mira', ['--master']);
  procs.push(master);
  master.stdin.write('play movie.mp4\n');
  await waitUntil(() => master.out.includes('> playing movie.mp4'), 20000, 'the master plays');
  const mSock = await waitUntil(() => mpvSocket(master.pid), 15000, 'the master mpv socket');
  // the master must be WELL into the film: a joiner at 0:00 next to a
  // master at 1.8s would fake a convergence
  await waitUntil(async () => (await mpvPos(mSock)) > 4, 25000, 'the master plays');
  log('the master is playing');

  const outFile = join(srv.dir, 'b.tui');
  const b = startClient(srv.base, outFile, 'Ben');
  procs.push(b);
  const bPid = await childPid(b.pid);
  const bSock = await waitUntil(() => mpvSocket(bPid), 15000, 'the joiner mpv socket');
  await waitUntil(async () => (await loadedFile(bSock)).endsWith('movie.mp4'), 30000, 'the joiner auto-loaded');
  // the live room adopts the master's clock: the follower converges onto
  // the master's position (the drift correction), so the landing point is
  // the master's, not 0:00
  const conv = await waitUntil(async () => {
    const [p, m] = await Promise.all([mpvPos(bSock), mpvPos(mSock)]);
    if (p == null || m == null) return null;
    return Math.abs(p - m) < 2 ? { p, m } : null;
  }, 40000, 'the joiner converged on the master clock');
  log(`the joiner converged on the master clock (${conv.p.toFixed(1)}s vs ${conv.m.toFixed(1)}s)`);
  assert.ok(conv.p > 3, `the joiner landed at ${conv.p}s — the live room did not adopt the position`);
  // a live room has NO lobby: the card must not appear
  const frames = flat(readFileSync(outFile, 'utf8'));
  assert.ok(!CARD_RE.test(frames), `the live room showed a lobby card\n--- frames ---\n${frames.slice(-1200)}`);
  log(`the live joiner landed at ${conv.p.toFixed(1)}s (master ${conv.m.toFixed(1)}s), no lobby card`);
});

// =============================================== P3: the load-event handoff
// A client sits in the lobby on an idle room; a master joins and plays
// another file -> the lobby client loads the NEW file (the load-event
// path) and the lobby is CLEARED (a stale lobby would resume the old
// file on the next empty enter).
await scenario('[L1-3] a load event hands the room over and clears the lobby', async () => {
  const srv = await startServer({
    now: 'movie.mp4',
    queue: [],
    pos: 42,
    paused: false,
    dur: 60,
    wall: Date.now(),
  });
  const outFile = join(srv.dir, 'a.tui');
  const a = startClient(srv.base, outFile, 'Ada');
  procs.push(a);
  const aPid = await childPid(a.pid);
  const aSock = await waitUntil(() => mpvSocket(aPid), 15000, 'the mpv socket');
  await waitUntil(() => CARD_RE.test(flat(readFileSync(outFile, 'utf8'))), 15000, 'the lobby card');
  assert.equal(await loadedFile(aSock), '', 'the idle room auto-loaded before the handoff');
  log('the client sits in the lobby');

  const master = startHeadless(srv.base, 'Mira', ['--master']);
  procs.push(master);
  await waitUntil(() => master.out.includes('> playing'), 25000, 'the master joins the room');
  master.stdin.write('play other.mp4\n');
  await waitUntil(() => master.out.includes('> playing other.mp4'), 20000, 'the master plays the other file');
  log('the master plays other.mp4');

  await waitUntil(async () => (await loadedFile(aSock)).endsWith('other.mp4'), 30000, 'the lobby client loaded the new file');
  log('the lobby client loaded the new file');

  // the lobby is CLEARED: an empty enter must not resume the old file
  a.stdin.write('\r');
  await sleep(5000);
  const after = await loadedFile(aSock);
  assert.ok(
    after.endsWith('other.mp4'),
    `the stale lobby resumed the old file (mpv plays ${after}) — the lobby was not cleared`
  );
  log('the lobby was cleared (the empty enter did not resume the old file)');
});

// ================================ P5: the PAUSED live room (ruling #86b)
// A follower that joins a PAUSED room must land on the room's position
// and stay paused (the master ruling #86b: the hello's position was
// dropped by the master's first heartbeat, so the joiner sat at 0:00).
// The guard leg: a later LOCAL scrub must not be dragged back by the
// heartbeats — the fix is load-scoped, not a paused-heartbeat seek loop.
await scenario('[L1-6] a paused live room: the joiner lands at the room position', async () => {
  const srv = await startServer(null);
  const master = startHeadless(srv.base, 'Mira', ['--master']);
  procs.push(master);
  master.stdin.write('play movie.mp4\n');
  await waitUntil(() => master.out.includes('> playing movie.mp4'), 20000, 'the master plays');
  const mSock = await waitUntil(() => mpvSocket(master.pid), 15000, 'the master mpv socket');
  await waitUntil(async () => (await mpvPos(mSock)) > 4, 25000, 'the master plays');
  // freeze the room: a paused room serves the FROZEN position
  await srv.post({ type: 'pause', from: 'probe', nick: 'probe' });
  await waitUntil(async () => (await srv.health()).paused === true, 15000, 'the room is paused');
  await waitUntil(async () => mpvPaused(mSock), 15000, 'the master paused');
  const roomPos = await waitUntil(async () => {
    const s = srv.roomState();
    return s && typeof s.pos === 'number' && s.pos > 2 ? s.pos : null;
  }, 15000, 'the room state carries the position');
  log(`the room is paused at ${roomPos.toFixed(1)}s`);

  const outFile = join(srv.dir, 'b.tui');
  const b = startClient(srv.base, outFile, 'Ben');
  procs.push(b);
  const bPid = await childPid(b.pid);
  const bSock = await waitUntil(() => mpvSocket(bPid), 15000, 'the joiner mpv socket');
  await waitUntil(async () => (await loadedFile(bSock)).endsWith('movie.mp4'), 30000, 'the joiner auto-loaded');
  const bPos = await waitUntil(async () => {
    const p = await mpvPos(bSock);
    return p != null && p > 0.5 ? p : null;
  }, 30000, 'the joiner landed on a position');
  assert.ok(
    Math.abs(bPos - roomPos) < 2.5,
    `the joiner landed at ${bPos}s, want ~${roomPos.toFixed(1)}s — the paused room did not carry its position`
  );
  assert.ok(bPos > 2, `the joiner landed at ${bPos}s — the paused room dropped its position`);
  assert.equal(await mpvPaused(bSock), true, 'the joiner is not paused (the room is)');
  log(`the paused joiner landed at ${bPos.toFixed(1)}s and stayed paused`);

  // the guard leg: a local scrub must survive the heartbeats (the fix
  // must not be a paused-heartbeat seek loop)
  await mpvCall(bSock, ['seek', '20', 'absolute']);
  await sleep(8000); // four heartbeats
  const after = await mpvPos(bSock);
  assert.ok(
    after > 10,
    `the local scrub was dragged back to ${after}s — the paused-room fix is a seek loop, not a load-scoped adopt`
  );
  log(`the local scrub survived the heartbeats (${after.toFixed(1)}s)`);
});

// ================================================== P4: the invite + refusals
// The hello prints the invite line, /invite re-prints it, and the two
// refusals are VISIBLE (a silent no-op is a bug — the F9 yardstick).
await scenario('[L1-4] the invite line, the /invite re-print and the refusals', async () => {
  const srv = await startServer(null);
  const outFile = join(srv.dir, 'a.tui');
  const a = startClient(srv.base, outFile, 'Ada');
  procs.push(a);
  const aPid = await childPid(a.pid);
  await waitUntil(() => mpvSocket(aPid), 15000, 'the mpv socket');

  // the hello prints "invite: <url>" in the system pane
  await waitUntil(() => countMatches(flat(readFileSync(outFile, 'utf8')), INVITE_RE) >= 1, 20000, 'the invite line');
  log('the hello printed the invite line');

  // the repaint noise: an idle client rewrites the system pane only when
  // its content changes. Measure the idle rate, then demand MORE invite
  // lines after /invite than the idle rate predicts.
  const c0 = countMatches(readFileSync(outFile, 'utf8'), INVITE_RE);
  await sleep(3000);
  const idle = countMatches(readFileSync(outFile, 'utf8'), INVITE_RE) - c0;
  const before = a.bytes;
  a.stdin.write('/invite\r');
  await waitUntil(
    () => countMatches(readFrom(outFile, before), INVITE_ANY_RE) > idle,
    15000,
    'the /invite re-print'
  );
  log('the /invite re-printed the invite line');

  // the empty enter with NO lobby is a visible refusal
  const before2 = a.bytes;
  a.stdin.write('\r');
  await waitUntil(() => NO_RESUME_RE.test(flat(readFrom(outFile, before2))), 15000, 'the resume refusal');
  log('the empty enter refused visibly (no lobby)');
});

// the invite refusal needs a hello WITHOUT an invite address: the server
// always knows its own address, so a tiny RESPONDER plays the server
// here (the client must refuse visibly, not sit mute).
await scenario('[L1-5] /invite without a known address refuses visibly', async () => {
  const responder = createServer((req, res) => {
    if (req.url.startsWith('/events')) {
      res.writeHead(200, {
        'content-type': 'text/event-stream',
        'cache-control': 'no-cache',
        connection: 'keep-alive',
      });
      res.write(
        `data: ${JSON.stringify({ type: 'hello', now: '', queue: [], roster: ['Nobody'], p: false })}\n\n`
      );
      return; // the stream stays open
    }
    if (req.url.startsWith('/files')) {
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify(['movie.mp4']));
      return;
    }
    if (req.method === 'POST') {
      res.writeHead(204);
      res.end();
      return;
    }
    res.writeHead(404);
    res.end();
  });
  const port = portCounter++;
  await new Promise((r) => responder.listen(port, '127.0.0.1', r));
  const base = `http://127.0.0.1:${port}`;
  const outFile = join(mkdtempSync(join(tmpdir(), 'wp-lobby-')), 'a.tui');
  const a = startClient(base, outFile, 'Ada');
  procs.push(a);
  const aPid = await childPid(a.pid);
  await waitUntil(() => mpvSocket(aPid), 15000, 'the mpv socket');
  await sleep(2000); // the hello lands
  const before = a.bytes;
  a.stdin.write('/invite\r');
  await waitUntil(() => NO_INVITE_RE.test(flat(readFrom(outFile, before))), 15000, 'the invite refusal');
  responder.close();
  log('the /invite without an address refused visibly');
});

// ================================ G2: an absent roster keeps the live path
// The lobby gate reads the hello roster. An ABSENT roster (a legacy or
// malformed hello) is NOT an idle signal: the client keeps the live
// path (auto-load) — byte-compatible with the pre-L1 tree. This closes
// M13's escape (the gate must not read "no roster" as "idle").
await scenario('[L1-7] an absent roster keeps the live path (the client auto-loads)', async () => {
  const responder = createServer((req, res) => {
    const u = new URL(req.url, 'http://127.0.0.1');
    if (u.pathname === '/events') {
      res.writeHead(200, {
        'content-type': 'text/event-stream',
        'cache-control': 'no-cache',
        connection: 'keep-alive',
      });
      // NO roster field at all
      res.write(`data: ${JSON.stringify({ type: 'hello', now: 'movie.mp4', queue: [], p: false })}\n\n`);
      return; // the stream stays open
    }
    if (u.pathname.startsWith('/files')) {
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify(['movie.mp4']));
      return;
    }
    if (u.pathname.startsWith('/v/')) {
      const body = readFileSync(join(VIDEOS, decodeURIComponent(u.pathname.slice(3))));
      const range = req.headers.range;
      if (range) {
        const m = /bytes=(\d*)-(\d*)/.exec(range);
        const start = m[1] ? parseInt(m[1], 10) : 0;
        let end = m[2] ? parseInt(m[2], 10) : body.length - 1;
        if (end >= body.length) end = body.length - 1;
        const chunk = body.slice(start, end + 1);
        res.writeHead(206, {
          'content-type': 'video/mp4',
          'accept-ranges': 'bytes',
          'content-length': chunk.length,
          'content-range': `bytes ${start}-${end}/${body.length}`,
        });
        res.end(chunk);
        return;
      }
      res.writeHead(200, {
        'content-type': 'video/mp4',
        'accept-ranges': 'bytes',
        'content-length': body.length,
      });
      res.end(body);
      return;
    }
    if (req.method === 'POST') {
      res.writeHead(204);
      res.end();
      return;
    }
    res.writeHead(404);
    res.end();
  });
  const port = portCounter++;
  await new Promise((r) => responder.listen(port, '127.0.0.1', r));
  const base = `http://127.0.0.1:${port}`;
  try {
    const outFile = join(mkdtempSync(join(tmpdir(), 'wp-lobby-')), 'a.tui');
    const a = startClient(base, outFile, 'Ada');
    procs.push(a);
    const aPid = await childPid(a.pid);
    const aSock = await waitUntil(() => mpvSocket(aPid), 15000, 'the mpv socket');
    // the live path: the file loads on its own (no roster = not idle)
    await waitUntil(async () => (await loadedFile(aSock)).endsWith('movie.mp4'), 30000, 'the client auto-loaded');
    // and NO lobby card: an absent roster is not an idle signal
    assert.ok(
      !CARD_RE.test(flat(readFileSync(outFile, 'utf8'))),
      'the absent roster produced a lobby card — the gate read it as idle'
    );
    log('the absent roster kept the live path (auto-load, no card)');
  } finally {
    responder.close();
  }
});

// ================================ F1: a heartbeat auto-joins the lobby
// The lobby is a hello-time snapshot. A heartbeat IS the "the room turned
// live" signal: the lobby client auto-joins (the card dies, the file
// loads, and it lands on the heartbeat's position — paused or playing).
// An IDLE room has no heartbeat, so the lobby keeps holding there (the
// L1-1/L1-3 semantics stand).
async function lobbyAutoJoin(paused) {
  const label = paused ? 'paused' : 'playing';
  await scenario(
    `[L1-8${paused ? 'a' : 'b'}] a lobby client auto-joins on the first heartbeat (${label} room)`,
    async () => {
      const srv = await startServer({
        now: 'movie.mp4',
        queue: [],
        pos: 42,
        paused,
        dur: 60,
        wall: Date.now(),
      });
      const outFile = join(srv.dir, 'a.tui');
      const a = startClient(srv.base, outFile, 'Ada');
      procs.push(a);
      const aPid = await childPid(a.pid);
      const aSock = await waitUntil(() => mpvSocket(aPid), 15000, 'the mpv socket');
      await waitUntil(async () => (await srv.health()).listeners >= 1, 15000, 'the hello');
      await waitUntil(() => CARD_RE.test(flat(readFileSync(outFile, 'utf8'))), 15000, 'the lobby card');
      assert.equal(await loadedFile(aSock), '', 'the idle room auto-loaded before any heartbeat');

      // a peer (the master) joins and drives the room: its heartbeat is
      // the "the room turned live" signal
      const b = startHeadless(srv.base, 'Bob', ['--master']);
      procs.push(b);
      // the headless client is a direct child: its mpv is ITS child
      const bSock = await waitUntil(() => mpvSocket(b.pid), 15000, 'the peer mpv socket');
      await waitUntil(async () => (await loadedFile(bSock)).endsWith('movie.mp4'), 30000, 'the peer loaded the file');
      const bPos = await waitUntil(async () => {
        const p = await mpvPos(bSock);
        return p != null && p > 40 ? p : null;
      }, 30000, 'the peer sits at the room position');

      // the lobby client auto-joins: the card dies, the file loads, and
      // it lands on the heartbeat's position. The landing is PARKED until
      // the duration is known (the F1 clamp), so the wait polls for the
      // CONVERGENCE with the room's clock — not for "some position": in a
      // playing room the client counts up from 0 before the parked seek
      // lands.
      await waitUntil(async () => (await loadedFile(aSock)).endsWith('movie.mp4'), 30000, 'the lobby client auto-joined');
      const landed = await waitUntil(async () => {
        const a = await mpvPos(aSock);
        const b = await mpvPos(bSock);
        return a != null && b != null && Math.abs(a - b) < 3 ? { a, b } : null;
      }, 30000, 'the lobby client landed on the heartbeat position');
      const aPos = landed.a;
      assert.equal(
        await mpvPaused(aSock),
        paused,
        `the lobby client is ${(await mpvPaused(aSock)) ? 'paused' : 'playing'}, want ${paused ? 'paused' : 'playing'}`
      );
      // the card is gone: the last frames do not carry it any more
      const tail = flat(readFileSync(outFile, 'utf8').slice(-3000));
      assert.ok(!CARD_RE.test(tail), 'the lobby card is still on screen after the auto-join');
      log(`the lobby client auto-joined at ${aPos.toFixed(1)}s (the heartbeat said ${bPos.toFixed(1)}s)`);
    }
  );
}
await lobbyAutoJoin(false);
await lobbyAutoJoin(true);

// ================================ G1: the co-resident lobby peer
// Two clients in the lobby (the roster dedupes to ONE entry — the
// false-idle edge: both clients share one nick), one resumes: the peer
// receives the load event, starts the file and has NO position of its
// own — it must land on the resume position (the seek the resume posts
// can arrive while the peer's file is still loading: the peer must PARK
// the position and apply it, not sit at 0:00).
await scenario('[L1-9] two lobby clients: the peer lands on the resume position', async () => {
  const srv = await startServer({
    now: 'movie.mp4',
    queue: [],
    pos: 42,
    paused: true,
    dur: 60,
    wall: Date.now(),
  });
  const outA = join(srv.dir, 'a.tui');
  const outB = join(srv.dir, 'b.tui');
  const a = startClient(srv.base, outA, 'Ada');
  procs.push(a);
  const b = startClient(srv.base, outB, 'Ada'); // the same nick: the roster dedupes
  procs.push(b);
  const aPid = await childPid(a.pid);
  const bPid = await childPid(b.pid);
  const aSock = await waitUntil(() => mpvSocket(aPid), 15000, 'the mpv socket');
  const bSock = await waitUntil(() => mpvSocket(bPid), 15000, 'the peer mpv socket');
  await waitUntil(async () => (await srv.health()).listeners >= 2, 15000, 'both hellos');
  await waitUntil(() => CARD_RE.test(flat(readFileSync(outA, 'utf8'))), 15000, 'the lobby card');
  await waitUntil(() => CARD_RE.test(flat(readFileSync(outB, 'utf8'))), 15000, 'the peer lobby card');
  assert.equal(await loadedFile(bSock), '', 'the peer auto-loaded (it was not in the lobby)');

  // one resumes
  a.stdin.write('\r');
  await waitUntil(async () => (await loadedFile(aSock)).endsWith('movie.mp4'), 30000, 'the resume loaded the file');
  // the peer follows the handoff and lands on the resume position
  await waitUntil(async () => (await loadedFile(bSock)).endsWith('movie.mp4'), 30000, 'the peer loaded the file');
  const bPos = await waitUntil(async () => {
    const p = await mpvPos(bSock);
    return p != null && p > 0.5 ? p : null;
  }, 30000, 'the peer landed on a position');
  assert.ok(
    Math.abs(bPos - 42) < 3,
    `the peer landed at ${bPos}s, want ~42s — the resume position was lost (the peer sits at 0:00)`
  );
  assert.equal(await mpvPaused(bSock), true, 'the peer is not paused (the room is)');
  log(`the peer landed on the resume position (${bPos.toFixed(1)}s, paused)`);
});

// ---------------------------------------------------------------- verdict
const failed = results.filter(([v]) => v === 'FAIL');
for (const [v, name] of results) console.log(`${v}: ${name}`);
if (failed.length) {
  console.error(`FAIL: ${failed.length}/${results.length} scenarios failed`);
  await cleanup(1);
}
console.log(`PASS: ${results.length} lobby scenarios`);
await cleanup(0);
