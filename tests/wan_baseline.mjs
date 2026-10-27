// WAN baseline / verification harness for the watchparty sync engine.
//
// Runs the real server.js and the real client binary through a bandwidth-
// capping TCP proxy and measures, over mpv's own IPC socket:
//   F2  follower behaviour when the link is slower than the bitrate
//       (hard seeks, position jumps, stall seconds, room pause events)
//   F3  behaviour when the SSE connection drops (network blip)
//       (does the client reload the file? does the film jump?)
//
// It never touches product code. Usage:
//   node tests/wan_baseline.mjs [--bin client-go/watchparty] [--server server.js]
//                               [--mbit 3] [--seconds 60] [--video-mbit 15]
//                               [--phase f2|f3|both] [--out raw.json]
//
// Needs: node, ffmpeg, and the built client binary.

import { execFileSync, spawn } from 'node:child_process';
import { createConnection } from 'node:net';
import { existsSync, copyFileSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { Proxy } from './wanlink.mjs';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const argv = process.argv.slice(2);
const arg = (n, d) => { const i = argv.indexOf(n); return i >= 0 && argv[i + 1] ? argv[i + 1] : d; };
const OPT = {
  bin: arg('--bin', join(ROOT, 'client-go/watchparty')),
  server: arg('--server', join(ROOT, 'server.js')),
  mbit: Number(arg('--mbit', 3)),
  seconds: Number(arg('--seconds', 60)),
  videoMbit: Number(arg('--video-mbit', 15)),
  phase: arg('--phase', 'both'),
  out: arg('--out', ''),
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const children = [];
const cleanup = () => { for (const c of children) { try { c.kill('SIGKILL'); } catch {} } };
process.on('exit', cleanup);
process.on('SIGINT', () => { cleanup(); process.exit(130); });
setTimeout(() => { cleanup(); process.exit(9); }, (OPT.seconds + 180) * 1000).unref();

// ---------------------------------------------------------------- mpv IPC
class MpvIpc {
  constructor(sockPath) { this.sockPath = sockPath; this.events = []; this.seq = 1; this.pending = new Map(); this.buf = ''; }
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
          m.error === 'success' ? p.res(m.data) : p.res(undefined);
        } else if (m.event) this.events.push({ ...m, at: Date.now() });
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
  observe(prop) { return this.cmd('observe_property', 900, prop); }
  count(event) { return this.events.filter((e) => e.event === event).length; }
  close() { try { this.conn.destroy(); } catch {} }
}

// ---------------------------------------------------------------- helpers
function mpvSocketPath(client) {
  for (let i = 0; i < 150; i++) {
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

function startClient(name, master, base) {
  const p = spawn(OPT.bin, ['--headless', ...(master ? ['--master'] : []), '--name', name, base], {
    env: { ...process.env, WATCHPARTY_MPV: '--vo=null --ao=null --no-config --force-window=no' },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  children.push(p);
  p.name = name; p.lines = []; p.out = '';
  const push = (d) => {
    p.out += d;
    for (const l of d.split('\n')) if (l.trim()) p.lines.push({ t: Date.now(), line: l });
  };
  p.stdout.on('data', (d) => push(d.toString()));
  p.stderr.on('data', (d) => push(d.toString()));
  return p;
}

function startServer(port, videos, stateFile) {
  const env = { ...process.env, PORT: String(port), VIDEOS_DIR: videos };
  if (stateFile) env.STATE_FILE = stateFile; // R4: unique state file per run (tmp)
  const s = spawn(process.execPath, [OPT.server], { env, stdio: ['ignore', 'pipe', 'pipe'] });
  children.push(s);
  s.out = '';
  s.stdout.on('data', (d) => { s.out += d; });
  s.stderr.on('data', (d) => { s.out += d; });
  s.exited = false;
  s.on('exit', (code, sig) => { s.exited = true; s.out += `\n[server exited code=${code} signal=${sig}]\n`; });
  return s;
}
async function waitServer(port) {
  for (let i = 0; i < 100; i++) {
    try { const r = await fetch(`http://127.0.0.1:${port}/files`); if (r.ok) return true; } catch {}
    await sleep(100);
  }
  return false;
}

function makeVideo(dir, mbit, seconds) {
  const file = join(dir, `hi${mbit}.mp4`);
  if (existsSync(file)) return file;
  execFileSync('ffmpeg', [
    '-y', '-loglevel', 'error',
    '-f', 'lavfi', '-i', `testsrc2=duration=${seconds}:size=1280x720:rate=30`,
    '-f', 'lavfi', '-i', `sine=frequency=440:duration=${seconds}`,
    '-c:v', 'libx264', '-preset', 'ultrafast', '-b:v', `${mbit}M`, '-maxrate', `${mbit}M`, '-bufsize', `${mbit * 2}M`,
    '-movflags', '+faststart', '-c:a', 'aac', '-shortest', file,
  ], { stdio: 'ignore' });
  return file;
}

const linesMatching = (client, re) => client.lines.filter((l) => re.test(l.line));

// ---------------------------------------------------------------- F2
async function phaseF2(ctx) {
  const server = startServer(ctx.port, ctx.dir, ctx.stateFile);
  await waitServer(ctx.port);
  const proxyF = new Proxy(ctx.proxyFPort, ctx.port, ctx.cap, 'F');   // throttled follower link
  const proxyM = new Proxy(ctx.proxyMPort, ctx.port, 0, 'M');         // master: local link
  await proxyF.listen(); await proxyM.listen();
  const master = startClient('M', true, `http://127.0.0.1:${ctx.proxyMPort}`);
  const follower = startClient('F', false, `http://127.0.0.1:${ctx.proxyFPort}`);
  await sleep(2500);
  master.stdin.write(`play ${ctx.videoName}\n`);
  await sleep(8000);

  const mIpc = await new MpvIpc(await mpvSocketPath(master)).connect();
  const fIpc = await new MpvIpc(await mpvSocketPath(follower)).connect();
  await mIpc.observe('pause'); await fIpc.observe('pause'); await fIpc.observe('speed');
  await fIpc.observe('paused-for-cache'); // mpv's real name (the fix reads pause-for-cache)

  const samples = [];
  const t0 = Date.now();
  let playPostedAt = null;
  const windowEnd = t0 + ctx.seconds * 1000;
  while (Date.now() < windowEnd) {
    const [mp, fp, fpaused, mpaused, fcache] = await Promise.all([
      mIpc.get('time-pos'), fIpc.get('time-pos'), fIpc.get('pause'), mIpc.get('pause'),
      fIpc.get('paused-for-cache'),
    ]);
    samples.push({ t: (Date.now() - t0) / 1000, mp, fp, fpaused, mpaused, fcache });
    // play test (part c): unpause the room and measure how long until the
    // next stall-pause arrives (must not be immediate)
    if (playPostedAt === null && Date.now() - t0 > ctx.seconds * 1000 * 0.6) {
      playPostedAt = Date.now();
      await fetch(`http://127.0.0.1:${ctx.port}/cmd`, {
        method: 'POST', headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ type: 'play', from: 'harness' }),
      });
    }
    await sleep(250);
  }
  // let the room settle after the play test
  await sleep(6000);

  const res = analyzeF2({ samples, mIpc, fIpc, master, follower, playPostedAt, t0, ctx });
  res.proxyLog = proxyF.log;
  res.serverLog = server.out;
  res.serverExited = server.exited;
  for (const c of children) { try { c.kill('SIGKILL'); } catch {} }
  proxyF.close(); proxyM.close(); mIpc.close(); fIpc.close();
  return res;
}

function analyzeF2({ samples, mIpc, fIpc, master, follower, playPostedAt, t0, ctx }) {
  let jumps = 0, stallSeconds = 0, maxDrift = 0, endDrift = null, maxJump = 0;
  for (let i = 1; i < samples.length; i++) {
    const a = samples[i - 1], b = samples[i];
    if (a.mp == null || b.mp == null || a.fp == null || b.fp == null) continue;
    const dt = b.t - a.t, dPos = b.fp - a.fp, drift = b.mp - b.fp;
    maxDrift = Math.max(maxDrift, Math.abs(drift));
    endDrift = drift;
    if (Math.abs(dPos - dt) > 0.75) { jumps++; maxJump = Math.max(maxJump, Math.abs(dPos - dt)); }
    if (dPos < dt * 0.5) stallSeconds += dt;
  }
  let cacheStallSeconds = 0;
  let masterPausedSamples = 0, followerPausedSamples = 0, firstMasterPauseAt = null;
  let followerIdleAt = null, masterIdleAt = null, bothIdleAt = null;
  for (const s2 of samples) {
    if (s2.mpaused === true) { masterPausedSamples++; if (firstMasterPauseAt === null) firstMasterPauseAt = s2.t; }
    if (s2.fpaused === true) followerPausedSamples++;
    if (s2.fp == null && followerIdleAt === null) followerIdleAt = s2.t;
    if (s2.mp == null && masterIdleAt === null) masterIdleAt = s2.t;
    if (s2.fp == null && s2.mp == null && bothIdleAt === null) bothIdleAt = s2.t;
  }
  for (let i = 1; i < samples.length; i++) {
    if (samples[i].fcache === true) cacheStallSeconds += samples[i].t - samples[i - 1].t;
  }
  const pauseLines = linesMatching(master, /^> pause/).map((l) => l.t);
  const playLines = linesMatching(master, /^> play/).map((l) => l.t);
  const afterPlay = playPostedAt ? pauseLines.filter((t) => t > playPostedAt) : [];
  return {
    windowSeconds: +(samples[samples.length - 1].t).toFixed(1),
    samples: samples.length,
    followerSeeks: fIpc.count('seek'),            // mpv-level seeks on the follower
    masterSeeks: mIpc.count('seek'),
    cacheStallSeconds: +cacheStallSeconds.toFixed(1),
    pausedForCacheEvents: fIpc.events.filter((e) => e.event === 'property-change' && e.name === 'paused-for-cache' && e.data === true).length,
    followerSpeedNudges: fIpc.events.filter((e) => e.event === 'property-change' && e.name === 'speed').length,
    positionJumps: jumps,
    maxJumpSec: +maxJump.toFixed(2),
    stallSeconds: +stallSeconds.toFixed(1),
    maxDriftSec: +maxDrift.toFixed(2),
    endDriftSec: endDrift == null ? null : +endDrift.toFixed(2),
    masterPausedSamples,
    followerPausedSamples,
    followerIdleAtSec: followerIdleAt,
    masterIdleAtSec: masterIdleAt,
    bothIdleAtSec: bothIdleAt,
    serverAbortedLines: (ctx.serverLog || '').split('\n').filter((l) => /aborted|stream error/.test(l)),
    firstMasterPauseAtSec: firstMasterPauseAt === null ? null : +firstMasterPauseAt.toFixed(1),
    roomPauseEvents: pauseLines.length,
    roomPlayEvents: playLines.length,
    roomPauseTimes: pauseLines.map((t) => +((t - t0) / 1000).toFixed(1)),
    roomPlayTimes: playLines.map((t) => +((t - t0) / 1000).toFixed(1)),
    playTestPostedAt: playPostedAt ? +((playPostedAt - t0) / 1000).toFixed(1) : null,
    rePauseAfterPlaySec: afterPlay.length ? +((afterPlay[0] - playPostedAt) / 1000).toFixed(1) : null,
    followerLogSeekLines: linesMatching(follower, /^> seek/).length,
    followerLogPlayingLines: linesMatching(follower, /^> playing/).length,
    masterLogPlayingLines: linesMatching(master, /^> playing/).length,
    followerOut: follower.out,
    masterOut: master.out,
    sampleRows: samples.map((x) => [x.t, x.mp, x.fp, x.fcache, x.fpaused]),
  };
}

// ---------------------------------------------------------------- F3
async function phaseF3(ctx) {
  const server = startServer(ctx.port, ctx.dir, ctx.stateFile);
  await waitServer(ctx.port);
  const proxyF = new Proxy(ctx.proxyFPort, ctx.port, 0, 'F');
  const proxyM = new Proxy(ctx.proxyMPort, ctx.port, 0, 'M');
  await proxyF.listen(); await proxyM.listen();
  const master = startClient('M', true, `http://127.0.0.1:${ctx.proxyMPort}`);
  const follower = startClient('F', false, `http://127.0.0.1:${ctx.proxyFPort}`);
  await sleep(2500);
  master.stdin.write(`play ${ctx.videoName}\n`);
  await sleep(12000);

  const mIpc = await new MpvIpc(await mpvSocketPath(master)).connect();
  const fIpc = await new MpvIpc(await mpvSocketPath(follower)).connect();
  const before = {
    mp: await mIpc.get('time-pos'), fp: await fIpc.get('time-pos'),
    masterPlaying: linesMatching(master, /^> playing/).length,
    followerPlaying: linesMatching(follower, /^> playing/).length,
    masterSeeks: mIpc.count('seek'), followerSeeks: fIpc.count('seek'),
  };
  const tKill = Date.now();
  const killed = { follower: proxyF.killEvents(), master: proxyM.killEvents() };
  await sleep(20000);
  const after = {
    mp: await mIpc.get('time-pos'), fp: await fIpc.get('time-pos'),
    masterPlaying: linesMatching(master, /^> playing/).length,
    followerPlaying: linesMatching(follower, /^> playing/).length,
    masterSeeks: mIpc.count('seek'), followerSeeks: fIpc.count('seek'),
  };
  // reconnect delay: first "> room:" line after the blip
  const helloAfter = (client) => {
    const l = client.lines.find((x) => x.t > tKill && /^> room:/.test(x.line));
    return l ? +((l.t - tKill) / 1000).toFixed(1) : null;
  };
  const res = {
    killedSse: killed,
    blipAtSec: 0,
    reconnectDelaySec: { master: helloAfter(master), follower: helloAfter(follower) },
    before, after,
    masterReloaded: after.masterPlaying > before.masterPlaying,
    followerReloaded: after.followerPlaying > before.followerPlaying,
    masterConnLost: /\[conn\] lost/.test(master.out),
    followerConnLost: /\[conn\] lost/.test(follower.out),
    masterOut: master.out,
    followerOut: follower.out,
    serverLog: server.out,
    serverExited: server.exited,
  };
  for (const c of children) { try { c.kill('SIGKILL'); } catch {} }
  proxyF.close(); proxyM.close(); mIpc.close(); fIpc.close();
  return res;
}

// ---------------------------------------------------------------- R4 (server crash + state restore)
async function phaseR4(ctx) {
  let server = startServer(ctx.port, ctx.dir, ctx.stateFile);
  await waitServer(ctx.port);
  const proxyF = new Proxy(ctx.proxyFPort, ctx.port, 0, 'F');
  const proxyM = new Proxy(ctx.proxyMPort, ctx.port, 0, 'M');
  await proxyF.listen(); await proxyM.listen();
  const master = startClient('M', true, `http://127.0.0.1:${ctx.proxyMPort}`);
  const follower = startClient('F', false, `http://127.0.0.1:${ctx.proxyFPort}`);
  await sleep(2500);
  master.stdin.write(`play ${ctx.videoName}\n`);
  await sleep(12000);
  const mIpc = await new MpvIpc(await mpvSocketPath(master)).connect();
  const fIpc = await new MpvIpc(await mpvSocketPath(follower)).connect();
  const snap = async () => ({
    mp: await mIpc.get('time-pos'), fp: await fIpc.get('time-pos'),
    midle: await mIpc.get('idle-active'), fidle: await fIpc.get('idle-active'),
    masterPlaying: linesMatching(master, /^> playing/).length,
    followerPlaying: linesMatching(follower, /^> playing/).length,
    masterSeeks: mIpc.count('seek'), followerSeeks: fIpc.count('seek'),
  });
  const before = await snap();

  // queue survival: put a second file in the queue before the crash
  const second = 'queued.mp4';
  try { copyFileSync(join(ctx.dir, ctx.videoName), join(ctx.dir, second)); } catch {}
  await fetch(`http://127.0.0.1:${ctx.port}/cmd`, {
    method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ type: 'enqueue', name: second, from: 'harness' }),
  });
  await sleep(1500);
  const queueBefore = linesMatching(master, /^> queue:/).map((l) => l.line).slice(-1)[0] || null;

  // crash the server (SIGKILL = no clean shutdown, no flush)
  const tKill = Date.now();
  server.kill('SIGKILL');
  await sleep(2000);
  const duringOutage = await snap();
  // restart with the SAME state file
  server = startServer(ctx.port, ctx.dir, ctx.stateFile);
  const up = await waitServer(ctx.port);
  await sleep(20000); // clients reconnect with backoff
  const after = await snap();

  // a third, fresh client joins after the restart
  const third = startClient('C', false, `http://127.0.0.1:${ctx.proxyFPort}`);
  await sleep(12000);
  const cIpc = await new MpvIpc(await mpvSocketPath(third)).connect();
  const cPos = await cIpc.get('time-pos');
  const cIdle = await cIpc.get('idle-active');
  const helloAfter = (client) => {
    const l = client.lines.find((x) => x.t > tKill && /^> room:/.test(x.line));
    return l ? { atSec: +((l.t - tKill) / 1000).toFixed(1), line: l.line } : null;
  };
  const res = {
    serverRestarted: up,
    stateFile: ctx.stateFile,
    restoredLog: /restored room state/.test(server.out),
    serverLog: server.out,
    before, duringOutage, after,
    reconnectDelaySec: { master: helloAfter(master), follower: helloAfter(follower) },
    queueBefore,
    queueAfter: helloAfter(master) ? helloAfter(master).line : null,
    masterReloaded: after.masterPlaying > before.masterPlaying,
    followerReloaded: after.followerPlaying > before.followerPlaying,
    thirdClient: {
      hello: linesMatching(third, /^> room:/).map((l) => l.line),
      pos: cPos, idle: cIdle,
      playing: linesMatching(third, /^> playing/).length,
      reloads: linesMatching(third, /^> playing/).length - 1,
      out: third.out,
    },
    masterOut: master.out,
    followerOut: follower.out,
  };
  for (const c of children) { try { c.kill('SIGKILL'); } catch {} }
  proxyF.close(); proxyM.close(); mIpc.close(); fIpc.close(); cIpc.close();
  return res;
}

// ---------------------------------------------------------------- main
(async () => {
  if (!existsSync(OPT.bin)) { console.error(`missing client binary ${OPT.bin}`); process.exit(2); }
  const dir = mkdtempSync(join(tmpdir(), 'wp-wan-'));
  const port = 19000 + Math.floor(Math.random() * 500);
  const videoName = `hi${OPT.videoMbit}.mp4`;
  const vdur = OPT.seconds + 20;
  makeVideo(dir, OPT.videoMbit, vdur);
  const vsize = Number(execFileSync('sh', ['-c', `stat -c %s ${join(dir, videoName)}`], { encoding: 'utf8' }).trim());
  const ctx = {
    dir, port, proxyFPort: port + 1000, proxyMPort: port + 2000, videoName,
    cap: Math.round((OPT.mbit * 1e6) / 8), seconds: OPT.seconds,
    stateFile: join(dir, 'state.json'), // R4: unique state file for this run
  };
  const out = {
    meta: {
      bin: OPT.bin, server: OPT.server, videoMbit: +(vsize * 8 / vdur / 1e6).toFixed(2),
      linkCapMbit: OPT.mbit, windowSeconds: OPT.seconds, at: new Date().toISOString(),
      node: process.version,
    },
  };
  if (OPT.phase === 'both' || OPT.phase === 'f2') {
    console.error('# F2: throttled follower link');
    out.f2 = await phaseF2(ctx);
  }
  if (OPT.phase === 'both' || OPT.phase === 'f3') {
    console.error('# F3: SSE blip');
    out.f3 = await phaseF3(ctx);
  }
  if (OPT.phase === 'both' || OPT.phase === 'r4') {
    console.error('# R4: server crash + state restore');
    out.r4 = await phaseR4(ctx);
  }
  const slim = JSON.parse(JSON.stringify(out));
  if (slim.f2) { delete slim.f2.followerOut; delete slim.f2.masterOut; }
  if (slim.f3) { delete slim.f3.masterOut; delete slim.f3.followerOut; }
  console.log(JSON.stringify(slim, null, 2));
  if (OPT.out) writeFileSync(OPT.out, JSON.stringify(out, null, 2));
  rmSync(dir, { recursive: true, force: true });
  cleanup();
  process.exit(0);
})();
