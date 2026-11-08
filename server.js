#!/usr/bin/env node
// Watchparty server: streams videos from a local folder over HTTP (Range-aware,
// zero transcode) and relays sync/chat/queue events between clients via SSE.
// Tailnet-only by design: no auth, no TLS. Do not port-forward this.
// Usage: VIDEOS_DIR=~/Videos node server.js   (PORT=8090 by default)

const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');

// B1-3: PORT=0 is a real request ("pick an ephemeral port"), not a falsy
// value: Number(env)||8090 turned it into 8090. A garbage PORT still
// falls back to the default (a NaN port would crash the listen).
const PORT = (() => {
  const raw = process.env.PORT;
  if (raw === undefined || raw === '') return 8090;
  const n = Number(raw);
  return Number.isInteger(n) && n >= 0 && n <= 65535 ? n : 8090;
})();
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
// F7: a failed persist is NOT latched off for the process lifetime: the
// disk can recover (a full volume that is emptied, a read-only mount that
// is fixed). The failure stays loud and the next attempt happens again.
const PERSIST_RETRY_MS = 5000;
let persistFailed = false;
let persistRetryTimer = null;
let stateDirty = false;
let stateTimer = null;
function stateSnapshot() {
  return JSON.stringify({
    now: state.now, queue: state.queue, pos: state.pos, paused: state.paused,
    dur: state.dur, wall: Date.now(),
  });
}
function persistFail(e) {
  persistFailed = true;
  console.error(`watchparty: state persistence failed (${e.message}) — retrying in ${PERSIST_RETRY_MS / 1000}s`);
  if (!persistRetryTimer) {
    persistRetryTimer = setTimeout(() => { persistRetryTimer = null; flushState(); }, PERSIST_RETRY_MS);
  }
}
function saveState() {
  // debounced write-behind: heartbeats fire ~0.5/s and a synchronous
  // write per event leaked event-loop latency that destabilized the
  // clients' seek-echo gates (soak bisect, .pi-state/test-ci-audit-raw.md)
  stateDirty = true;
  if (persistFailed) return; // the retry timer owns the next attempt (no log spam)
  if (!stateTimer) stateTimer = setTimeout(flushState, 2000);
}
// B3: ONE in-flight flush chain. Two writers on the same .tmp path would
// interleave their content: the room state could end up corrupted, or an
// older snapshot could win the rename. The exit path joins this chain
// instead of racing it.
let stateFlush = Promise.resolve();

