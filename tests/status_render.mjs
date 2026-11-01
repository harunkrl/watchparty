// Status-render e2e (F8): the ONE non-headless client suite.
//
// Every other client suite runs the binary with --headless, so ui.go's
// RENDER path (the "▶ playing" chip, the progress bar) had ZERO coverage —
// the exact hole the field bug fell through (A8: the human's "the video
// plays but the TUI does not show it as playing").
//
// Scenario S3 (master-2's persistent case): the master dies at a film
// boundary; the follower's 'ended' post advances the queue; the follower
// plays the NEXT film while e.snap stays nil forever -> the engine never
// emits a UiState -> the TUI keeps the EOF state ("no position yet" / the
// paused icon) while the film PLAYS.
//
// The suite asserts the honest state: while the follower's mpv advances on
// the new film, the RENDERED TUI must show the playing state.
//
// The follower runs under a REAL pty (`script`, util-linux) and the suite
// answers the terminal capability queries (OSC 11, CSI 6n, ...) like a
// real terminal — without an answer the client stalls 5s on the query and
// eats its first line (A5 G1; the FREEZE-2 pty pattern). The pty width is
// nudged every second so the TUI repaints WHOLE frames (bubbletea only
// repaints the CHANGED lines — a chip that did not change is never
// written again, and the suite must read the CURRENT screen, not the
// first frame that mentioned it).
//
// Requires mpv + ffmpeg + script(1) in PATH; SKIPs otherwise.
// Run: node tests/status_render.mjs [--bin client-go/watchparty]

import { strict as assert } from 'node:assert';
import { execSync, spawn } from 'node:child_process';
import { appendFileSync, closeSync, existsSync, mkdtempSync, openSync, readFileSync, readSync, statSync, writeFileSync } from 'node:fs';
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

// port band 19500-19899: no other suite uses it (the CI bands stop at
// 18989; wan_baseline's 19000-19499 is a manual tool)
const PORT = 19500 + Math.floor(Math.random() * 400);
const BASE = `http://127.0.0.1:${PORT}`;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
// no window: the TUI is what we read, the video only has to PLAY
const MPV_ARGS = '--vo=null --ao=null --no-config --force-window=no';

const dir = mkdtempSync(join(tmpdir(), 'wp-status-'));
// a.mp4: long enough that the follower's snapshot goes stale (6s) before
// the film ends. b.mp4: the film the queue advances to.
const video = (name, secs) =>
  execSync(
    `ffmpeg -y -loglevel error -f lavfi -i testsrc2=duration=${secs}:size=320x240:rate=10 ` +
      `-f lavfi -i sine=frequency=440:duration=${secs} -c:v libx264 -preset ultrafast -c:a aac -shortest ` +
      join(dir, name)
  );
video('a.mp4', 16);
video('b.mp4', 30);

const T0 = Date.now();
const log = (...a) => console.log(`[${((Date.now() - T0) / 1000).toFixed(1)}s]`, ...a);

const server = spawn(process.execPath, [join(ROOT, 'server.js')], {
  env: { ...process.env, PORT: String(PORT), VIDEOS_DIR: dir, STATE_FILE: join(dir, 'state.json') },
  stdio: ['ignore', 'pipe', 'pipe'],
});
let serverOut = '';
server.stdout.on('data', (d) => (serverOut += d));
server.stderr.on('data', (d) => (serverOut += d));

// ---------------------------------------------------------------- the pty
// QUERIES: [query, reply]. A real terminal answers these; a dumb pty makes
// the client wait 5s for the answer (A5 G1).
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
// captures everything it paints. The width nudge in the background makes
// the TUI repaint whole frames (bubbletea repaints only changed lines).
function startFollower(outFile) {
  const cmd =
    `stty rows 50 cols 200; stty -echo; ` +
    `(while :; do stty -F /dev/tty cols 201 2>/dev/null; sleep 1; stty -F /dev/tty cols 200 2>/dev/null; sleep 1; done) & ` +
    `exec ${BIN} --name F ${BASE}`;
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
  p.on('exit', (code, sig) => log(`the follower client exited code=${code} sig=${sig}`));
  return p;
}

