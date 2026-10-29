#!/usr/bin/env node
// Watchparty server: streams videos from a local folder over HTTP (Range-aware,
// zero transcode) and relays sync/chat/queue events between clients via SSE.
// Tailnet-only by design: no auth, no TLS. Do not port-forward this.
// Usage: VIDEOS_DIR=~/Videos node server.js   (PORT=8090 by default)

const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');

const PORT = Number(process.env.PORT) || 8090;
const VIDEOS = path.resolve(process.env.VIDEOS_DIR || path.join(__dirname, 'videos'));
// resolved lazily on first /v or /sub request: the videos dir may not exist at boot
let realBase;
const VIDEO_RE = /\.(mkv|mp4|webm|avi|mov|m4v|ts|flv|wmv|mpg|mpeg)$/i;

fs.mkdirSync(VIDEOS, { recursive: true });

const listeners = new Map(); // sse response -> client id
const nickById = new Map(); // client id -> nick (notices name people, not ids)
// pos/paused/posAt: last master heartbeat, so late joiners can seek
// straight to the live room position instead of converging over seconds.
// posAt is hrtime-based (monotonic): an NTP wall-clock jump between
// heartbeats can no longer skew the age adjustment.
// dur: optional duration the master reports with its heartbeats (S3) —
// lets hello clamp long-downtime joins before EOF.
const mono = () => Number(process.hrtime.bigint() / 1000000n);
const state = { now: null, queue: [], pos: null, paused: false, posAt: 0, dur: null };

// S1: natural sort — "movie2" lists before "movie10" (numeric collation,
// locale pinned to 'en' so a host locale change cannot reorder listings)
const natural = new Intl.Collator('en', { numeric: true }).compare;
const list = () => fs.readdirSync(VIDEOS).filter((f) => VIDEO_RE.test(f)).sort(natural);
// R8: subtitle siblings (movie.tr.srt, movie.es.srt, ...) live next to the
// videos; the client lists them per load and lets each user pick a track
// locally — subtitle choice never enters the sync layer
const SUB_RE = /\.(srt|ass|ssa|vtt)$/i;
const listSubs = () => fs.readdirSync(VIDEOS).filter((f) => SUB_RE.test(f)).sort(natural);

// S-A: the invite URL only the server can know — a client may sit on another
// host, so hello points at a dialable IPv4 instead of localhost. Preference:
// tailscale (100.64/10), then RFC1918 private, then any non-internal v4;
// loopback only as the last resort.
function inviteHost() {
  const rank = (a) => {
    const [o1, o2] = a.split('.').map(Number);
    if (o1 === 100 && o2 >= 64 && o2 <= 127) return 0; // CGNAT / tailscale
    if (o1 === 10 || (o1 === 172 && o2 >= 16 && o2 <= 31) || (o1 === 192 && o2 === 168)) return 1;
    return 2;
  };
  let best = '127.0.0.1';
  let bestRank = 3;
  for (const ifs of Object.values(os.networkInterfaces())) {
    // some platforms return undefined entries (H): never crash the /events handler
    for (const it of ifs || []) {
      if (it.family !== 'IPv4' || it.internal) continue;
      const r = rank(it.address);
      if (r < bestRank) [best, bestRank] = [it.address, r];
    }
  }
  return best;
}

// R9: recent chat replayed once to each NEW connection id — a joiner sees
// what they missed; an SSE blip reconnect (same id) does not re-receive it.
// In-memory only: a server restart clears history.
const CHAT_HISTORY = 50;
const chatLog = [];
const chatHistorySent = new Set();