function flushState() {
  stateTimer = null;
  if (!stateDirty) return;
  stateDirty = false;
  stateFlush = stateFlush.then(async () => {
    const tmp = `${STATE_FILE}.tmp`;
    await fs.promises.writeFile(tmp, stateSnapshot());
    const h = await fs.promises.open(tmp, 'r');
    try { await h.sync(); } finally { await h.close(); }
    // B4: keep the previous good snapshot — a corrupted state.json must
    // not wipe the room (the restore falls back to it, loudly)
    try {
      await fs.promises.copyFile(STATE_FILE, `${STATE_FILE}.bak`);
    } catch (e) {
      if (e.code !== 'ENOENT') console.error(`watchparty: state backup failed (${e.message})`);
    }
    await fs.promises.rename(tmp, STATE_FILE);
    persistFailed = false; // the disk is back: the normal path resumes
  }).catch((e) => {
    // F7: the snapshot we lost is still dirty -> the retry writes it
    stateDirty = true;
    persistFail(e);
  });
}
// F7: the exit must not wait for a stuck flush. A hung fsync (a stalled
// disk, a hung network mount) keeps a libuv threadpool thread busy
// forever: process.exit() cannot complete while such a write is stuck
// (master-2's probe: exit(0) called, the process still alive 9 s later).
// The exit therefore races a drain timeout, SKIPS the write with a loud
// log when the chain is still in flight, and hard-kills the process.
// The normal path is unchanged: an empty chain -> the exit write runs ->
// a clean exit.
// EXIT_DRAIN_MS is a knob (like SSE_MAX_BUFFER): the pin drives it down so
// the test does not depend on the wall clock of a loaded (shared) box.
const EXIT_DRAIN_MS = Number(process.env.EXIT_DRAIN_MS) || 2000;
const EXIT_WRITE_MS = 5000;
let exiting = false;
const persistAndExit = () => {
  if (exiting) return; // one exit path (a second signal must not race the first)
  exiting = true;
  let phase = 'drain'; // drain -> write
  const onExitTimeout = () => {
    console.error(phase === 'drain'
      ? 'watchparty: exit write skipped — the flush is still in flight'
      : 'watchparty: the exit-time state write is stuck — hard kill');
    process.kill(process.pid, 'SIGKILL'); // process.exit() cannot complete here
  };
  let timer = setTimeout(onExitTimeout, EXIT_DRAIN_MS);
  // a pending flush must not race the exit write on the same .tmp
  if (stateTimer) { clearTimeout(stateTimer); stateTimer = null; }
  // B3: the exit write JOINS the flush chain instead of racing it on the
  // same .tmp. The chain is one write+fsync+rename, so the join cannot
  // hold the exit for long.
  const exitPath = async () => {
    phase = 'write';
    clearTimeout(timer);
    timer = setTimeout(onExitTimeout, EXIT_WRITE_MS); // a stuck write must not hang the exit either
    if (stateDirty) {
      try {
        // A5-4 (amendment): the exit-time write is the most important persist
        // — the same atomic+durable dance as flushState (a signal handler
        // must finish before exit; the write is ASYNC so a stuck fsync
        // cannot block the event loop and disable the hard kill above)
        const tmp = `${STATE_FILE}.tmp`;
        await fs.promises.writeFile(tmp, stateSnapshot());
        const h = await fs.promises.open(tmp, 'r');
        try { await h.sync(); } finally { await h.close(); }
        await fs.promises.rename(tmp, STATE_FILE);
      } catch (e) {
        // C1: this was the ONLY empty catch in the file: a lost room state
        // (the dir vanished, the disk is full) was swallowed silently.
        // A lost room must be visible.
        console.error(`watchparty: exit-time state write failed (${e.message})`);
      }
    }
    process.exit(0);
  };
  stateFlush.then(exitPath, exitPath);
};
(function restoreState() {
  let saved;
  try {
    saved = JSON.parse(fs.readFileSync(STATE_FILE, 'utf8'));
  } catch (e) {
    if (e.code === 'ENOENT') return; // no state yet: a fresh server
    // B4: the main file is broken — the backup holds the room's last good
    // snapshot. The fallback is logged loudly: a silent restore looks
    // exactly like an empty room.
    console.error(`watchparty: state restore failed (${e.message}) — trying the backup`);
    try {
      saved = JSON.parse(fs.readFileSync(`${STATE_FILE}.bak`, 'utf8'));
      console.error(`watchparty: restored the room state from ${STATE_FILE}.bak`);
    } catch (e2) {
      console.error(`watchparty: backup restore failed too (${e2.message})`);
      return;
    }
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
    // F10 S3 (B1-1 + M2-02): the paused flag restores INDEPENDENTLY of
    // the position (F9's handoff, across a restart): a room that was
    // paused while it had no position yet must come back PAUSED — the
    // restore nested the pause inside the pos-valid branch and lost it.
    // The strict compare also kills the coercion nit: a hand-corrupted
    // paused:"false" is not a pause.
    state.paused = saved.paused === true;
    if (typeof saved.pos === 'number' && Number.isFinite(saved.pos) && saved.pos >= 0) {
      state.pos = saved.pos;
      // F2: a garbage wall must not NaN the age (hello.pos serialized as null)
      const wall = typeof saved.wall === 'number' && Number.isFinite(saved.wall) ? saved.wall : Date.now();
      state.posAt = mono() - Math.max(0, Date.now() - wall);
      state.dur = typeof saved.dur === 'number' && saved.dur > 0 ? saved.dur : null;
    }
  }
  console.log(`watchparty: restored room state from ${STATE_FILE}`);
})();

// B1: a client that stops reading must not make the server buffer every
// room event in its socket — the only unbounded memory path here.
const SSE_MAX_BUFFER = Number(process.env.SSE_MAX_BUFFER) || 4 * 1024 * 1024;

// sseWrite writes one SSE chunk. A client whose buffer grew past the
// threshold is dropped: its window never drains, so the buffer would
// grow for the life of the process.
function sseWrite(res, chunk) {
  if (res.writableEnded || res.destroyed) return false;
  if (res.writableLength > SSE_MAX_BUFFER) {
    console.error(`watchparty: dropping a client that stopped reading (${res.writableLength} bytes buffered)`);
    res.destroy();
    return false;
  }
  res.write(chunk);
  return true;
}

