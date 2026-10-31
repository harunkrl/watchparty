// Batch C (stall family) end-to-end: a follower must NOT pause the room
// when the master's heartbeat goes silent. The wanlink proxy's silence
// mode black-holes the SSE data WITHOUT an RST: the TCP links stay
// healthy, so the client's reconnect logic never fires and the follower
// simply stops hearing the room's clock.
//
// Usage: node tests/stall_family.mjs [--bin client-go/watchparty]
//                                    [--server server.js] [--silence 12]
// Needs: node, ffmpeg and the built client binary.

import { execFileSync, execSync, spawn } from 'node:child_process';
import { createConnection } from 'node:net';
import { existsSync, mkdtempSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { Proxy } from './wanlink.mjs';
import { stopServer, rmWithRetry } from './helpers.mjs';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const argv = process.argv.slice(2);
const arg = (n, d) => { const i = argv.indexOf(n); return i >= 0 && argv[i + 1] ? argv[i + 1] : d; };
const OPT = {
  bin: arg('--bin', join(ROOT, 'client-go/watchparty')),
  server: arg('--server', join(ROOT, 'server.js')),
  silence: Number(arg('--silence', 12)), // seconds of a silent master (> staleAge 6s)
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// A5-5: the f2 guard pattern — this suite needs mpv, ffmpeg and the built
// client binary; without them it must SKIP, not FAIL (CI installs mpv+ffmpeg
// before running it, so only local runs take the skip path).
const has = (bin) => {
  try { execSync(`command -v ${bin}`, { stdio: 'ignore' }); return true; } catch { return false; }
};
if (!(has('mpv') && has('ffmpeg') && existsSync(OPT.bin))) {
  console.log('SKIP: stall_family needs mpv, ffmpeg and the client binary (--bin)');
  process.exit(0);
}
const children = [];
const cleanup = () => { for (const c of children) { try { c.kill('SIGKILL'); } catch {} } };
process.on('SIGINT', () => { cleanup(); process.exit(130); });
setTimeout(() => { cleanup(); process.exit(9); }, 180000).unref();

const PORT = 18100 + Math.floor(Math.random() * 800);
const PROXY_PORT = PORT + 1;

function startServer(port, videos) {
  const s = spawn(process.execPath, [OPT.server], {
    env: { ...process.env, PORT: String(port), VIDEOS_DIR: videos, STATE_FILE: join(videos, 'state.json') },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  children.push(s);
  s.out = '';
  s.stdout.on('data', (d) => { s.out += d; });
  s.stderr.on('data', (d) => { s.out += d; });
  return s;
}

function startClient(name, master, base) {
  const p = spawn(OPT.bin, ['--headless', ...(master ? ['--master'] : []), '--name', name, base], {
    env: { ...process.env, WATCHPARTY_MPV: '--vo=null --ao=null --no-config --force-window=no' },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  children.push(p);
  p.lines = [];
  const push = (d) => {
    for (const l of d.split('\n')) if (l.trim()) p.lines.push(l);
  };
  p.stdout.on('data', (d) => push(d.toString()));
  p.stderr.on('data', (d) => push(d.toString()));
  return p;
}

async function waitServer(port) {
  for (let i = 0; i < 100; i++) {
    try { const r = await fetch(`http://127.0.0.1:${port}/files`); if (r.ok) return true; } catch {}
    await sleep(100);
  }
  return false;
}

function makeVideo(dir, seconds) {
  const file = join(dir, 'film.mp4');
  if (existsSync(file)) return file;
  execFileSync('ffmpeg', [
    '-y', '-loglevel', 'error',
    '-f', 'lavfi', '-i', `testsrc2=duration=${seconds}:size=320x240:rate=10`,
    '-f', 'lavfi', '-i', `sine=frequency=440:duration=${seconds}`,
    '-c:v', 'libx264', '-preset', 'ultrafast', '-c:a', 'aac', '-shortest', file,
  ], { stdio: 'ignore' });
  return file;
}

function linesMatching(client, re) { return client.lines.filter((l) => re.test(l)); }

// mpv's IPC socket of a client's mpv child (linux only).
function mpvSocketPath(client) {  for (let i = 0; i < 150; i++) {
    try {
      const pids = execFileSync('sh', ['-c', `pgrep -P ${client.pid}`], { encoding: 'utf8' }).trim().split('\n').filter(Boolean);
      for (const pid of pids) {
        const cmd = readFileSync(`/proc/${pid}/cmdline`, 'utf8');
        const s = cmd.split('\0').find((a) => a.startsWith('--input-ipc-server='));
        if (s) return s.slice('--input-ipc-server='.length);
      }
    } catch {}
    execFileSync('sh', ['-c', 'sleep 0.1']);
  }
  return null;
}

class MpvIpc {
  constructor(sockPath) { this.sockPath = sockPath; this.seq = 1; this.pending = new Map(); this.buf = ''; }
  async connect() {
    this.conn = createConnection(this.sockPath);
    await new Promise((res, rej) => { this.conn.once('connect', res); this.conn.once('error', rej); });
    this.conn.on('data', (d) => {
      this.buf += d.toString('utf8');
      let i;
      while ((i = this.buf.indexOf('\n')) >= 0) {
        const line = this.buf.slice(0, i); this.buf = this.buf.slice(i + 1);
        if (!line.trim()) continue;
        let m; try { m = JSON.parse(line); } catch { continue; }
        if (m.request_id && this.pending.has(m.request_id)) {
          const p = this.pending.get(m.request_id); this.pending.delete(m.request_id);
          if (m.error === 'success') p.res(m.data); else p.res(undefined);
        }
      }
    });
    return this;
  }
  cmd(command, ...args) {
    const id = this.seq++;
    return new Promise((res) => {
      this.pending.set(id, { res });
      this.conn.write(JSON.stringify({ command: [command, ...args], request_id: id }) + '\n');
      setTimeout(() => { if (this.pending.has(id)) { this.pending.delete(id); res(undefined); } }, 3000);
    });
  }
  get(prop) { return this.cmd('get_property', prop); }
  close() { try { this.conn.destroy(); } catch {} }
}

const dir = mkdtempSync(join(tmpdir(), 'wp-stall-'));
let server = null;
let failed = 0;
const check = (ok, msg) => {
  if (ok) { console.log(`PASS: ${msg}`); return; }
  failed++;
  console.error(`FAIL: ${msg}`);
};

try {
  makeVideo(dir, 240); // 240s: both scenarios (silence + reconnect) must stay clear of EOF
  server = startServer(PORT, dir);
  if (!await waitServer(PORT)) throw new Error('the server did not come up');

  // the follower goes through the proxy (its link can be silenced), the
  // master is local (it keeps heartbeating to the server)
  const proxy = new Proxy(PROXY_PORT, PORT, 0, 'F');
  await proxy.listen();
  const master = startClient('M', true, `http://127.0.0.1:${PORT}`);
  const follower = startClient('F', false, `http://127.0.0.1:${PROXY_PORT}`);

  await sleep(2500);
  // the master starts the film for the room
  master.stdin.write('play film.mp4\n');
  await sleep(6000);

  const mIpc = await new MpvIpc(await mpvSocketPath(master)).connect();
  const fIpc = await new MpvIpc(await mpvSocketPath(follower)).connect();
  const mPos = await mIpc.get('time-pos');
  const fPos = await fIpc.get('time-pos');
  check(typeof mPos === 'number' && typeof fPos === 'number', `both clients play (master ${mPos}, follower ${fPos})`);
  check(Math.abs(mPos - fPos) < 3, `the room is in sync before the silence (${mPos} vs ${fPos})`);

  // ---- the follower stops hearing the room (the proxy eats the SSE) ----
  // The links stay healthy: the reconnect logic never fires, the follower
  // simply stops hearing the room's clock.
  proxy.silenceEvents(true);
  await sleep(3000);

  // ---- the room jumps forward 40s (a seek the follower cannot see) ----
  // The follower's snapshot ages while it is deaf; when the heartbeat
  // comes back the gap is a RESUME (a sleep, a suspend, a long outage),
  // not starvation: the follower must re-seek and must NOT pause the room.
  await mIpc.cmd('seek', 40, 'absolute');
  await sleep(4000);

  proxy.silenceEvents(false); // the heartbeat comes back
  await sleep(9000);

  const pausedDuringSilence = linesMatching(master, /^> pause$/).length + linesMatching(follower, /^> pause$/).length;
  check(pausedDuringSilence === 0, `a resume must not pause the room (pauses: ${pausedDuringSilence})`);

  const mPos2 = await mIpc.get('time-pos');
  const fPos2 = await fIpc.get('time-pos');
  check(Math.abs(mPos2 - fPos2) < 4, `the room re-converged after the resume (${mPos2} vs ${fPos2})`);

  mIpc.close(); fIpc.close();

  // ---- m2-501 pin: sleep/resume WITH a reconnect must not pause the room ----
  // drop mode DESTROYS the follower's SSE (a real RST — unlike silence mode,
  // the sockets die), so its client must reconnect when the knob releases:
  // hello re-anchors it with a seek, the cache flushes, and the OLD engine
  // turned exactly that into a mechanical room pause (resumeTil wiped by
  // hello). Until worker2's engine fix lands this check is EXPECTED RED —
  // the failure is the pause line itself ("> pause (stall)").
  proxy.dropEvents(true); // the follower's link dies (sleep simulation)
  await sleep(4000); // the master plays on; the reconnect attempts all die
  const mNow = await new MpvIpc(await mpvSocketPath(master)).connect();
  await mNow.cmd('seek', 40, 'relative'); // the film moves on while "away"
  await sleep(2000);
  mNow.close();
  proxy.dropEvents(false); // the network is back: reconnect + hello + seek
  await sleep(12000); // the settle window elapses

  const pausedAfterResume =
    linesMatching(master, /> pause \(stall\)/).length +
    linesMatching(follower, /> pause \(stall\)/).length;
  check(pausedAfterResume === 0,
    `a reconnect-resume must not mechanically pause the room (stall pauses: ${pausedAfterResume})`);

  const mIpc2 = await new MpvIpc(await mpvSocketPath(master)).connect();
  const fIpc2 = await new MpvIpc(await mpvSocketPath(follower)).connect();
  const mPos3 = await mIpc2.get('time-pos');
  const fPos3 = await fIpc2.get('time-pos');
  check(Math.abs(mPos3 - fPos3) < 4,
    `the room re-converged after the reconnect-resume (${mPos3} vs ${fPos3})`);
  mIpc2.close(); fIpc2.close();
} catch (e) {
  failed++;
  console.error(`FAIL: ${e.stack || e}`);
} finally {
  // F6/C1: kill -> WAIT for the exit -> rm (3x retry). The server writes its
  // room state into dir when it exits: removing the dir first raced that
  // write (ENOTEMPTY) and lost it.
  if (server) await stopServer(server, dir);
  else rmWithRetry(dir);
  cleanup();
}

if (failed) { console.error(`stall_family: ${failed} failing check(s)`); process.exit(1); }
console.log('PASS: the stall family held — a silent master never pauses the room');
process.exit(0);