// stripAnsi removes the escape sequences; flat removes the line breaks so a
// wrapped chip ("▶ pla" + "ying") is still found.
const stripAnsi = (s) =>
  s
    .replace(/\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)/g, '')
    .replace(/\x1b\[[0-9;?$>< ]*[A-Za-z]/g, '')
    .replace(/\x1b[()][A-Z0-9]/g, '')
    .replace(/\x1b[=>]/g, '');
const flat = (s) => stripAnsi(s).replace(/\r?\n/g, '');

// readFrom reads the file from a BYTE offset: the frames painted after the
// offset are the ones that count.
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
  const pids = execSync(`pgrep -P ${clientPid}`).toString().trim().split('\n').filter(Boolean);
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

// waitUntil polls fn (it may be async) until it returns a truthy value.
async function waitUntil(fn, ms, what) {
  const t0 = Date.now();
  for (;;) {
    let v;
    try {
      v = await fn();
    } catch (e) {
      throw new Error(`${what}: ${e.message}`);
    }
    if (v) return v;
    if (Date.now() - t0 > ms) throw new Error('timeout: ' + what);
    await sleep(150);
  }
}

const master = spawn(BIN, ['--headless', '--master', '--name', 'M', BASE], {
  env: { ...process.env, WATCHPARTY_MPV: MPV_ARGS },
  stdio: ['pipe', 'pipe', 'pipe'],
});
let masterOut = '';
master.stdout.on('data', (d) => (masterOut += d));
master.stderr.on('data', (d) => (masterOut += d));

const outFile = join(dir, 'follower.tui');
const follower = startFollower(outFile);

const cleanup = async (code) => {
  for (const p of [follower, master]) {
    try {
      execSync(`pkill -TERM -P ${p.pid}`);
    } catch {}
    try {
      p.kill();
    } catch {}
  }
  await stopServer(server, dir);
  process.exit(code);
};

try {
  let up = false;
  for (let i = 0; i < 100 && !up; i++) {
    try {
      const r = await fetch(`${BASE}/files`, { signal: AbortSignal.timeout(500) });
      up = r.ok;
    } catch {
      await sleep(100);
    }
  }
  assert.ok(up, 'server did not come up');

  // the master plays a.mp4 and queues b.mp4; the follower (pty) joins and
  // loads a.mp4 too
  master.stdin.write('play a.mp4\n');
  await waitUntil(() => masterOut.includes('> playing a.mp4'), 15000, 'master playing a.mp4');
  master.stdin.write('queue b.mp4\n');
  await waitUntil(() => masterOut.includes('queue: [b.mp4]'), 10000, 'b.mp4 queued');
  log('the master plays a.mp4 and queues b.mp4');

  const followerPid = execSync(`pgrep -P ${follower.pid}`).toString().trim().split('\n')[0];
  const fSock = await waitUntil(() => mpvSocket(followerPid), 15000, 'the follower mpv socket');
  await waitUntil(async () => {
    const p = await mpvCall(fSock, ['get_property', 'path']);
    return p && p.endsWith('/a.mp4') ? p : null;
  }, 20000, 'the follower loaded a.mp4');
  await waitUntil(async () => (await mpvCall(fSock, ['get_property', 'time-pos'])) > 0.5, 10000, 'the follower plays');
  log('the follower plays a.mp4');

  // the follower must have the master's heartbeat (its drift chip appears
  // only with a snapshot) — otherwise the follower never had a clock and
  // the scenario would prove nothing
  await waitUntil(() => flat(readFileSync(outFile, 'utf8')).includes('▸ '), 10000, 'the follower got the heartbeat');
  log('the follower has the master heartbeat (the drift chip is there)');

  // the master dies: the room loses its clock owner (the claim dies with
  // the SSE connection) and the follower's snapshot goes stale after 6s
  try {
    execSync(`pkill -TERM -P ${master.pid}`);
  } catch {}
  master.kill('SIGKILL');
  log('the master is dead');

  // a.mp4 ends -> the follower posts 'ended' -> the queue advances -> the
  // follower loads b.mp4 (snap stays nil: no master sends heartbeats)
  const bPath = await waitUntil(async () => {    try {
      const p = await mpvCall(fSock, ['get_property', 'path']);
      return p && p.endsWith('/b.mp4') ? p : null;
    } catch {
      return null; // the property is unavailable while mpv is idle
    }
  }, 45000, 'the queue advanced to b.mp4');
  log(`the queue advanced to ${bPath}`);

  // the film really plays (the mpv position advances)
  const pos1 = await mpvCall(fSock, ['get_property', 'time-pos']);
  await sleep(3000);
  const pos2 = await mpvCall(fSock, ['get_property', 'time-pos']);
  assert.ok(pos2 > pos1 + 0.5, `the film is not playing: ${pos1} -> ${pos2}`);

  // only the frames from NOW on count: the state above was the bug window
  const from = follower.bytes;
  await sleep(6000); // three poll ticks
  const tail = flat(readFrom(outFile, from));

  assert.ok(
    !tail.includes('no position yet'),
    `the TUI still says "no position yet" while the film plays (pos ${pos1} -> ${pos2})\n` +
      `--- rendered tail ---\n${tail.slice(-1200)}`
  );
  // the CHIP is "▶ playing"; the engine's log line is "▶ playing <file>" —
  // the lookahead tells them apart
  assert.ok(
    /▶ playing(?![a-z0-9])/.test(tail),
    `the TUI does not show the playing state while the film plays (pos ${pos1} -> ${pos2})\n` +
      `--- rendered tail ---\n${tail.slice(-1200)}`
  );
  log('PASS: the rendered TUI shows the playing state while the film plays');
  await cleanup(0);
} catch (e) {
  console.error('FAIL:', e.message);
  try {
    console.error('--- follower pty tail ---\n' + flat(readFileSync(outFile, 'utf8')).slice(-1200));
  } catch {}
  console.error('--- master out ---\n' + masterOut);
  console.error('--- server out ---\n' + serverOut);
  try {
    const whole = flat(readFileSync(outFile, 'utf8'));
    writeFileSync('/tmp/wp-status-capture.txt', whole);
    console.error('--- markers ---');
    for (const m of ['no position yet', '▶ playing', '⏸', 'drift', 'post:', 'ended']) {
      console.error(`  ${JSON.stringify(m)}: ${whole.split(m).length - 1}`);
    }
  } catch {}
  await cleanup(1);
}