function broadcast(ev, exceptId) {
  const line = `data: ${JSON.stringify(ev)}\n\n`;
  for (const [res, id] of listeners) if (id !== exceptId) sseWrite(res, line);
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
    // A5-3: forged POST-only ids would grow this Set without bound — at the
    // cap, CLEAR it: bounded memory, and a legitimate 65th impostor still
    // gets its one-time notice (and can self-demote) instead of being
    // stranded silently (master-2 review, P3-3)
    if (impostorSeen.size >= 64) impostorSeen.clear();
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
// F3: cooldown for the old-file "the room moved on" notice — the stale
// master posts every ~2s, so a tight cooldown would spam the room
let lastMovedOnNotice = -Infinity;

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
    state.paused = false; // F10 S5 (B1-4): a newly loaded file starts playing
    const ev = { type: 'load', name: state.now, from: null };
    if (byNick) ev.nick = byNick; // actor nick (S-D); absent on restore/replay paths
    broadcast(ev);
    broadcast({ type: 'queue', queue: state.queue });
    saveState();
  }
}

// B2: the wrong-file gate drops a position that names another file. The
// poster is an OLD master still driving the previous film: its steering
// posts (load/play/pause) are dropped too, or it can drag the room back
// to its own film and pause a room that moved on. NO clearing exception
// (master-2 proved it re-opens the hole in two steps). Entries expire by
// TTL, so the set stays bounded.
const STALE_TTL_MS = Number(process.env.STALE_TTL_MS) || 60000;
const STALE_MAX = Number(process.env.STALE_MAX) || 1024; // F7: the map's absolute cap
const staleClients = new Map(); // client id -> mono() of the last gate hit

const isStale = (from) => {
  if (!from) return false;
  const at = staleClients.get(from);
  if (at === undefined) return false;
  if (mono() - at > STALE_TTL_MS) { staleClients.delete(from); return false; }
  return true;
};

// the set grows with the distinct stale ids: sweep the expired ones
setInterval(() => {
  for (const [id, at] of staleClients) if (mono() - at > STALE_TTL_MS) staleClients.delete(id);
}, Math.max(1000, STALE_TTL_MS / 2)).unref();

// the wrong-file gate (B2 for the positions, F10 S1 for the seeks): a post
// that names ANOTHER file comes from a client still driving the previous
// film — drop the post entirely and mark the poster stale for
// STALE_TTL_MS. The notice is rate-limited (the stale master posts every
// ~2s, so an unlimited notice would spam the room).
const wrongFile = (cmd) => {
  if (state.now === null || typeof cmd.file !== 'string' || cmd.file === state.now) return false;
  if (cmd.from) {
    // F7: the same cap-and-clear shape A5-3 gave impostorSeen: a forged
    // POST-only id flood must not grow the map without bound
    if (staleClients.size >= STALE_MAX) staleClients.clear();
    staleClients.set(cmd.from, mono());
  }
  if (mono() - lastMovedOnNotice > 10000) {
    lastMovedOnNotice = mono();
    // P2-1: state.now is a client-supplied FILENAME — same trust
    // boundary as every other notice text (w1-2 class)
    broadcast({ type: 'notice', from: null, text: `the room moved on: now playing ${clean(state.now, 64)}` });
  }
  return true;
};

// F10 S2 (M2-04/B2-2 + master-2's class completion): the Go client decodes
// EVERY event into the room.Event struct — a known field with the wrong
// JSON type makes the client drop the WHOLE event (room.go: the decode
// error skips the event), so the room advances while the clients miss it.
// The server used to relay such a body raw (the p case: p:"no" committed
// paused:true AND the raw body went out). These are the fields the client
// decodes; a present field must have exactly its type. The server's own
// broadcasts are typed by construction, and an UNKNOWN field keeps
// relaying AS-IS (the wire law).
const WIRE_TYPES = {
  type: 'string', nick: 'string', text: 'string', name: 'string',
  now: 'string', queue: 'strings', roster: 'strings', invite: 'string',
  inviteAlt: 'string', t: 'number', p: 'boolean', dur: 'number',
  file: 'string', from: 'string', pos: 'number', mechanical: 'boolean',
};
// wireTyped: the first known field with the wrong type, or null
const wireTyped = (cmd) => {
  for (const [field, want] of Object.entries(WIRE_TYPES)) {
    const v = cmd[field];
    if (v === undefined) continue; // absent: the field stays off the wire
    if (want === 'string' && typeof v !== 'string') return field;
    if (want === 'number' && (typeof v !== 'number' || !Number.isFinite(v))) return field;
    if (want === 'boolean' && typeof v !== 'boolean') return field;
    if (want === 'strings' && (!Array.isArray(v) || v.some((x) => typeof x !== 'string'))) return field;
  }
  return null;
};

// the drop log is RATE-LIMITED (the notice rate-limit's shape): a client
// that floods typed cmds must not fill the server's stderr — the drop
// itself is what matters, not one line per attempt.
let lastDropLog = -Infinity;
const DROP_LOG_MS = 1000;

