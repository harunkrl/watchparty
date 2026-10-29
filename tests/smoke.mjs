// Smoke test: self-contained. Spawns server.js with a temp videos folder,
// then exercises every endpoint and the relay logic (chat/load/queue/ended,
// sender exclusion, range requests, traversal rejection).
// Usage: node tests/smoke.mjs

import { strict as assert } from 'node:assert';
import { spawn } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync, symlinkSync } from 'node:fs';
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
  env: { ...process.env, PORT: String(PORT), VIDEOS_DIR: dir, STATE_FILE: join(dir, 'state.json') },
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
  // S-A: hello carries a dialable invite URL — the server is the only party
  // that knows its reachable address (the client may be off-host)
  assert.match(a.events[0].invite, /^http:\/\/\d+\.\d+\.\d+\.\d+:\d+$/, `invite: ${a.events[0].invite}`);
  await waitUntil(() => a.events.some((e) => e.type === 'join' && e.nick === 'Bob'));

  // S5: health endpoint for watchdog/manual liveness checks
  const health = await (await fetch(`${BASE}/health`)).json();
  assert.equal(health.now, null);
  assert.equal(health.queueLen, 0);
  assert.ok(health.listeners >= 2, `listeners: ${health.listeners}`);

  // S2: idle skip/ended is still a no-op but explains itself
  await post({ type: 'skip', from: 'bob' });
  await waitUntil(() => a.events.some((e) => e.type === 'notice' && e.text.includes('nothing playing')));

  // listing + subtitle sidecar
  const files = await (await fetch(`${BASE}/files`)).json();
  assert.deepEqual(files, ['movie one.mkv', 'movie two.mkv']);
  const sub = await fetch(`${BASE}/sub/${encodeURIComponent('movie one.srt')}`);
  assert.equal(sub.status, 200);

  // R8: subtitle siblings are listed for the client's track picker
  writeFileSync(join(dir, 'movie one.tr.srt'), '1\n00:00:00,000 --> 00:00:05,000\nmerhaba\n');
  writeFileSync(join(dir, 'movie one.es.srt'), '1\n00:00:00,000 --> 00:00:05,000\nhola\n');
  const subs = await (await fetch(`${BASE}/subs`)).json();
  assert.deepEqual(subs, ['movie one.es.srt', 'movie one.srt', 'movie one.tr.srt']);

  // S1: natural sort — "movie2" before "movie10" (numeric collation)
  writeFileSync(join(dir, 'movie2.mkv'), Buffer.alloc(1000, 1));
  writeFileSync(join(dir, 'movie10.mkv'), Buffer.alloc(1000, 1));
  writeFileSync(join(dir, 'movie2.tr.srt'), '1\n00:00:00,000 --> 00:00:05,000\nmerhaba\n');
  writeFileSync(join(dir, 'sub2.srt'), '1\n00:00:00,000 --> 00:00:05,000\na\n');
  writeFileSync(join(dir, 'sub10.srt'), '1\n00:00:00,000 --> 00:00:05,000\nb\n');
  const naturalFiles = await (await fetch(`${BASE}/files`)).json();
  assert.ok(naturalFiles.indexOf('movie2.mkv') < naturalFiles.indexOf('movie10.mkv'), `natural /files order: ${naturalFiles}`);
  const naturalSubs = await (await fetch(`${BASE}/subs`)).json();
  assert.ok(naturalSubs.indexOf('sub2.srt') < naturalSubs.indexOf('sub10.srt'), `natural /subs order: ${naturalSubs}`);

  // S4: /library pairs each video with its subtitle siblings (client parity:
  // a sub belongs to a video when its name starts with <video stem + ".">)
  const library = await (await fetch(`${BASE}/library`)).json();
  assert.ok(Array.isArray(library) && library.length >= 4, `library: ${JSON.stringify(library)}`);
  assert.deepEqual(library.find((e) => e.name === 'movie one.mkv').subs,
    ['movie one.es.srt', 'movie one.srt', 'movie one.tr.srt']);
  assert.deepEqual(library.find((e) => e.name === 'movie2.mkv').subs, ['movie2.tr.srt']);
  assert.deepEqual(library.find((e) => e.name === 'movie10.mkv').subs, []);
  assert.deepEqual(library.find((e) => e.name === 'movie two.mkv').subs, []);

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

  // suffix ranges serve the last N bytes (RFC 9110)
  const last = await fetch(`${BASE}/v/${name}`, { headers: { range: 'bytes=-100' } });
  assert.equal(last.status, 206);
  assert.equal(last.headers.get('content-range'), 'bytes 9900-9999/10000');
  assert.equal(last.headers.get('content-length'), '100');
  const lastByte = await fetch(`${BASE}/v/${name}`, { headers: { range: 'bytes=-1' } });
  assert.equal(lastByte.status, 206);
  assert.equal(lastByte.headers.get('content-range'), 'bytes 9999-9999/10000');

  // malformed percent-encoding answers 400 and never kills the process
  for (const bad of ['%', '%fg', '%2']) {
    assert.equal((await fetch(`${BASE}/v/${bad}`)).status, 400);
  }
  assert.ok((await fetch(`${BASE}/files`)).ok, 'server alive after bad requests');

  // path traversal is rejected
  const evil = await fetch(`${BASE}/v/${encodeURIComponent('../../etc/passwd')}`);
  assert.equal(evil.status, 404, 'traversal must be rejected');

  // a symlink planted inside the videos dir cannot escape it
  symlinkSync('/etc/passwd', join(dir, 'evil.mp4'));
  const viaLink = await fetch(`${BASE}/v/evil.mp4`);
  assert.equal(viaLink.status, 404, 'symlink escape must be rejected');
  assert.ok(!(await viaLink.text()).includes('root:'), 'no passwd content leaks');

  // chat relay: bob speaks, alice hears, bob does not hear himself
  await post({ type: 'chat', from: 'bob', nick: 'bob', text: 'selam' });
  await waitUntil(() => a.events.some((e) => e.type === 'chat'));
  assert.equal(b.events.filter((e) => e.type === 'chat').length, 0);

  // nicks and chat text are sanitized at the server (control chars, length)
  const evilSse = sse('evil', 'a\x1b[2Jb\nc\x07'.repeat(40));
  await waitUntil(() => a.events.some((e) => e.type === 'join' && e.nick === 'a[2Jbc'.repeat(40).slice(0, 32)));
  await post({ type: 'chat', from: 'evil', nick: 'x\x1b[2Jy', text: 't\nu\x1b]0;evil' });
  await waitUntil(() => a.events.some((e) => e.type === 'chat' && e.nick === 'x[2Jy'));
  const gotChat = a.events.find((e) => e.type === 'chat' && e.nick === 'x[2Jy');
  assert.equal(gotChat.text, 'tu]0;evil');
  evilSse.close();

  // R9: chat history replays once to a NEW connection id (a late joiner
  // sees what they missed); a same-id reconnect must not see it again
  const lateHist = sse('latehist', 'LateHist');
  await waitUntil(() => lateHist.events.some((e) => e.type === 'chat'));
  const seen = lateHist.events.filter((e) => e.type === 'chat').map((e) => e.text);
  assert.deepEqual(seen, ['selam', 'tu]0;evil']);
  lateHist.close();
  await sleep(200);
  const lateHist2 = sse('latehist', 'LateHist');
  await sleep(300);
  assert.equal(lateHist2.events.filter((e) => e.type === 'chat').length, 0, 'no re-replay for the same id');
  lateHist2.close();

  // load + queue + auto-advance (empty queue => no second load)
  await post({ type: 'enqueue', from: 'bob', name: 'movie one.mkv' });
  await waitUntil(() => a.events.some((e) => e.type === 'queue'));
  assert.equal((await (await fetch(`${BASE}/health`)).json()).queueLen, 1, 'health queueLen tracks the queue');
  await post({ type: 'load', from: 'bob', name: 'movie one.mkv' });
  await waitUntil(() => a.events.some((e) => e.type === 'load'));

  // hello age-adjustment runs on monotonic time: a fresh joiner lands at
  // posted pos + actual elapsed (an NTP wall-clock jump cannot skew it)
  await post({ type: 'position', from: 'bob', t: 100, p: false });
  await sleep(600);
  const latecomer = sse('late2', 'late2');
  await waitUntil(() => latecomer.events.some((e) => e.type === 'hello' && e.pos >= 100.3 && e.pos <= 102));
  latecomer.close();
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
    assert.ok(a.events.some((ev) => ev.type === t), `missing ${t}`);
  }

  // S-B pin: play/pause/seek relays carry the actor's room nick (the client
  // renders "> pause by <nick>"); the server's roster wins over any
  // client-supplied nick, and the actor is echo-filtered
  await post({ type: 'pause', from: 'bob', nick: 'spoofed' });
  await waitUntil(() => a.events.some((e) => e.type === 'pause' && e.nick === 'Bob'));
  assert.ok(!b.events.some((e) => e.type === 'pause'), 'sender must not hear his own pause');

  // B3 pin: a poster with no roster entry (never opened SSE) must relay
  // with NO nick at all — a client-supplied fallback would fake "> pause by ada"
  await post({ type: 'pause', from: 'ghost', nick: 'spoofed' });
  await waitUntil(() => a.events.some((e) => e.type === 'pause' && e.from === 'ghost'));
  const ghost = a.events.find((e) => e.type === 'pause' && e.from === 'ghost');
  assert.ok(!('nick' in ghost), `ghost pause must carry no nick, got ${JSON.stringify(ghost)}`);

  // S6 pin: the server relays zero/false wire values verbatim (the C7
  // client fix relies on this) and the gates type-check junk away
  await post({ type: 'seek', from: 'bob', t: 0 });
  await post({ type: 'position', from: 'bob', t: 0, p: false });
  await post({ type: 'seek', from: 'bob' }); // no t: gate must drop
  await post({ type: 'position', from: 'bob', t: 'x' }); // string t: gate must drop
  await waitUntil(() => a.events.some((e) => e.type === 'position' && e.t === 0));
  await sleep(300);
  const zeroSeek = a.events.filter((e) => e.type === 'seek' && e.t === 0);
  assert.equal(zeroSeek.length, 1, 'seek t=0 relayed exactly once');
  assert.ok('t' in zeroSeek[0], 'seek t=0 must carry t on the wire');
  const falsePos = a.events.find((e) => e.type === 'position' && e.t === 0);
  assert.ok('p' in falsePos && falsePos.p === false, 'position p=false must carry p on the wire');
  assert.ok(!a.events.some((e) => e.type === 'seek' && !('t' in e)), 't-less seek must not relay');
  assert.ok(!a.events.some((e) => e.type === 'position' && typeof e.t === 'string'), 'string-t position must not relay');

  // hello carries the live room position (age-adjusted) for late joiners
  await post({ type: 'position', from: 'bob', t: 43.5, p: true }); // paused master
  const c = sse('carol', 'Carol');
  await waitUntil(() => c.events[0]);
  assert.equal(c.events[0].type, 'hello');
  assert.equal(c.events[0].p, true);
  assert.ok(Math.abs(c.events[0].pos - 43.5) < 0.5, `pos ${c.events[0].pos}`);
  c.close();

  await post({ type: 'play', from: 'bob' });
  const d = sse('dave', 'Dave');
  await waitUntil(() => d.events[0]);
  assert.equal(d.events[0].p, false);
  assert.ok(d.events[0].pos >= 43.5, 'pos age-adjusted while playing');
  d.close();

  // a fresh load resets the position: joiners are not dragged to stale offsets
  await post({ type: 'load', from: 'bob', name: 'movie one.mkv' });
  await waitUntil(() => a.events.filter((ev) => ev.type === 'load').length === 3);
  const er = sse('erin', 'Erin');
  await waitUntil(() => er.events[0]);
  assert.equal(er.events[0].pos, undefined);
  er.close();

  // S3: an optional dur on the heartbeat lets hello clamp long-downtime
  // joins before EOF (pos 100 in a 50s file lands at dur-1)
  await post({ type: 'position', from: 'bob', t: 100, p: true, dur: 50 });
  const fred = sse('fred', 'Fred');
  await waitUntil(() => fred.events[0]);
  assert.equal(fred.events[0].pos, 49, `dur clamp: ${fred.events[0].pos}`);
  fred.close();
  // a fresh load clears the stale duration: the next position is unclamped
  const loadsAt = a.events.filter((ev) => ev.type === 'load').length;
  await post({ type: 'load', from: 'bob', name: 'movie one.mkv' });
  await waitUntil(() => a.events.filter((ev) => ev.type === 'load').length === loadsAt + 1);
  await post({ type: 'position', from: 'bob', t: 60, p: true });
  const gina = sse('gina', 'Gina');
  await waitUntil(() => gina.events[0]);
  assert.equal(gina.events[0].pos, 60, `stale dur must not clamp: ${gina.events[0].pos}`);
  gina.close();

  // R7 single-master election: bob owns the clock (his heartbeats above);
  // a second position poster is dropped with exactly one notice, and the
  // claim dies with the owner's SSE connection.
  await post({ type: 'position', from: 'bob', t: 50, p: false });
  await waitUntil(() => a.events.some((e) => e.type === 'position' && e.t === 50));

  const noticesBefore = a.events.filter((e) => e.type === 'notice').length;
  const posBefore = a.events.filter((e) => e.type === 'position').length;
  await post({ type: 'position', from: 'impostor', t: 99, p: false });
  await waitUntil(() => a.events.some((e) => e.type === 'notice' && e.text.includes('ignoring impostor')));
  assert.equal(a.events.filter((e) => e.type === 'notice').length, noticesBefore + 1, 'notice not one-time');
  assert.equal(a.events.filter((e) => e.type === 'position').length, posBefore, 'impostor heartbeat relayed');
  await post({ type: 'position', from: 'impostor', t: 99, p: false }); // repeat: still one notice, no relay
  await sleep(300);
  assert.equal(a.events.filter((e) => e.type === 'notice').length, noticesBefore + 1);
  assert.equal(a.events.filter((e) => e.type === 'position').length, posBefore);

  // B2 pin: loading a queued file rebroadcasts the queue — every client
  // must see the playing file leave it (QUEUE box, /skip guard, /queue).
  // Uses a fresh file so state.now cannot shadow later scenarios' names.
  writeFileSync(join(dir, 'movie three.mkv'), Buffer.alloc(1000, 1));
  await post({ type: 'enqueue', from: 'bob', name: 'movie three.mkv' });
  await waitUntil(() => a.events.some((e) => e.type === 'queue' && e.queue.includes('movie three.mkv')));
  const qA = a.events.filter((e) => e.type === 'queue').length;
  const qB = b.events.filter((e) => e.type === 'queue').length;
  await post({ type: 'load', from: 'bob', name: 'movie three.mkv' });
  await waitUntil(() => a.events.filter((e) => e.type === 'queue').length > qA
    && b.events.filter((e) => e.type === 'queue').length > qB, 3000);
  const lastA = [...a.events].reverse().find((e) => e.type === 'queue');
  const lastB = [...b.events].reverse().find((e) => e.type === 'queue');
  assert.ok(!lastA.queue.includes('movie three.mkv'), 'playing file must leave the queue for alice');
  assert.ok(!lastB.queue.includes('movie three.mkv'), 'playing file must leave the queue for bob');

  // the claim dies with the owner's connection: the next poster takes over
  b.close();
  await waitUntil(() => a.events.some((e) => e.type === 'bye' && e.nick === 'Bob'));
  await post({ type: 'position', from: 'impostor', t: 60, p: false });
  await waitUntil(() => a.events.filter((e) => e.type === 'position').length > posBefore, 3000, 'successor relay');

  // ended is master-exclusive; skip stays dual-control
  await post({ type: 'enqueue', from: 'carl', name: 'movie two.mkv' });
  await waitUntil(() => a.events.some((e) => e.type === 'queue' && e.queue.includes('movie two.mkv')));
  const loadsBefore = a.events.filter((e) => e.type === 'load').length;
  await post({ type: 'ended', from: 'carl' }); // non-owner: dropped
  await sleep(400);
  assert.equal(a.events.filter((e) => e.type === 'load').length, loadsBefore, 'non-owner ended advanced the queue');
  await post({ type: 'ended', from: 'impostor' }); // owner: advances
  await waitUntil(() => a.events.filter((e) => e.type === 'load').length > loadsBefore);
  await post({ type: 'enqueue', from: 'carl', name: 'movie one.mkv' });
  await waitUntil(() => a.events.some((e) => e.type === 'queue' && e.queue.includes('movie one.mkv')));
  await post({ type: 'skip', from: 'carl' }); // anyone may skip
  await waitUntil(() => a.events.some((e) => e.type === 'load' && e.name === 'movie one.mkv'));

  b.close();
  await waitUntil(() => a.events.some((e) => e.type === 'bye' && e.nick === 'Bob'));
  a.close();
  console.log('PASS: all smoke assertions held');
  cleanup(0);
} catch (e) {
  console.error('FAIL:', e.message);
  cleanup(1);
}
