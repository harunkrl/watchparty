// Pause-sync test: two real clients (master + follower) against one server.
// Pauses the master's mpv directly via a second IPC connection (as if the
// user pressed space), then asserts the follower's mpv pauses and resumes.
// Requires mpv + ffmpeg in PATH; SKIPs otherwise.
// Run: nix-shell -p mpv ffmpeg --run "node tests/pause_sync.mjs"

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

const PORT = 18000 + Math.floor(Math.random() * 1000);
const BASE = `http://127.0.0.1:${PORT}`;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const MPV_ARGS = '--vo=null --ao=null --no-config --force-window=no';

// interop matrix (M3): {python,go} x {master,follower}
function arg(name, dflt) {
  const i = process.argv.indexOf('--' + name);
  return i >= 0 ? process.argv[i + 1] : dflt;
}
const masterEngine = arg('master-engine', 'python');
const followerEngine = arg('follower-engine', 'python');
if (!['python', 'go'].includes(masterEngine) || !['python', 'go'].includes(followerEngine)) {
  console.error('--master-engine/--follower-engine must be python or go');
  process.exit(2);
}
const GO_BIN = join(ROOT, 'client-go/watchparty');
if ((masterEngine === 'go' || followerEngine === 'go') && !existsSync(GO_BIN)) {
  console.error(`missing ${GO_BIN} — build it first: cd client-go && go build`);
  process.exit(2);
}
console.log(`interop: master=${masterEngine} follower=${followerEngine}`);

const dir = mkdtempSync(join(tmpdir(), 'wp-pause-'));
execSync(
  'ffmpeg -y -loglevel error -f lavfi -i testsrc2=duration=90:size=320x240:rate=10 ' +
    '-f lavfi -i sine=frequency=440:duration=90 -c:v libx264 -preset ultrafast -c:a aac -shortest ' +
    join(dir, 'test.mp4')
);

const server = spawn(process.execPath, [join(ROOT, 'server.js')], {
  env: { ...process.env, PORT: String(PORT), VIDEOS_DIR: dir },
  stdio: ['ignore', 'pipe', 'pipe'],
});
function client(name, extra, engine) {
  // go needs flags before the positional url; python's argparse takes either
  const argv = engine === 'go'
    ? [GO_BIN, '--headless', '--name', name, ...extra, BASE]
    : ['python3', join(ROOT, 'client.py'), BASE, '--name', name, ...extra];
  const c = spawn(argv[0], argv.slice(1), {
    env: { ...process.env, WATCHPARTY_MPV: MPV_ARGS },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  c.out = '';
  c.stdout.on('data', (d) => {
    c.out += d;
  });
  c.stderr.on('data', (d) => {
    c.out += d;
  });
  return c;
}
const master = client('M', ['--master'], masterEngine);
const follower = client('F', [], followerEngine);

// find the mpv IPC socket path of a client's mpv child
function mpvSocket(pyProc) {
  const mpvPid = execSync(`pgrep -P ${pyProc.pid}`).toString().trim();
  const cmdline = readFileSync(`/proc/${mpvPid}/cmdline`, 'utf8').split('\0');
  return cmdline.find((a) => a.startsWith('--input-ipc-server=')).slice('--input-ipc-server='.length);
}

// tiny mpv JSON-IPC caller
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
          msg.error === 'success' ? resolve(msg.data) : reject(new Error(msg.error));
        }
      } catch {
        reject(new Error('bad ipc line: ' + buf));
      }
    });
    s.on('error', reject);
  });
}

function waitUntil(fn, ms = 15000, what = '') {
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
      if (Date.now() - t0 > ms) return (clearInterval(iv), reject(new Error('timeout: ' + what)));
    }, 50);
  });
}

const cleanup = (code) => {
  // kill the mpv children first: a bare client.kill() orphans them
  for (const p of [master, follower]) {
    try {
      execSync(`pkill -TERM -P ${p.pid}`);
    } catch {}
    p.kill();
  }
  server.kill();
  rmSync(dir, { recursive: true, force: true });
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
  await waitUntil(() => master.out.includes('commands:') && follower.out.includes('commands:'), 15000, 'clients up');

  master.stdin.write('play test.mp4\n');
  // both players load, both report playing state
  await waitUntil(() => master.out.includes('> playing test.mp4'), 10000, 'master playing');
  await waitUntil(() => follower.out.includes('> playing test.mp4'), 10000, 'follower playing');

  const mSock = mpvSocket(master);
  const fSock = mpvSocket(follower);

  // let playback settle into a steady heartbeat exchange
  await sleep(4000);

  // PAUSE the master (simulates the user pressing space on the master window)
  await mpvCall(mSock, ['set_property', 'pause', true]);

  // follower's mpv must pause (discrete event now, heartbeat convergence later)
  const t0 = Date.now();
  let fPaused = false;
  while (Date.now() - t0 < 6000) {
    fPaused = (await mpvCall(fSock, ['get_property', 'pause'])) === true;
    if (fPaused) break;
    await sleep(200);
  }
  assert.ok(fPaused, 'follower did not pause within 6s');
  console.log(`follower paused ${Date.now() - t0}ms after master pause`);

  // stay paused for a while: no oscillation, no creeping seek
  await sleep(5000);
  const posWhilePaused = await mpvCall(fSock, ['get_property', 'time-pos']);
  await sleep(2000);
  const posWhilePaused2 = await mpvCall(fSock, ['get_property', 'time-pos']);
  assert.ok(Math.abs(posWhilePaused2 - posWhilePaused) < 0.3, `creeped while paused: ${posWhilePaused} -> ${posWhilePaused2}`);

  // RESUME
  await mpvCall(mSock, ['set_property', 'pause', false]);
  const t1 = Date.now();
  let fPlaying = false;
  while (Date.now() - t1 < 6000) {
    fPlaying = (await mpvCall(fSock, ['get_property', 'pause'])) === false;
    if (fPlaying) break;
    await sleep(200);
  }
  assert.ok(fPlaying, 'follower did not resume within 6s');

  // SEEK drift correction: master scrubs to 60s, follower must converge
  await mpvCall(mSock, ['seek', 60, 'absolute']);
  const t2 = Date.now();
  let converged = false;
  while (Date.now() - t2 < 12000) {
    const fp = await mpvCall(fSock, ['get_property', 'time-pos']);
    if (fp >= 59 && fp <= 65) {
      converged = true;
      console.log(`follower converged ${Date.now() - t2}ms after master seek`);
      break;
    }
    await sleep(300);
  }
  assert.ok(converged, `follower did not converge after master seek (at ${await mpvCall(fSock, ['get_property', 'time-pos'])})`);

  console.log('PASS: pause/resume/seek sync held');
  cleanup(0);
} catch (e) {
  console.error('FAIL:', e.message);
  console.error('--- master out ---\n' + master.out);
  console.error('--- follower out ---\n' + follower.out);
  cleanup(1);
}