// R4: persist the room across restarts — a mid-film server restart used to
// wipe now/queue, leaving fresh joiners an empty room. state.json next to
// the server by default; STATE_FILE overrides (the systemd unit points it
// at /var/lib/watchparty — the deploy dir is read-only). posAt is
// monotonic and resets between boots, so the age travels as wall-clock ms.
const STATE_FILE = process.env.STATE_FILE || path.join(__dirname, 'state.json');
let persistOK = true;
let stateDirty = false;
let stateTimer = null;
function stateSnapshot() {
  return JSON.stringify({
    now: state.now, queue: state.queue, pos: state.pos, paused: state.paused,
    dur: state.dur, wall: Date.now(),
  });
}
function persistFail(e) {
  persistOK = false;
  console.error(`watchparty: state persistence disabled (${e.message})`);
}
function saveState() {
  // debounced write-behind: heartbeats fire ~0.5/s and a synchronous
  // write per event leaked event-loop latency that destabilized the
  // clients' seek-echo gates (soak bisect, .pi-state/test-ci-audit-raw.md)
  if (!persistOK) return;
  stateDirty = true;
  if (!stateTimer) stateTimer = setTimeout(flushState, 2000);
}
function flushState() {
  stateTimer = null;
  if (!stateDirty || !persistOK) return;
  stateDirty = false;
  const tmp = `${STATE_FILE}.tmp`;
  fs.writeFile(tmp, stateSnapshot(), (e) => {
    if (e) return persistFail(e);
    fs.rename(tmp, STATE_FILE, (e) => { if (e) persistFail(e); }); // atomic: no torn state.json
  });
}
process.on('SIGTERM', () => {
  if (persistOK && stateDirty) {
    try { fs.writeFileSync(STATE_FILE, stateSnapshot()); } catch { /* best effort */ }
  }
  process.exit(0);
});
(function restoreState() {
  let saved;
  try {
    saved = JSON.parse(fs.readFileSync(STATE_FILE, 'utf8'));
  } catch (e) {
    if (e.code !== 'ENOENT') console.error(`watchparty: state restore failed (${e.message})`);
    return;
  }
  const files = list();
  state.queue = Array.isArray(saved.queue) ? saved.queue.filter((n) => files.includes(n)) : [];
  if (saved.now && files.includes(saved.now)) {
    state.now = saved.now;
    if (typeof saved.pos === 'number' && Number.isFinite(saved.pos) && saved.pos >= 0) {
      state.pos = saved.pos;
      state.paused = !!saved.paused;
      // F2: a garbage wall must not NaN the age (hello.pos serialized as null)
      const wall = typeof saved.wall === 'number' && Number.isFinite(saved.wall) ? saved.wall : Date.now();
      state.posAt = mono() - Math.max(0, Date.now() - wall);
      state.dur = typeof saved.dur === 'number' && saved.dur > 0 ? saved.dur : null;
    }
  }
  console.log(`watchparty: restored room state from ${STATE_FILE}`);
})();

function broadcast(ev, exceptId) {
  const line = `data: ${JSON.stringify(ev)}\n\n`;
  for (const [res, id] of listeners) if (id !== exceptId) res.write(line);
}

function clean(s, max) {
  // strip control chars (newlines, ANSI escapes) and cap length: the
  // server is the trust boundary for what every TUI prints
  return String(s).replace(/[\x00-\x1f\x7f]/g, '').slice(0, max);
}

// R7: exactly one clock. The first client to post a position heartbeat
// owns the room clock (a master announces itself exactly that way); a
// second master's heartbeats are dropped with a one-time room notice.
// Only position/ended are master-exclusive — pause/seek/load/skip stay
// dual-control. The claim dies with the owner's SSE connection, so a
// restarted or failed master is succeeded cleanly.
let clockOwner = null; // client id of the current master
const impostorSeen = new Set();
function isClockOwner(from) {
  if (from === clockOwner) return true;
  if (clockOwner === null) {
    clockOwner = from; // first heartbeat wins: followers never post positions
    impostorSeen.delete(from);
    return true;
  }
  if (!impostorSeen.has(from)) {
    impostorSeen.add(from);
    broadcast({
      type: 'notice',
      from, // the impostor's id: named clients demote themselves to follower
      text: `two masters: ${nickById.get(clockOwner) || clockOwner} owns the clock; ignoring ${nickById.get(from) || from}`,
    });
  }
  return false;
}

// A2: room-wide cooldown — the idle notice explains a no-op, it must not
// become an amplifier for a stuck key or a hostile loop
let lastIdleNotice = -Infinity;