// F11 F5 (A11-F5): the cmd trace is rate-limited PER TYPE, the same
// shape as the drop log. The trace logged one line per cmd: 100 chats
// wrote 100 lines, and the drop log never even engaged (valid traffic
// is not dropped) — the stderr-flood class F10 closed for drops stayed
// open through the trace. The first occurrence of a type logs at once,
// the repeats inside 1s collapse into ONE counted summary (the count
// must surface: a silent swallow is the bug this fixes).
const TRACE_LOG_MS = 1000;
const traceState = new Map(); // type -> { last, collapsed, timer }
const traceCmd = (cmd) => {
  const type = clean(String(cmd.type || ''), 24) || 'unknown';
  const now = mono();
  let st = traceState.get(type);
  if (!st) {
    // the type strings are client-supplied: the map must not grow
    // without bound (a hostile client could send a million types)
    if (traceState.size > 64) traceState.clear();
    st = { last: 0, collapsed: 0, timer: null };
    traceState.set(type, st);
  }
  if (now - st.last < TRACE_LOG_MS) {
    st.collapsed++;
    if (!st.timer) {
      st.timer = setTimeout(() => {
        st.timer = null;
        if (st.collapsed > 0) {
          console.log(`cmd: ${type} (${st.collapsed} suppressed in 1s)`);
          st.collapsed = 0;
        }
        st.last = mono();
      }, TRACE_LOG_MS);
      if (st.timer.unref) st.timer.unref(); // the timer must not hold the server open
    }
    return;
  }
  st.last = now;
  console.log(`cmd: ${type} from=${clean(String(cmd.from || ''), 32).slice(0, 8)}${cmd.type === 'seek' && cmd.t != null ? ` t=${Number(cmd.t).toFixed(1)}` : ''}${cmd.name ? ` name=${clean(cmd.name, 64)}` : ''}`);
};

