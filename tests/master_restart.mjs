// Master-restart test: with both sides playing, kill the master client,
// restart it, and verify the room resumes (resume-by-design): the new
// master reloads the file at the crash-time position and the follower
// converges to it instead of the room desyncing or starting from zero.
// Usage: nix-shell -p mpv ffmpeg --run "node tests/master_restart.mjs"

import { strict as assert } from 'node:assert';
import { execSync, spawn } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { createConnection } from 'node:net';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

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
const GO_BIN = join(ROOT, 'client-go/watchparty');
if (!existsSync(GO_BIN)) {
  console.error(`missing ${GO_BIN} — build it first: cd client-go && go build`);
  process.exit(2);
}

const PORT = 18400 + Math.floor(Math.random() * 400);
const BASE = `http://127.0.0.1:${PORT}`;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const MPV_ARGS = '--vo=null --ao=null --no-config --force-window=no';

const dir = mkdtempSync(join(tmpdir(), 'wp-mrestart-'));
execSync(
  'ffmpeg -y -loglevel error -f lavfi -i testsrc2=duration=90:size=320x240:rate=10 ' +
    '-f lavfi -i sine=frequency=440:duration=90 -c:v libx264 -preset ultrafast -c:a aac -shortest ' +
    join(dir, 'test.mp4')
);

const server = spawn(process.execPath, [join(ROOT, 'server.js')], {
  env: { ...process.env, PORT: String(PORT), VIDEOS_DIR: dir, STATE_FILE: join(dir, 'state.json') },
  stdio: ['ignore', 'pipe', 'pipe'],
});

function startMaster() {
  const m = spawn(GO_BIN, ['--headless', '--master', '--name', 'M', BASE], {
    env: { ...process.env, WATCHPARTY_MPV: MPV_ARGS },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  m.out = '';
  m.stdout.on('data', (d) => (m.out += d));
  m.stderr.on('data', (d) => (m.out += d));
  return m;
}

// find the mpv IPC socket of a client's mpv child (waits for spawn)
async function mpvSocket(p, tries = 100) {
  for (let i = 0; i < tries; i++) {
    try {
      const mpvPid = execSync(`pgrep -P ${p.pid}`).toString().trim().split('\n')[0];
      const cmdline = readFileSync(`/proc/${mpvPid}/cmdline`, 'utf8');
      const sock = cmdline.split('\0').find((a) => a.startsWith('--input-ipc-server='));
      if (sock) return sock.slice('--input-ipc-server='.length);
    } catch {}
    await sleep(100);
  }
  throw new Error('mpv socket not found');
}

const mpvGet = (sockPath, prop) =>
  new Promise((resolve, reject) => {
    const s = createConnection(sockPath, () =>
      s.write(JSON.stringify({ command: ['get_property', prop], request_id: 42 }) + '\n')
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

const cleanup = (code) => {
  for (const p of [master, follower, server]) {
    try { execSync(`pkill -TERM -P ${p.pid}`); } catch {}
    p.kill();
  }
  rmSync(dir, { recursive: true, force: true });
  process.exit(code);
};

let master = startMaster();
const follower = spawn(GO_BIN, ['--headless', '--name', 'F', BASE], {
  env: { ...process.env, WATCHPARTY_MPV: MPV_ARGS },
  stdio: ['pipe', 'pipe', 'pipe'],
});

try {
  let up = false;
  for (let i = 0; i < 100 && !up; i++) {
    try { up = (await fetch(`${BASE}/files`)).ok; } catch { await sleep(100); }
  }
  assert.ok(up, 'server did not come up');
  await sleep(2000);

  master.stdin.write('play test.mp4\n');
  await sleep(8000); // both sides are ~8s in

  const fSock = await mpvSocket(follower);
  const posBefore = await mpvGet(fSock, 'time-pos');
  assert.ok(posBefore > 5, `follower should be playing, got ${posBefore}`);

  // kill the master (mpv child dies with it)
  master.kill('SIGTERM');
  await sleep(2000);

  // restart the master: resume-by-design reloads the file at room position
  master = startMaster();
  const mSock = await mpvSocket(master);
  await sleep(3000); // hello -> load + seek + first heartbeats

  let ok = false;
  let last = {};
  for (let i = 0; i < 40 && !ok; i++) {
    try {
      const mPos = await mpvGet(mSock, 'time-pos');
      const fPos = await mpvGet(fSock, 'time-pos');
      const mPause = await mpvGet(mSock, 'pause');
      const fPath = await mpvGet(fSock, 'path');
      last = { mPos, fPos, mPause, fPath };
      ok = Math.abs(mPos - fPos) < 2.5 && !mPause && String(fPath).includes('test.mp4');
    } catch {}
    if (!ok) await sleep(500);
  }
  assert.ok(ok, `room did not reconverge after master restart: ${JSON.stringify(last)}`);
  assert.ok(last.mPos > 5, `master restarted from scratch instead of resuming: ${last.mPos}`);

  console.log(`PASS: master restart resumed the room (master@${last.mPos.toFixed(1)}s, follower@${last.fPos.toFixed(1)}s)`);
  cleanup(0);
} catch (e) {
  console.error('FAIL:', e.message);
  console.error('--- master ---\n' + master.out);
  console.error('--- follower ---\n' + follower.out);
  cleanup(1);
}
