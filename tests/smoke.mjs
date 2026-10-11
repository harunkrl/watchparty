// Smoke test: self-contained. Spawns server.js with a temp videos folder,
// then exercises every endpoint and the relay logic (chat/load/queue/ended,
// sender exclusion, range requests, traversal rejection).
// Usage: node tests/smoke.mjs

import { strict as assert } from 'node:assert';
import { spawn } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const PORT = 18000 + Math.floor(Math.random() * 1000);
const BASE = `http://127.0.0.1:${PORT}`;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

const dir = mkdtempSync(join(tmpdir(), 'wp-test-'));
writeFileSync(join(dir, 'movie one.mkv'), Buffer.alloc(10000, 7));
writeFileSync(join(dir, 'movie one.srt'), '1\n00:00:00,000 --> 00:00:05,000\nhi\n');
writeFileSync(join(dir, 'movie two.mkv'), Buffer.alloc(8000, 9));

const server = spawn(process.execPath, [join(ROOT, 'server.js')], {
  env: { ...process.env, PORT: String(PORT), VIDEOS_DIR: dir },
  stdio: ['ignore', 'pipe', 'pipe'],
});

// Minimal SSE collector
function sse(id, nick) {
  const events = [];
  const ctrl = new AbortController();
  (async () => {
    const res = await fetch(`${BASE}/events?id=${id}&nick=${encodeURIComponent(nick)}`, { signal: ctrl.signal });
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
  return { events, close: () => ctrl.abort() };
}

async function post(obj) {
  const res = await fetch(`${BASE}/cmd`, { method: 'POST', body: JSON.stringify(obj) });
  assert.equal(res.status, 204);
}

function waitUntil(fn, ms = 3000) {
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

const cleanup = (code) => {
  server.kill();
  rmSync(dir, { recursive: true, force: true });
  process.exit(code);
};
server.on('exit', (code) => {
  rmSync(dir, { recursive: true, force: true });
  if (code && code !== 0 && !process.exitCode) {
    console.error(`server exited with ${code}`);
    process.exit(1);
  }
});

try {
  // wait for the server to accept connections
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

  const a = sse('alice', 'Alice'); // receiver
  const b = sse('bob', 'Bob'); // sender
  await sleep(200);
  assert.equal(a.events[0].type, 'hello');
  await waitUntil(() => a.events.some((e) => e.type === 'join' && e.nick === 'Bob'));

  // listing + subtitle sidecar
  const files = await (await fetch(`${BASE}/files`)).json();
  assert.deepEqual(files, ['movie one.mkv', 'movie two.mkv']);
  const sub = await fetch(`${BASE}/sub/${encodeURIComponent('movie one.srt')}`);
  assert.equal(sub.status, 200);

  // full + range video serving, original byte count
  const name = encodeURIComponent('movie one.mkv');
  const full = await fetch(`${BASE}/v/${name}`);
  assert.equal(full.status, 200);
  assert.equal(full.headers.get('accept-ranges'), 'bytes');
  assert.equal((await full.arrayBuffer()).byteLength, 10000);
  const part = await fetch(`${BASE}/v/${name}`, { headers: { range: 'bytes=0-99' } });
  assert.equal(part.status, 206);
  assert.equal(part.headers.get('content-length'), '100');
  assert.equal(part.headers.get('content-range'), 'bytes 0-99/10000');
  assert.equal((await part.arrayBuffer()).byteLength, 100);

  // path traversal is rejected
  const evil = await fetch(`${BASE}/v/${encodeURIComponent('../../etc/passwd')}`);
  assert.equal(evil.status, 404);

  // chat relay: bob speaks, alice hears, bob does not hear himself
  await post({ type: 'chat', from: 'bob', nick: 'bob', text: 'selam' });
  await waitUntil(() => a.events.some((e) => e.type === 'chat'));
  assert.equal(b.events.filter((e) => e.type === 'chat').length, 0);

  // load + queue + auto-advance (empty queue => no second load)
  await post({ type: 'enqueue', from: 'bob', name: 'movie one.mkv' });
  await waitUntil(() => a.events.some((e) => e.type === 'queue'));
  await post({ type: 'load', from: 'bob', name: 'movie one.mkv' });
  await waitUntil(() => a.events.some((e) => e.type === 'load'));
  await post({ type: 'ended', from: 'bob' });
  await sleep(300);
  assert.equal(a.events.filter((e) => e.type === 'load').length, 1);

  // skip advances the queue for everyone, including the requester
  await post({ type: 'enqueue', from: 'bob', name: 'movie two.mkv' });
  await waitUntil(() => a.events.some((e) => e.type === 'queue' && e.queue.length === 1));
  await post({ type: 'skip', from: 'bob' });
  await waitUntil(() => a.events.some((e) => e.type === 'load' && e.name === 'movie two.mkv'));
  assert.ok(b.events.some((e) => e.type === 'load' && e.name === 'movie two.mkv'));

  // sync events pass through
  for (const ev of [{ type: 'pause' }, { type: 'play' }, { type: 'seek', t: 42 }, { type: 'position', t: 43 }]) {
    await post({ ...ev, from: 'bob' });
  }
  await waitUntil(() => a.events.some((e) => e.type === 'position'));
  for (const t of ['pause', 'play', 'seek', 'position']) {
    assert.ok(a.events.some((e) => e.type === t), `missing ${t}`);
  }

  b.close();
  await waitUntil(() => a.events.some((e) => e.type === 'bye' && e.nick === 'Bob'));
  a.close();
  console.log('PASS: all smoke assertions held');
  cleanup(0);
} catch (e) {
  console.error('FAIL:', e.message);
  cleanup(1);
}