// advanceQueue is the shared body of ended/skip: promote the next queued
// file for everyone.
function advanceQueue() {
  if (!state.now) {
    // S2: idle skip/ended stays a no-op, but explains itself (every client
    // renders notices; the queue keeps waiting)
    if (mono() - lastIdleNotice > 2000) {
      lastIdleNotice = mono();
      broadcast({ type: 'notice', from: null, text: 'nothing playing; queue keeps waiting' });
    }
    return;
  }
  // enqueue checked the disk, but a file may have vanished since; never
  // auto-advance into a 404 that would hijack state.now
  state.queue = state.queue.filter((n) => list().includes(n));
  if (state.queue.length) {
    state.now = state.queue.shift();
    state.pos = null; // positions belong to the previous file
    state.dur = null; // so does its duration
    broadcast({ type: 'load', name: state.now, from: null });
    broadcast({ type: 'queue', queue: state.queue });
    saveState();
  }
}

function handleCmd(cmd) {
  if (cmd.type === 'chat') {
    cmd.nick = clean(cmd.nick || '', 32);
    cmd.text = clean(cmd.text || '', 500); // mirrors client ChatMaxLen
  }
  // S-B: play/pause/seek relays name the actor (client renders
  // "> pause by <nick>"). cmd.from is client-supplied so attribution is
  // spoofable — tailnet-trusted; but the nick itself comes ONLY from the
  // roster (join-time cleaned): a never-connected poster has no entry, so
  // the relay carries no nick instead of a client-supplied fake (B3).
  if (cmd.type === 'play' || cmd.type === 'pause' || cmd.type === 'seek') {
    cmd.nick = nickById.get(cmd.from); // undefined => field drops off the wire
  }
  switch (cmd.type) {
    case 'chat':
      chatLog.push({ type: 'chat', nick: cmd.nick, text: cmd.text, from: cmd.from });
      if (chatLog.length > CHAT_HISTORY) chatLog.shift();
      broadcast(cmd, cmd.from);
      break;
    case 'seek':
      // trust boundary: t must be a plain finite non-negative number, or
      // state.pos (and every late joiner's hello.pos) gets poisoned with
      // strings/objects
      if (typeof cmd.t !== 'number' || !Number.isFinite(cmd.t) || cmd.t < 0) break;
      broadcast(cmd, cmd.from);
      break;
    case 'play':
    case 'pause':
      state.paused = cmd.type === 'pause';
      broadcast(cmd, cmd.from);
      saveState();
      break;
    case 'position':
      if (typeof cmd.t !== 'number' || !Number.isFinite(cmd.t) || cmd.t < 0) break; // same trust boundary as seek
      if (!isClockOwner(cmd.from)) break;
      state.pos = cmd.t;
      state.paused = !!cmd.p;
      state.posAt = mono();
      // S3: optional duration, same trust boundary as t; a heartbeat may
      // omit it (the known value stays valid until the file changes)
      if (typeof cmd.dur === 'number' && Number.isFinite(cmd.dur) && cmd.dur > 0) state.dur = cmd.dur;
      broadcast(cmd, cmd.from);
      saveState();
      break;
    case 'load':
      if (!list().includes(cmd.name)) break;
      state.now = cmd.name;
      state.pos = null; // old file's position is meaningless now
      state.dur = null; // so is its duration
      state.queue = state.queue.filter((n) => n !== cmd.name);
      broadcast(cmd, cmd.from);
      // B2: same rebroadcast as advanceQueue — clients must see the playing
      // file leave the queue or their QUEUE box / skip guard go stale
      broadcast({ type: 'queue', queue: state.queue });
      saveState();
      break;
    case 'enqueue':
      if (list().includes(cmd.name) && !state.queue.includes(cmd.name) && cmd.name !== state.now) {
        state.queue.push(cmd.name);
        broadcast({ type: 'queue', queue: state.queue });
        saveState();
      }
      break;
    case 'ended': // master hit EOF; a follower cannot end the file
      // A1: ended never CLAIMS the clock — only position heartbeats do. A
      // follower's EOF in an ownerless room must advance the queue without
      // demoting the real master when it returns.
      if (clockOwner === null || clockOwner === cmd.from) advanceQueue();
      break;
    case 'skip': // same advance, on demand — anyone (dual control)
      advanceQueue();
      break;
  }
}

