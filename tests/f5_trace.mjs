// F11 worker2 (S2 / A11-F5): the cmd trace is rate-limited per type.
//
// The trace line (server.js, the POST handler) logged ONE LINE PER CMD:
// 100 chats = 100 trace lines. The F10 drop log never even engaged —
// valid traffic is not dropped — so the stderr-flood class F10 promised
// to close stayed open through the trace (A11: 100 valid chats -> 100
// trace lines, 0 drops; the mistyped variant -> 100 traces + 1 drop).
//
// The pins:
//   (a) 100 valid chats -> the chat trace is BOUNDED (rate-limited)
//   (b) 100 typed-garbage cmds -> the drop log stays rate-limited (F10)
//   (c) a quiet room -> the FIRST occurrence of each type still traces
//       (the rate limit must not silence the room)
//
// Server-only: no mpv, no client binary.
// Usage: node tests/f5_trace.mjs

import { strict as assert } from 'node:assert';
import { spawn } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
// the worker2 port band (f6: 18500+, persist: 18900+, f2: 19300+, f10: 19100+)
let portCounter = 19600 + Math.floor(Math.random() * 100);
const results = [];

const waitExit = (proc) =>
  proc.exitCode === null ? new Promise((r) => proc.on('exit', r)) : Promise.resolve();

function startServer(files) {
  const dir = mkdtempSync(join(tmpdir(), 'wp-f5-'));
  for (const f of files) writeFileSync(join(dir, f), Buffer.alloc(4096, 1));
  const port = portCounter++;
  const proc = spawn(process.execPath, [join(ROOT, 'server.js')], {
    env: { ...process.env, PORT: String(port), VIDEOS_DIR: dir, STATE_FILE: join(dir, 'state.json') },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let out = '';
  proc.stdout.on('data', (d) => (out += d));
  proc.stderr.on('data', (d) => (out += d));
  return {
    dir,
    base: `http://127.0.0.1:${port}`,
    proc,
    out: () => out,
    lines: (prefix) => out.split('\n').filter((l) => l.startsWith(prefix)),
    stop: async () => {
      if (proc.exitCode === null && proc.signalCode === null) {
        proc.kill();
        await waitExit(proc);
      }
      rmSync(dir, { recursive: true, force: true });
    },
  };
}

async function waitReady(base) {
  for (let i = 0; i < 100; i++) {
    try {
      const r = await fetch(`${base}/files`, { signal: AbortSignal.timeout(500) });
      if (r.ok) return;
    } catch {}
    await sleep(100);
  }
  throw new Error('the server did not come up');
}

async function post(base, obj) {
  const res = await fetch(`${base}/cmd`, { method: 'POST', body: JSON.stringify(obj) });
  assert.equal(res.status, 204, `the cmd post failed: ${res.status}`);
}

async function scenario(name, fn) {
  try {
    await fn();
    results.push(['PASS', name]);
    console.log(`PASS ${name}`);
  } catch (e) {
    results.push(['FAIL', `${name} — ${e.message}`]);
    console.error(`FAIL ${name}: ${e.message}`);
  }
}

// (a) 100 valid chats -> the trace is bounded.
await scenario('[F5] 100 valid chats do not write 100 trace lines', async () => {
  const s = startServer(['a.mkv']);
  try {
    await waitReady(s.base);
    for (let i = 0; i < 100; i++) await post(s.base, { type: 'chat', from: 'u', text: `hi ${i}` });
    await sleep(1300); // the collapse window closes: the count is reported
    const traces = s.lines('cmd: chat');
    assert.ok(
      traces.length <= 3,
      `100 chats wrote ${traces.length} trace lines (the trace is not rate-limited): ${JSON.stringify(traces.slice(0, 5))}`
    );
    // the count must be visible: a suppressed burst may not be silent
    const summary = traces.find((l) => /suppressed/.test(l));
    assert.ok(summary, `the suppressed burst was silent: ${JSON.stringify(traces)}`);
    assert.ok(/suppressed/.test(summary) && !/suppressed in 1s: 0/.test(summary), `the summary counted nothing: ${summary}`);
  } finally { await s.stop(); }
});

// (b) 100 typed-garbage cmds -> the drop log stays rate-limited (F10's
// behavior must not regress) and the drop still happens.
await scenario('[F5] 100 typed-garbage cmds keep the drop log rate-limited', async () => {
  const s = startServer(['a.mkv']);
  try {
    await waitReady(s.base);
    // the garbage: a known field with a wrong type (p is boolean). The
    // chat path coerces text/nick to strings, so the typed field must be
    // one it does NOT touch — otherwise the gate never sees the garbage.
    for (let i = 0; i < 100; i++) await post(s.base, { type: 'chat', from: 'u', text: 'hi', p: 'yes' });
    await sleep(1300);
    const drops = s.lines('watchparty: dropped a');
    assert.ok(drops.length >= 1, 'the typed cmds were not dropped at all');
    assert.ok(drops.length <= 2, `100 garbage cmds wrote ${drops.length} drop lines: ${JSON.stringify(drops)}`);
    const traces = s.lines('cmd: chat');
    assert.ok(traces.length <= 3, `100 garbage cmds wrote ${traces.length} trace lines`);
  } finally { await s.stop(); }
});

// (c) a quiet room -> the first occurrence of each type still traces.
await scenario('[F5] a quiet room traces the first occurrence of each type', async () => {
  const s = startServer(['a.mkv']);
  try {
    await waitReady(s.base);
    await post(s.base, { type: 'load', from: 'm', name: 'a.mkv' });
    await post(s.base, { type: 'chat', from: 'u', text: 'hello' });
    await post(s.base, { type: 'pause', from: 'm' });
    await post(s.base, { type: 'seek', from: 'm', t: 5 });
    await sleep(300);
    for (const type of ['chat', 'pause', 'seek']) {
      assert.ok(
        s.lines(`cmd: ${type}`).length >= 1,
        `the first ${type} was not traced (the rate limit over-blocked): ${JSON.stringify(s.lines('cmd: '))}`
      );
    }
  } finally { await s.stop(); }
});

const failed = results.filter(([r]) => r === 'FAIL');
if (failed.length) {
  console.error(`\n${failed.length}/${results.length} pin(s) failed:`);
  for (const [, name] of failed) console.error(`  - ${name}`);
  process.exit(1);
}
console.log(`\nPASS: all ${results.length} F5 trace pins held`);
