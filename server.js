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
const VIDEO_RE = /\.(mkv|mp4|webm|avi|mov|m4v|ts|flv|wmv|mpg|mpeg)$/i;

fs.mkdirSync(VIDEOS, { recursive: true });

const listeners = new Map(); // sse response -> client id
// pos/paused/posAt: last master heartbeat, so late joiners can seek
// straight to the live room position instead of converging over seconds.
const state = { now: null, queue: [], pos: null, paused: false, posAt: 0 };

const list = () => fs.readdirSync(VIDEOS).filter((f) => VIDEO_RE.test(f)).sort();

function broadcast(ev, exceptId) {
  const line = `data: ${JSON.stringify(ev)}\n\n`;
  for (const [res, id] of listeners) if (id !== exceptId) res.write(line);
}

function handleCmd(cmd) {
  switch (cmd.type) {
    case 'chat':
    case 'seek':
      broadcast(cmd, cmd.from);
      break;
    case 'play':
    case 'pause':
      state.paused = cmd.type === 'pause';
      broadcast(cmd, cmd.from);
      break;
    case 'position':
      state.pos = cmd.t;
      state.paused = !!cmd.p;
      state.posAt = Date.now();
      broadcast(cmd, cmd.from);
      break;
    case 'load':
      if (!list().includes(cmd.name)) break;
      state.now = cmd.name;
      state.pos = null; // old file's position is meaningless now
      state.queue = state.queue.filter((n) => n !== cmd.name);
      broadcast(cmd, cmd.from);
      break;
    case 'enqueue':
      if (list().includes(cmd.name) && !state.queue.includes(cmd.name) && cmd.name !== state.now) {
        state.queue.push(cmd.name);
        broadcast({ type: 'queue', queue: state.queue });
      }
      break;
    case 'ended': // master hit EOF: auto-advance the queue for everyone
    case 'skip': // same, on demand
      if (state.queue.length && state.now) {
        state.now = state.queue.shift();
        state.pos = null; // positions belong to the previous file
        broadcast({ type: 'load', name: state.now, from: null });
        broadcast({ type: 'queue', queue: state.queue });
      }
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
    const nick = u.searchParams.get('nick') || id;
    listeners.set(res, id);
    const hello = { type: 'hello', now: state.now, queue: state.queue };
    if (state.now && state.pos != null) {
      // age-adjusted: where the master is right now, not at heartbeat time
      hello.pos = state.pos + (state.paused ? 0 : (Date.now() - state.posAt) / 1000);
      hello.p = state.paused;
    }
    res.write(`data: ${JSON.stringify(hello)}\n\n`);
    broadcast({ type: 'join', nick }, id);
    const ping = setInterval(() => res.write(': ping\n\n'), 15000);
    req.on('close', () => {
      clearInterval(ping);
      listeners.delete(res);
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
    const file = path.resolve(VIDEOS, name);
    if (!file.startsWith(VIDEOS + path.sep) || !fs.existsSync(file)) {
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
