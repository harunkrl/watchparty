// Phase A integration test: real mpv end-to-end (master only).
// Requires mpv + ffmpeg in PATH; SKIPs (exit 0) when they are missing.
// Run: nix-shell -p mpv ffmpeg --run "node tests/integration.mjs"

import { strict as assert } from 'node:assert';
import { execSync, spawn } from 'node:child_process';
import { existsSync, mkdtempSync, rmSync } from 'node:fs';
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
  console.log('      try: nix-shell -p mpv ffmpeg --run "node tests/integration.mjs"');
  process.exit(0);
}
const GO_BIN = join(ROOT, 'client-go/watchparty');
if (!existsSync(GO_BIN)) {
  console.error(`missing ${GO_BIN} — build it first: cd client-go && go build`);
  process.exit(2);
}

const PORT = 18000 + Math.floor(Math.random() * 1000);
const BASE = `http://127.0.0.1:${PORT}`;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const dir = mkdtempSync(join(tmpdir(), 'wp-int-'));
execSync(
  'ffmpeg -y -loglevel error -f lavfi -i testsrc2=duration=12:size=320x240:rate=10 ' +
    '-f lavfi -i sine=frequency=440:duration=12 -c:v libx264 -preset ultrafast -c:a aac -shortest ' +
    join(dir, 'test.mp4')
);

const server = spawn(process.execPath, [join(ROOT, 'server.js')], {
  env: { ...process.env, PORT: String(PORT), VIDEOS_DIR: dir },
  stdio: ['ignore', 'pipe', 'pipe'],
});
const client = spawn(GO_BIN, ['--headless', '--master', '--name', 'tester', BASE], {
  env: { ...process.env, WATCHPARTY_MPV: '--vo=null --ao=null --no-config --force-window=no' },
  stdio: ['pipe', 'pipe', 'pipe'],
});

let out = '';
client.stdout.on('data', (c) => {
  out += c;
});
client.stderr.on('data', (c) => {
  out += c;
});

const events = [];
const ctrl = new AbortController();
(async () => {
  const res = await fetch(`${BASE}/events?id=test-run`, { signal: ctrl.signal });
  const reader = res.body.getReader();
  const dec = new TextDecoder();
  let buf = '';
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += dec.decode(value, { stream: true });
    while (true) {
      const i = buf.indexOf('\n\n');
      if (i < 0) break;
      const chunk = buf.slice(0, i);
      buf = buf.slice(i + 2);
      if (chunk.startsWith('data: ')) events.push(JSON.parse(chunk.slice(6)));
    }
  }
})().catch(() => {});

function waitUntil(fn, ms = 20000) {
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
      if (Date.now() - t0 > ms) return (clearInterval(iv), reject(new Error('timeout')));
    }, 25);
  });
}

// the client's own mpv child (hermetic: ignores other watchparty mpvs on the machine)
function mpvChildPid(pid) {
  return Number(execSync(`pgrep -P ${pid}`).toString().trim());
}
function alive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

const cleanup = (code) => {
  ctrl.abort();
  client.kill();
  server.kill();
  rmSync(dir, { recursive: true, force: true });
  process.exit(code);
};
server.on('exit', (code) => {
  if (code && !process.exitCode) {
    console.error(`server exited with ${code}`);
    process.exit(1);
  }
});

try {
  // server up
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

  // client is up and its mpv exists
  await waitUntil(() => out.includes('commands:'));
  const mpvPid = mpvChildPid(client.pid);
  assert.ok(alive(mpvPid), 'expected a live mpv process');

  // play: load relayed, position heartbeats flow and advance
  client.stdin.write('play test.mp4\n');
  await waitUntil(() => events.some((e) => e.type === 'load' && e.name === 'test.mp4'));
  await waitUntil(() => {
    const ps = events.filter((e) => e.type === 'position');
    return ps.length >= 2 && ps[ps.length - 1].t > ps[0].t;
  });
  assert.ok(out.includes('> playing test.mp4'), `expected play log, got: ${out}`);

  // chat round-trips through the server
  client.stdin.write('chat selam\n');
  await waitUntil(() => events.some((e) => e.type === 'chat' && e.nick === 'tester'));

  // quit reaps mpv (hardened shutdown path)
  client.stdin.write('quit\n');
  await waitUntil(() => client.exitCode !== null, 10000);
  await sleep(500);
  assert.ok(!alive(mpvPid), 'mpv should be gone after quit');
  assert.ok(!out.includes('panic:'), `client crashed:\n${out}`);

  console.log('PASS: integration with real mpv held');
  cleanup(0);
} catch (e) {
  console.error('FAIL:', e.message, '\n--- client output ---\n' + out);
  cleanup(1);
}