// Serve the original file byte-for-byte. Only container-level HTTP Range
// support, so players can seek; never any transcoding.
function serveFile(req, res, file, ctype) {
  // the file can vanish between realpath and stat (TOCTOU); an unguarded
  // throw here would take the whole server down
  let size;
  try {
    size = fs.statSync(file).size;
  } catch {
    res.writeHead(404);
    return res.end();
  }
  const headers = { 'content-type': ctype, 'accept-ranges': 'bytes', 'content-length': size };
  if (size === 0) { // 0-byte file: createReadStream would throw on end=-1
    res.writeHead(200, headers);
    return res.end();
  }
  let start = 0;
  let end = size - 1;
  let status = 200;
  const range = req.headers.range;
  if (range && !range.includes(',')) {
    const m = /bytes=(\d*)-(\d*)/.exec(range);
    if (m) {
      status = 206;
      if (m[1]) start = parseInt(m[1], 10);
      else if (m[2]) start = Math.max(0, size - parseInt(m[2], 10)); // suffix range
      if (m[1] && m[2]) end = parseInt(m[2], 10); // explicit end only; suffix keeps size-1
      if (!Number.isFinite(start) || start > end || start >= size) {
        res.writeHead(416, { 'content-range': `bytes */${size}` });
        return res.end();
      }
      end = Math.min(end, size - 1);
      headers['content-range'] = `bytes ${start}-${end}/${size}`;
      headers['content-length'] = end - start + 1;
    }
  }
  if (req.method === 'HEAD') {
    res.writeHead(status, headers);
    return res.end();
  }
  res.writeHead(status, headers);
  const stream = fs.createReadStream(file, { start, end });
  stream.on('error', (e) => {
    // a dead read (EIO/EPERM) would otherwise hang the response mid-body
    console.error(`stream error ${file}: ${e.message}`);
    res.destroy();
  });
  // R-out2: mid-body client hangups land in the log — seek aborts are
  // normal and rare, but a burst without seeks is the outlier signature
  // seen once in live WAN runs (both streams ended early, no error)
  res.on('close', () => {
    if (!res.writableEnded) console.log(`aborted ${path.basename(file)} bytes ${start}-${end}`);
  });
  stream.pipe(res);
}