function handleCmd(cmd) {
  // F7: every cmd type carries a nick -> normalize it through the same
  // clean() the roster uses. A client-supplied nick is untrusted text:
  // the probe showed ANSI escapes surviving on a position event (the
  // position case was the odd one out — chat was cleaned, the steering
  // types took the roster's nick).
  cmd.nick = clean(cmd.nick || '', 32);
  if (cmd.type === 'chat') {
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
  // F10 S2: the known-field type gate runs BEFORE any commit and before
  // the broadcast — a body the client would drop must not reach the room.
  const badField = wireTyped(cmd);
  if (badField) {
    if (mono() - lastDropLog > DROP_LOG_MS) {
      lastDropLog = mono();
      console.error(
        `watchparty: dropped a ${clean(cmd.type || '', 24)} cmd — the field "${badField}" is not ${WIRE_TYPES[badField]}`
      );
    }
    return;
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
      // F10 S1 (M2-03): seek is a steering type — it gets the gates the
      // other steering types have: a stale client cannot steer the room,
      // and a post that names another file is dropped (the wrong-file
      // gate's shape + notice).
      if (isStale(cmd.from)) break;
      if (wrongFile(cmd)) break;
      // F10 S4 (B1-2): the relayed seek must also land in the room state —
      // a late joiner's hello carried the pre-seek position (pos/posAt
      // already exist in the persisted shape: no format change).
      state.pos = cmd.t;
      state.posAt = mono();
      broadcast(cmd, cmd.from);
      saveState();
      break;
    case 'play':
    case 'pause':
      if (isStale(cmd.from)) break; // B2: a stale client cannot steer the room
      state.paused = cmd.type === 'pause';
      broadcast(cmd, cmd.from);
      saveState();
      break;
    case 'position':
      if (typeof cmd.t !== 'number' || !Number.isFinite(cmd.t) || cmd.t < 0) break; // same trust boundary as seek
      // F3/B2: a post that names another file comes from a client still
      // driving the previous film — drop the post ENTIRELY (no state.pos
      // overwrite, no claim refresh) and tell the human (rate-limited).
      // No `file` = old client = exactly the file-agnostic behavior; an
      // idle room has no file to protect, so the gate stays off there.
      if (wrongFile(cmd)) break;
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
      if (isStale(cmd.from)) break; // B2: a stale client cannot steer the room
      if (!(list() || []).includes(cmd.name)) break; // null listing (w1-1): cannot verify, no-op
      state.now = cmd.name;
      state.pos = null; // old file's position is meaningless now
      state.dur = null; // so is its duration
      state.paused = false; // F10 S5 (B1-4): a newly loaded file starts playing
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

  // S5: watchdog / manual liveness probe. F6: it also reports the room's
  // paused flag (a watcher needs to tell "paused" from "stuck"); this is a
  // diagnostic endpoint, not the room protocol.
  if (req.method === 'GET' && u.pathname === '/health') {
    res.writeHead(200, { 'content-type': 'application/json' });
    return res.end(JSON.stringify({ now: state.now, queueLen: state.queue.length, paused: state.paused, listeners: listeners.size }));
  }

  if (req.method === 'GET' && u.pathname === '/events') {
    res.writeHead(200, {
      'content-type': 'text/event-stream',
      'cache-control': 'no-cache',
      connection: 'keep-alive',
    });
    const id = u.searchParams.get('id') || '';
    const nick = clean(u.searchParams.get('nick') || id, 32);
    // B5: a half-open connection would keep its nick in the roster for
    // the life of the process: TCP keepalive lets the OS notice the dead
    // peer, the socket errors and the close handler cleans the roster.
    res.socket.setKeepAlive?.(true, 30000);
    listeners.set(res, id);
    nickById.set(id, nick);
    // roster rides hello: late joiners must see everyone already in the
    // room, not only the joins that happen after them
    const [inviteHost, inviteHostAlt] = inviteHosts();
    const hello = {
      type: 'hello', now: state.now, queue: state.queue,
      roster: [...new Set(nickById.values())],
      invite: `http://${inviteHost}:${realPort()}`,
    };
    if (inviteHostAlt) hello.inviteAlt = `http://${inviteHostAlt}:${realPort()}`; // absent when there is no second address
    // F9 item 2 (A9 S1 CASE B): the room's paused flag travels AS-IS and
    // INDEPENDENTLY of the position. A room that is paused but has no
    // position yet (a lone follower paused it; no clock owner ever beat)
    // must still hand the pause to a joining client — otherwise the
    // joiner's first heartbeat (p=false) erases the room's pause and the
    // film resumes under the follower's feet. We CARRY the state, we never
    // invent one: the "do not force p=true" intent below stands (forcing a
    // pause would pause a RECONNECTING master, whose p=true heartbeats
    // would then hold the room paused forever — gate regression:
    // master_restart).
    hello.p = state.paused;
    if (state.now && state.pos != null) {
      // M1-A: nobody is driving the room (ownerless after a clean bye or a
      // server restart, or the claim went stale on a half-open death) — do
      // not extrapolate a late joiner from a clock nobody is advancing
      // (m2-502: it would land them at EOF): serve the FROZEN last-known
      // position.
      if (clockOwner === null || claimStale()) hello.pos = state.pos;
      // age-adjusted: where the master is right now, not at heartbeat time
      else hello.pos = state.pos + (state.paused ? 0 : (mono() - state.posAt) / 1000);
      // S3: a known duration keeps long-downtime joins before EOF
      if (state.dur != null) hello.pos = Math.min(hello.pos, state.dur - 1);
      // A3: never hand out a negative position
      hello.pos = Math.max(0, hello.pos);
    }
    sseWrite(res, `data: ${JSON.stringify(hello)}\n\n`);
    // R9: replay chat history to first-time connections only (same-id
    // reconnects already have these lines in their TUI)
    if (id && !chatHistorySent.has(id)) {
      chatHistorySent.add(id); // per-process random ids: never re-replay one id
      if (chatHistorySent.size > 1024) { // bound: drop the oldest insertion
        for (const k of chatHistorySent) { chatHistorySent.delete(k); break; }
      }
      for (const ev of chatLog) sseWrite(res, `data: ${JSON.stringify(ev)}\n\n`);
    }
    broadcast({ type: 'join', nick }, id);
    const ping = setInterval(() => sseWrite(res, ': ping\n\n'), 15000);
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
          traceCmd(cmd);
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

// the real listening port: PORT=0 means "pick one for me" (B1-3), so the
// boot log and the invite must carry the port the server actually got
const realPort = () => server.address()?.port ?? PORT;

// gap#5: tests require() this file for inviteHosts; only the real entry
// point listens and registers signal handlers (I-2: a leaked persistAndExit
// would exit(0) the REQUIRING process on its own SIGINT/SIGTERM)
module.exports = { inviteHosts };
if (require.main === module) {
  process.on('SIGTERM', persistAndExit);
  process.on('SIGINT', persistAndExit); // m2-S6: Ctrl-C on a manual run must persist too
  server.listen(PORT, () =>
    console.log(`watchparty: serving ${VIDEOS} on http://0.0.0.0:${realPort()} (tailnet only, no auth)`)
  );
}
