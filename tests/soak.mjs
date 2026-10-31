// Soak test: both sides act (pause/resume/seek) and the room must stay sane —
// no echo storms (the bug: each side reposting what it merely applied), and
// final convergence. An SSE observer counts every relayed event; budgets are
// storm detectors, not exact expectations.
// Usage: nix-shell -p mpv ffmpeg --run "node tests/soak.mjs [--master-engine go] [--follower-engine go]"

import { strict as assert } from 'node:assert';
import { execSync, spawn } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { createConnection } from 'node:net';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { stopServer, rmWithRetry } from './helpers.mjs';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');

function has(bin) {
  try {
    execSync(`command -v ${bin}`, { stdio: 'ignore' });
    return true;
  } catch {
    return false;
  }
}
if (!has('mpv') || !has('ffmpeg')) {
  console.log('SKIP: need mpv and ffmpeg in PATH');
  process.exit(0);
}

const PORT = 18200 + Math.floor(Math.random() * 500);
const BASE = `http://127.0.0.1:${PORT}`;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const MPV_ARGS = '--vo=null --ao=null --no-config --force-window=no';

const GO_BIN = join(ROOT, 'client-go/watchparty');
if (!existsSync(GO_BIN)) {
  console.error(`missing ${GO_BIN} — build it first: cd client-go && go build`);
  process.exit(2);
}

const dir = mkdtempSync(join(tmpdir(), 'wp-soak-'));
execSync(
  'ffmpeg -y -loglevel error -f lavfi -i testsrc2=duration=120:size=320x240:rate=10 ' +
    '-f lavfi -i sine=frequency=440:duration=120 -c:v libx264 -preset ultrafast -c:a aac -shortest ' +
    join(dir, 'test.mp4')
);

const server = spawn(process.execPath, [join(ROOT, 'server.js')], {
  env: { ...process.env, PORT: String(PORT), VIDEOS_DIR: dir, STATE_FILE: join(dir, 'state.json') },
  stdio: ['ignore', 'pipe', 'pipe'],
});
function client(name, extra) {
  const argv = [GO_BIN, '--headless', '--name', name, ...extra, BASE];
  const c = spawn(argv[0], argv.slice(1), {
    env: { ...process.env, WATCHPARTY_MPV: MPV_ARGS },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  c.out = '';
  c.stdout.on('data', (d) => (c.out += d));
  c.stderr.on('data', (d) => (c.out += d));
  return c;
}
function mpvSocket(p) {
  const mpvPid = execSync(`pgrep -P ${p.pid}`).toString().trim();
  return readFileSync(`/proc/${mpvPid}/cmdline`, 'utf8')
    .split('\0')
    .find((a) => a.startsWith('--input-ipc-server='))
    .slice('--input-ipc-server='.length);
}
const mpvCall = (sockPath, command) =>
  new Promise((resolve, reject) => {
    const s = createConnection(sockPath, () =>
      s.write(JSON.stringify({ command, request_id: 42 }) + '\n')
    );
    let buf = '';
    s.on('data', (d) => {
      buf += d;
      const i = buf.indexOf('\n');
      if (i < 0) return;
      const msg = JSON.parse(buf.slice(0, i));
      s.end();
      if (msg.error === 'success') resolve(msg.data);
      else reject(new Error(msg.error));
    });
    s.on('error', reject);
  });

// observer: count relayed discrete events
// observer: count relayed discrete events (started once the server is up)
const counts = { pause: 0, play: 0, seek: 0, load: 0 };
async function observe() {
  const res = await fetch(`${BASE}/events?id=soak-obs&nick=obs`);
  const reader = res.body.getReader();
  const dec = new TextDecoder();
  let buf = '';
  while (true) {
    const { done, value } = await reader.read();
    if (done) return;
    buf += dec.decode(value, { stream: true });
    let i;
    while ((i = buf.indexOf('\n\n')) >= 0) {
      const chunk = buf.slice(0, i);
      buf = buf.slice(i + 2);
      const line = chunk.split('\n').find((l) => l.startsWith('data: '));
      if (!line) continue;
      const ev = JSON.parse(line.slice(6));
      if (counts[ev.type] !== undefined && ev.from !== 'soak-obs') counts[ev.type]++;
    }
  }
}

const master = client('M', ['--master']);
const follower = client('F', []);
const cleanup = async (code) => {
  for (const p of [master, follower]) {
    try { execSync(`pkill -TERM -P ${p.pid}`); } catch {}
    p.kill();
  }
  await stopServer(server, dir); // kill -> wait for the exit -> rm (F6/C1)
  process.exit(code);
};

try {
  let up = false;
  for (let i = 0; i < 100 && !up; i++) {
    try { up = (await fetch(`${BASE}/files`)).ok; } catch { await sleep(100); }
  }
  assert.ok(up, 'server did not come up');
  observe().catch(() => {}); // a dead observer must fail the load guard below
  await sleep(2000);

  master.stdin.write('play test.mp4\n');
  await sleep(6000); // load + settle into heartbeat exchange
  const mSock = mpvSocket(master);
  const fSock = mpvSocket(follower);

  // alternating user actions on both sides
  await mpvCall(mSock, ['set_property', 'pause', true]);   // master pauses
  await sleep(3000);
  await mpvCall(mSock, ['set_property', 'pause', false]);
  await sleep(3000);
  await mpvCall(fSock, ['set_property', 'pause', true]);   // follower pauses
  await sleep(3000);
  await mpvCall(fSock, ['set_property', 'pause', false]);
  await sleep(3000);
  await mpvCall(mSock, ['seek', 40, 'absolute']);          // master scrubs
  await sleep(4000);
  await mpvCall(fSock, ['seek', 20, 'absolute']);          // follower scrubs
  await sleep(4000);
  await mpvCall(fSock, ['set_property', 'pause', true]);   // follower pauses again
  await sleep(3000);
  await mpvCall(fSock, ['set_property', 'pause', false]);
  await sleep(8000); // settle

  const mPause = await mpvCall(mSock, ['get_property', 'pause']);
  const fPause = await mpvCall(fSock, ['get_property', 'pause']);
  const mPos = await mpvCall(mSock, ['get_property', 'time-pos']);
  const fPos = await mpvCall(fSock, ['get_property', 'time-pos']);
  console.log(`relay counts: ${JSON.stringify(counts)}`);
  assert.ok(counts.load >= 1, 'observer saw nothing — SSE observer was dead');
  console.log(`final: master ${mPause ? 'paused' : 'playing'}@${mPos?.toFixed(1)} follower ${fPause ? 'paused' : 'playing'}@${fPos?.toFixed(1)}`);

  assert.equal(mPause, false, 'master ended paused');
  assert.equal(fPause, false, 'follower ended paused');
  assert.ok(Math.abs(mPos - fPos) < 2.5, `positions diverged: ${mPos} vs ${fPos}`);
  // storm budgets: 3 pause + 3 play + 2 seek user actions; echoes would blow these up
  assert.ok(counts.pause <= 8, `pause echo storm: ${counts.pause}`);
  assert.ok(counts.play <= 8, `play echo storm: ${counts.play}`);
  assert.ok(counts.seek <= 8, `seek echo storm: ${counts.seek}`);

  console.log('PASS: soak held — no echo storms, both sides converged');
  await cleanup(0);
} catch (e) {
  console.error('FAIL:', e.message);
  console.error(`counts: ${JSON.stringify(counts)}`);
  console.error('--- master out ---\n' + master.out);
  console.error('--- follower out ---\n' + follower.out);
  await cleanup(1);
}