const server = http.createServer((req, res) => {
  const u = new URL(req.url, 'http://localhost');

  if (req.method === 'GET' && u.pathname === '/files') {
    res.writeHead(200, { 'content-type': 'application/json' });
    return res.end(JSON.stringify(list()));
  }

  if (req.method === 'GET' && u.pathname === '/subs') {
    res.writeHead(200, { 'content-type': 'application/json' });
    return res.end(JSON.stringify(listSubs()));
  }

  // S4: videos paired with their subtitle siblings — client parity: a sub
  // belongs to a video when its name starts with <video stem + ".">
  if (req.method === 'GET' && u.pathname === '/library') {
    const subs = listSubs();
    const library = list().map((f) => {
      const stem = f.slice(0, f.length - path.extname(f).length);
      return { name: f, subs: subs.filter((s) => s.startsWith(`${stem}.`)) };
    });
    res.writeHead(200, { 'content-type': 'application/json' });
    return res.end(JSON.stringify(library));
  }

  // S5: watchdog / manual liveness probe
  if (req.method === 'GET' && u.pathname === '/health') {
    res.writeHead(200, { 'content-type': 'application/json' });
    return res.end(JSON.stringify({ now: state.now, queueLen: state.queue.length, listeners: listeners.size }));
  }

  if (req.method === 'GET' && u.pathname === '/events') {
    res.writeHead(200, {
      'content-type': 'text/event-stream',
      'cache-control': 'no-cache',
      connection: 'keep-alive',
    });
    const id = u.searchParams.get('id') || '';
    const nick = clean(u.searchParams.get('nick') || id, 32);
    listeners.set(res, id);
    nickById.set(id, nick);
    // roster rides hello: late joiners must see everyone already in the
    // room, not only the joins that happen after them
    const hello = {
      type: 'hello', now: state.now, queue: state.queue,
      roster: [...new Set(nickById.values())],
      invite: `http://${inviteHost()}:${PORT}`,
    };
    if (state.now && state.pos != null) {
      // age-adjusted: where the master is right now, not at heartbeat time
      hello.pos = state.pos + (state.paused ? 0 : (mono() - state.posAt) / 1000);
      // S3: a known duration keeps long-downtime joins before EOF
      if (state.dur != null) hello.pos = Math.min(hello.pos, state.dur - 1);
      // A3: never hand out a negative position
      hello.pos = Math.max(0, hello.pos);
      hello.p = state.paused;
    }
    res.write(`data: ${JSON.stringify(hello)}\n\n`);
    // R9: replay chat history to first-time connections only (same-id
    // reconnects already have these lines in their TUI)
    if (id && !chatHistorySent.has(id)) {
      chatHistorySent.add(id); // per-process random ids: never re-replay one id
      if (chatHistorySent.size > 1024) { // bound: drop the oldest insertion
        for (const k of chatHistorySent) { chatHistorySent.delete(k); break; }
      }
      for (const ev of chatLog) res.write(`data: ${JSON.stringify(ev)}\n\n`);
    }
    broadcast({ type: 'join', nick }, id);
    const ping = setInterval(() => res.write(': ping\n\n'), 15000);
    req.on('close', () => {
      clearInterval(ping);
      listeners.delete(res);
      // a same-id reconnect can register before the dead socket's close
      // fires; only tear down presence when no live connection still
      // carries this id
      if ([...listeners.values()].includes(id)) return;
      if (id === clockOwner) clockOwner = null; // succeeded by the next heartbeat poster
      impostorSeen.delete(id);
      nickById.delete(id);
      broadcast({ type: 'bye', nick }, id);
    });
    return;
  }

  if (req.method === 'POST' && u.pathname === '/cmd') {
    let body = '';
    req.on('data', (c) => {
      if (body.length + c.length > 8192) { // never buffer unbounded input
        res.writeHead(413);
        res.end();
        req.destroy();
        return;
      }
      body += c;
    });
    req.on('end', () => {
      try {
        const cmd = JSON.parse(body);
        // discrete cmd trace: who drove the room (position spam excluded).
        // This is the log that pins echo/duplicate bugs (R-out1b).
        // t is logged for seek only: C7 clients tag every cmd with t/p, and
        // echoing it on chat/pause lines would bury the trace in noise
        if (cmd.type !== 'position') {
          // trace fields are client-supplied: clean() keeps ANSI escapes
          // out of the server log
          console.log(`cmd: ${clean(cmd.type || '', 24)} from=${clean(String(cmd.from || ''), 32).slice(0, 8)}${cmd.type === 'seek' && cmd.t != null ? ` t=${Number(cmd.t).toFixed(1)}` : ''}${cmd.name ? ` name=${clean(cmd.name, 64)}` : ''}`);
        }
        handleCmd(cmd);
        res.writeHead(204);
      } catch {
        res.writeHead(400);
      }
      res.end();
    });
    return;
  }

  const m = (req.method === 'GET' || req.method === 'HEAD') && u.pathname.match(/^\/(v|sub)\/(.+)$/);
  if (m) {
    let name;
    try {
      name = decodeURIComponent(m[2]);
    } catch {
      res.writeHead(400); // malformed percent-encoding must not kill the process
      return res.end();
    }
    let file;
    try {
      // realpath resolves symlinks: lexical containment alone is not enough,
      // createReadStream would happily follow a symlink planted in VIDEOS
      file = fs.realpathSync(path.resolve(VIDEOS, name));
      realBase ||= fs.realpathSync(VIDEOS);
    } catch {
      res.writeHead(404);
      return res.end();
    }
    if (!file.startsWith(realBase + path.sep)) {
      res.writeHead(404);
      return res.end();
    }
    return serveFile(req, res, file, m[1] === 'v' ? 'video/mp4' : 'text/plain');
  }

  res.writeHead(404);
  res.end();
});

server.listen(PORT, () =>
  console.log(`watchparty: serving ${VIDEOS} on http://0.0.0.0:${PORT} (tailnet only, no auth)`)
);
