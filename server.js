#!/usr/bin/env node
// Watchparty server: streams videos from a local folder over HTTP (Range-aware,
// zero transcode) and relays sync/chat/queue events between clients via SSE.
// Tailnet-only by design: no auth, no TLS. Do not port-forward this.
// Usage: VIDEOS_DIR=~/Videos node server.js   (PORT=8090 by default)

const http = require('http');
const fs = require('fs');
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
const mono = () => Number(process.hrtime.bigint() / 1000000n);
const state = { now: null, queue: [], pos: null, paused: false, posAt: 0 };

const list = () => fs.readdirSync(VIDEOS).filter((f) => VIDEO_RE.test(f)).sort();

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
    wall: Date.now(),
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
    if (typeof saved.pos === 'number') {
      state.pos = saved.pos;
      state.paused = !!saved.paused;
      state.posAt = mono() - Math.max(0, Date.now() - (saved.wall || Date.now()));
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

// advanceQueue is the shared body of ended/skip: promote the next queued
// file for everyone.
function advanceQueue() {
  if (state.queue.length && state.now) {
    state.now = state.queue.shift();
    state.pos = null; // positions belong to the previous file
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
  switch (cmd.type) {
    case 'chat':
    case 'seek':
      broadcast(cmd, cmd.from);
      break;
    case 'play':
    case 'pause':
      state.paused = cmd.type === 'pause';
      broadcast(cmd, cmd.from);
      saveState();
      break;
    case 'position':
      if (!isClockOwner(cmd.from)) break;
      state.pos = cmd.t;
      state.paused = !!cmd.p;
      state.posAt = mono();
      broadcast(cmd, cmd.from);
      saveState();
      break;
    case 'load':
      if (!list().includes(cmd.name)) break;
      state.now = cmd.name;
      state.pos = null; // old file's position is meaningless now
      state.queue = state.queue.filter((n) => n !== cmd.name);
      broadcast(cmd, cmd.from);
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
      if (isClockOwner(cmd.from)) advanceQueue();
      break;
    case 'skip': // same advance, on demand — anyone (dual control)
      advanceQueue();
      break;
  }
}

// Serve the original file byte-for-byte. Only container-level HTTP Range
// support, so players can seek; never any transcoding.
function serveFile(req, res, file, ctype) {
  const size = fs.statSync(file).size;
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
    const hello = { type: 'hello', now: state.now, queue: state.queue };
    if (state.now && state.pos != null) {
      // age-adjusted: where the master is right now, not at heartbeat time
      hello.pos = state.pos + (state.paused ? 0 : (mono() - state.posAt) / 1000);
      hello.p = state.paused;
    }
    res.write(`data: ${JSON.stringify(hello)}\n\n`);
    broadcast({ type: 'join', nick }, id);
    const ping = setInterval(() => res.write(': ping\n\n'), 15000);
    req.on('close', () => {
      clearInterval(ping);
      listeners.delete(res);
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
        handleCmd(JSON.parse(body));
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
