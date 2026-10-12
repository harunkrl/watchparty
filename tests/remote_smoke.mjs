// Remote end-to-end smoke: run a headless master client against an ALREADY
// RUNNING server (any network), play one file, and verify via the SSE stream
// that the room sees the load event and advancing position heartbeats.
// Usage: nix-shell -p mpv --run "node tests/remote_smoke.mjs http://pve:8090 vid.mp4"

import { strict as assert } from 'node:assert';
import { spawn } from 'node:child_process';
import { existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const BASE = process.argv[2] || 'http://pve:8090';
const FILE = process.argv[3] || 'vid.mp4';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const GO_BIN = join(ROOT, 'client-go/watchparty');
if (!existsSync(GO_BIN)) {
  console.error(`missing ${GO_BIN} — build it first: cd client-go && go build`);
  process.exit(2);
}

async function* sse() {
  const res = await fetch(`${BASE}/events?id=wansmoke-obs&nick=obs`);
  assert.ok(res.ok, `SSE connect failed: ${res.status}`);
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
      if (line) yield JSON.parse(line.slice(6));
    }
  }
}

const client = spawn(GO_BIN, ['--headless', '--master', '--name', 'wansmoke', BASE], {
  env: { ...process.env, WATCHPARTY_MPV: '--vo=null --ao=null --no-config --force-window=no' },
  stdio: ['pipe', 'pipe', 'inherit'],
});
let out = '';
client.stdout.on('data', (d) => (out += d));
const done = (code, msg) => {
  client.kill();
  console.log(msg);
  process.exit(code);
};

try {
  await new Promise((r) => (client.stdout.on('data', r), setTimeout(r, 1500)));
  const t0 = Date.now();
  client.stdin.write(`play ${FILE}\n`);

  let sawLoad = false;
  let positions = [];
  for await (const ev of sse()) {
    if (ev.type === 'load' && ev.name === FILE) sawLoad = true;
    if (ev.type === 'position' && ev.from && ev.from.startsWith('wansmoke')) {
      positions.push(ev.t);
      if (positions.length >= 3) break;
    }
    if (Date.now() - t0 > 30000) throw new Error('timeout waiting for events');
  }
  assert.ok(sawLoad, 'load event never relayed');
  assert.ok(positions[2] > positions[0], `positions not advancing: ${positions}`);

  const seekLatency = ((Date.now() - t0) / 1000).toFixed(1);
  client.stdin.write('quit\n');
  console.log(`PASS: WAN round-trip held (play -> load -> 3 advancing heartbeats in ${seekLatency}s from ${BASE})`);
  setTimeout(() => process.exit(0), 2000);
} catch (e) {
  done(1, `FAIL: ${e.message}\n--- client out ---\n${out}`);
}
