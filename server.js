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
// w1-1: the videos dir can vanish/unmount at runtime; an unguarded readdir
// here would crash the process from EVERY listing path (/files /subs /library,
// load/enqueue gates, advanceQueue) — same crash class the /v stat guard covers
// null (NOT []) on failure: callers distinguish "empty dir" from "cannot
// read" (I-3) — the HTTP listings map null to [], advanceQueue keeps the
// queue on a transient read failure instead of wiping it
const list = () => {
  try { return fs.readdirSync(VIDEOS).filter((f) => VIDEO_RE.test(f)).sort(natural); }
  catch (e) { console.error(`watchparty: videos dir unreadable (${e.message})`); return null; }
};
// R8: subtitle siblings (movie.tr.srt, movie.es.srt, ...) live next to the
// videos; the client lists them per load and lets each user pick a track
// locally — subtitle choice never enters the sync layer
const SUB_RE = /\.(srt|ass|ssa|vtt)$/i;
const listSubs = () => {
  try { return fs.readdirSync(VIDEOS).filter((f) => SUB_RE.test(f)).sort(natural); }
  catch { return []; } // same dir condition as list(); its warning covers both
};

// S-A/S-C: the invite URLs only the server can know — a client may sit on
// another host, so hello points at dialable IPv4s instead of localhost.
// invite = best (tailscale 100.64/10, then RFC1918 private, then any other
// non-internal v4); inviteAlt = the best address from a DIFFERENT rank,
// drawn only from the CGNAT/RFC1918 pools (the tailscale+LAN pair is the
// goal). shortcut: docker's 172.16/12 ranks below the other RFC1918 ranges
// (bridges are rarely the address a friend can reach) — it is only picked
// when no other private address exists. Loopback only as the last resort.
// gap#5: the interfaces parameter makes the ranking unit-testable; production
// code always calls it without arguments.
function inviteHosts(interfaces = os.networkInterfaces()) {
  const oct = (a) => a.split('.').map(Number);
  const isCgnat = ([o1, o2]) => o1 === 100 && o2 >= 64 && o2 <= 127;
  const isDocker = ([o1, o2]) => o1 === 172 && o2 >= 16 && o2 <= 31;
  const isPrivate = ([o1, o2]) => o1 === 10 || (o1 === 192 && o2 === 168) || isDocker([o1, o2]);
  const cands = [];
  for (const ifs of Object.values(interfaces)) {
    // some platforms return undefined entries (H): never crash the /events handler
    for (const it of ifs || []) {
      // an entry itself can be undefined on some platforms (H): skip it
      if (!it || it.family !== 'IPv4' || it.internal) continue;
      const o = oct(it.address);
      // 0 CGNAT/tailscale · 1 RFC1918 · 2 docker 172.16/12 · 3 other non-internal
      const r = isCgnat(o) ? 0 : isPrivate(o) && !isDocker(o) ? 1 : isDocker(o) ? 2 : 3;
      cands.push({ addr: it.address, r });
    }
  }
  cands.sort((a, b) => a.r - b.r); // stable sort: interface order breaks ties
  const best = cands[0];
  // S-C2: alt comes only from the CGNAT/real-RFC1918 pools — docker 172.16/12
  // (r=2) must never become inviteAlt (a docker bridge is not the friend's LAN);
  // it stays eligible for best when it is the only address (unlabeled single).
  const alt = best && cands.find((c) => c !== best && c.r <= 1 && c.r !== best.r);
  return [best ? best.addr : '127.0.0.1', alt && alt.addr];
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
const persistAndExit = () => {
  if (persistOK && stateDirty) {
    try { fs.writeFileSync(STATE_FILE, stateSnapshot()); } catch { /* best effort */ }
  }
  process.exit(0);
};
(function restoreState() {
  let saved;
  try {
    saved = JSON.parse(fs.readFileSync(STATE_FILE, 'utf8'));
  } catch (e) {
    if (e.code !== 'ENOENT') console.error(`watchparty: state restore failed (${e.message})`);
    return;
  }
  // m2-503: a null listing (transient unreadable dir, w1-1/I-3) must not
  // wipe the persisted room at boot — "cannot read" is not "empty". The
  // disk filter applies only when the dir was actually readable;
  // advanceQueue already follows the same rule.
  const files = list();
  const onDisk = (n) => !files || files.includes(n);
  state.queue = Array.isArray(saved.queue) ? saved.queue.filter(onDisk) : [];
  if (saved.now && onDisk(saved.now)) {
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
// dual-control. The claim dies with the owner's SSE connection (a restarted
// or failed master is succeeded cleanly) — AND with the owner's silence
// (w1-3): a half-open death (laptop sleep, RST-less drop) never fires close,
// so a claim that gets no position heartbeat for CLAIM_FRESH_MS is released
// silently and the next poster takes over. Only the OWNER's own posts count:
// the stall-family follower posts nothing while the master is silent, so
// follower silence must never drop the claim. A paused master keeps
// heartbeating, so paused rooms are unaffected. 45s ≈ 20 missed 2s
// heartbeats; CLAIM_FRESH_MS shortens it for tests. An explicit value
// passes through verbatim — including 0 ("always stale", A5-8); unset,
// empty or non-numeric keeps the 45s default.
const cfRaw = String(process.env.CLAIM_FRESH_MS ?? '').trim();
const cfEnv = Number(cfRaw);
const CLAIM_FRESH_MS = cfRaw && Number.isFinite(cfEnv) ? cfEnv : 45000;
let clockOwner = null; // client id of the current master
let claimAt = -Infinity; // mono() of the owner's last position post
const impostorSeen = new Set();
// M1-A: is the current claim STALE (owner silent past CLAIM_FRESH_MS)?
// Read-only on purpose: hello/ended only OBSERVE staleness — only a
// competing position post may release the claim (isClockOwner), so an
// ended post can never demote a returning master.
const claimStale = () => clockOwner !== null && mono() - claimAt > CLAIM_FRESH_MS;
function isClockOwner(from) {
  if (from === clockOwner) { claimAt = mono(); return true; }
  if (claimStale()) {
    clockOwner = null; // stale claim (w1-3): released silently, no notice
  }
  if (clockOwner === null) {
    clockOwner = from; // first heartbeat wins: followers never post positions
    claimAt = mono();
    impostorSeen.delete(from);
    return true;
  }
  if (!impostorSeen.has(from)) {
    impostorSeen.add(from);
    broadcast({
      type: 'notice',
      // raw ids: named clients demote themselves to follower on this field
      // (never clean it — the client compares it to its own id)
      from,
      // w1-2: the text fallbacks embed client-supplied ids — same trust
      // boundary as the cmd trace, no raw ANSI/OSC may reach a TUI
      text: `two masters: ${nickById.get(clockOwner) || clean(clockOwner, 32)} owns the clock; ignoring ${nickById.get(from) || clean(from, 32)}`,
    });
  }
  return false;
}

// A2: room-wide cooldown — the queue notices explain a no-op, they must not
// become an amplifier for a stuck key or a hostile loop
let lastQueueNotice = -Infinity;

// advanceQueue is the shared body of ended/skip: promote the next queued
// file for everyone.
function advanceQueue(byNick) {
  if (!state.now) {
    // S2: idle skip/ended stays a no-op, but explains itself (every client
    // renders notices; the queue keeps waiting)
    if (mono() - lastQueueNotice > 2000) {
      lastQueueNotice = mono();
      broadcast({ type: 'notice', from: null, text: 'nothing playing; queue keeps waiting' });
    }
    return;
  }
  // enqueue checked the disk, but a file may have vanished since; never
  // auto-advance into a 404 that would hijack state.now. A null listing
  // (I-3: transient readdir failure) skips the filter — one bad read must
  // not wipe the queue or announce end of queue; the next turn re-checks.
  const files = list();
  if (files) {
    const hadQueue = state.queue.length > 0;
    state.queue = state.queue.filter((n) => files.includes(n));
    if (!state.queue.length) {
      // I2/w1-4: the queue emptying must be visible — a vanished last file
      // would otherwise leave a ghost queue on connected clients. The notice
      // rides the same transition (I-4): a skip on an ALREADY empty queue
      // stays quiet — repeating it explains nothing new.
      if (hadQueue) {
        broadcast({ type: 'queue', queue: state.queue });
        if (mono() - lastQueueNotice > 2000) {
          lastQueueNotice = mono();
          broadcast({ type: 'notice', from: null, text: 'end of queue' });
        }
      }
      return;
    }
  }
  if (state.queue.length) {
    state.now = state.queue.shift();
    state.pos = null; // positions belong to the previous file
    state.dur = null; // so does its duration
    const ev = { type: 'load', name: state.now, from: null };
    if (byNick) ev.nick = byNick; // actor nick (S-D); absent on restore/replay paths
    broadcast(ev);
    broadcast({ type: 'queue', queue: state.queue });
    saveState();
  }
}

function handleCmd(cmd) {
  if (cmd.type === 'chat') {
    cmd.nick = clean(cmd.nick || '', 32);
    cmd.text = clean(cmd.text || '', 500); // mirrors client ChatMaxLen
  }
  // S-B/S-D: play/pause/seek/load relays name the actor (client renders
  // "> pause by <nick>"). cmd.from is client-supplied so attribution is
  // spoofable — tailnet-trusted; but the nick itself comes ONLY from the
  // roster (join-time cleaned): a never-connected poster has no entry, so
  // the relay carries no nick instead of a client-supplied fake (B3).
  if (cmd.type === 'play' || cmd.type === 'pause' || cmd.type === 'seek' || cmd.type === 'load') {
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
      if (!(list() || []).includes(cmd.name)) break; // null listing (w1-1): cannot verify, no-op
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
      if ((list() || []).includes(cmd.name) && !state.queue.includes(cmd.name) && cmd.name !== state.now) {
        state.queue.push(cmd.name);
        const ev = { type: 'queue', queue: state.queue };
        const enqNick = nickById.get(cmd.from);
        if (enqNick) ev.nick = enqNick; // actor nick (S-D); unknown poster => no field (Karar #26)
        broadcast(ev);
        saveState();
      }
      break;
    case 'ended': // master hit EOF; a follower cannot end the file
      // A1: ended never CLAIMS the clock — only position heartbeats do. A
      // follower's EOF in an ownerless room must advance the queue without
      // demoting the real master when it returns.
      // M1-A: a STALE claim counts as ownerless here too (the owner has
      // been silent > CLAIM_FRESH_MS — half-open death): a follower's EOF
      // may advance the queue. Read-only: a returning master keeps its
      // claim; only a position post releases it.
      if (clockOwner === null || clockOwner === cmd.from || claimStale()) advanceQueue(nickById.get(cmd.from));
      break;
    case 'skip': // same advance, on demand — anyone (dual control)
      advanceQueue(nickById.get(cmd.from));
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
    return res.end(JSON.stringify(list() || [])); // null on readdir failure (w1-1)
  }

  if (req.method === 'GET' && u.pathname === '/subs') {
    res.writeHead(200, { 'content-type': 'application/json' });
    return res.end(JSON.stringify(listSubs()));
  }

  // S4: videos paired with their subtitle siblings — client parity: a sub
  // belongs to a video when its name starts with <video stem + ".">
  if (req.method === 'GET' && u.pathname === '/library') {
    const subs = listSubs();
    const library = (list() || []).map((f) => {
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
    const [inviteHost, inviteHostAlt] = inviteHosts();
    const hello = {
      type: 'hello', now: state.now, queue: state.queue,
      roster: [...new Set(nickById.values())],
      invite: `http://${inviteHost}:${PORT}`,
    };
    if (inviteHostAlt) hello.inviteAlt = `http://${inviteHostAlt}:${PORT}`; // absent when there is no second address
    if (state.now && state.pos != null) {
      // M1-A: nobody is driving the room (ownerless after a clean bye or a
      // server restart, or the claim went stale on a half-open death) — do
      // not extrapolate a late joiner from a clock nobody is advancing
      // (m2-502: it would land them at EOF): serve the FROZEN last-known
      // position. The paused flag travels AS-IS from the room state:
      // forcing p=true here would pause a RECONNECTING master, whose p=true
      // heartbeats would then hold the room paused forever (gate
      // regression: master_restart).
      if (clockOwner === null || claimStale()) hello.pos = state.pos;
      // age-adjusted: where the master is right now, not at heartbeat time
      else hello.pos = state.pos + (state.paused ? 0 : (mono() - state.posAt) / 1000);
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

// gap#5: tests require() this file for inviteHosts; only the real entry
// point listens and registers signal handlers (I-2: a leaked persistAndExit
// would exit(0) the REQUIRING process on its own SIGINT/SIGTERM)
module.exports = { inviteHosts };
if (require.main === module) {
  process.on('SIGTERM', persistAndExit);
  process.on('SIGINT', persistAndExit); // m2-S6: Ctrl-C on a manual run must persist too
  server.listen(PORT, () =>
    console.log(`watchparty: serving ${VIDEOS} on http://0.0.0.0:${PORT} (tailnet only, no auth)`)
  );
}
