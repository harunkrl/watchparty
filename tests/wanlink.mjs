// TCP proxy with a shared bandwidth cap, used to simulate a slow WAN link.
// - classifies connections by request path (/events, /v/ + /sub/, /cmd)
// - video responses are paced to a shared byte budget (round-robin between
//   open connections, backpressure to the upstream so nothing piles up)
// - killEvents() drops the live SSE connections (network blip simulation)
import { createServer, connect } from 'node:net';

export class Proxy {
  constructor(port, upstreamPort, capBytesPerSec, label = '') {
    this.port = port; this.upstreamPort = upstreamPort; this.cap = capBytesPerSec; this.label = label;
    this.events = [];
    this.log = [];
    this.videoConns = [];
    this.bucket = { credit: 0, last: Date.now(), rr: 0 };
    if (this.cap > 0) this.tick();
    this.server = createServer((client) => this.handle(client));
  }
  tick() {
    const b = this.bucket;
    const now = Date.now();
    b.credit = Math.min(b.credit + ((now - b.last) / 1000) * this.cap, this.cap);
    b.last = now;
    let guard = 0;
    while (b.credit > 0 && this.videoConns.length && guard++ < 2000) {
      const c = this.videoConns[b.rr % this.videoConns.length];
      if (!c.queue.length) { b.rr++; continue; }
      const chunk = c.queue[0];
      if (chunk.length <= b.credit) {
        b.credit -= chunk.length; c.queue.shift(); c.queued -= chunk.length;
        c.client.write(chunk);
      } else b.rr++;
      b.rr++;
    }
    for (const c of this.videoConns) {
      if (c.queued < 256 * 1024 && c.up.isPaused()) c.up.resume();
    }
    setTimeout(() => this.tick(), 20);
  }
  handle(client) {
    const up = connect(this.upstreamPort, '127.0.0.1');
    let head = Buffer.alloc(0), classified = false, kind = 'other';
    const entry = { client, up, kind, queue: [], queued: 0 };
    const pending = [];
    client.on('data', (d) => {
      if (!classified) {
        head = Buffer.concat([head, d]);
        const idx = head.indexOf('\r\n\r\n');
        if (idx < 0) return;
        const reqHead = head.subarray(0, idx).toString('latin1');
        const first = reqHead.split('\r\n')[0] || '';
        kind = first.includes('/events') ? 'events' : (/\/v\/|\/sub\//.test(first) ? 'video' : (first.includes('/cmd') ? 'cmd' : 'other'));
        entry.kind = kind;
        this.log.push(`${this.label} ${kind}: ${first} | ${reqHead.split('\r\n').find((l) => l.toLowerCase().startsWith('range')) || 'no-range'}`);
        if (kind === 'events') this.events.push(entry);
        classified = true;
        up.write(head.subarray(0, idx + 4));
        const rest = head.subarray(idx + 4);
        if (rest.length) up.write(rest);
        for (const c of pending) this.deliver(entry, c);
        pending.length = 0;
        return;
      }
      up.write(d);
    });
    up.on('data', (d) => {
      if (!classified) { pending.push(d); return; }
      this.deliver(entry, d);
    });
    const drop = () => { try { client.destroy(); } catch {} try { up.destroy(); } catch {} };
    client.on('error', drop); up.on('error', drop);
    client.on('close', () => { up.destroy(); }); up.on('close', () => { client.destroy(); });
  }
  deliver(entry, data) {
    if (entry.kind !== 'video' || this.cap <= 0) { entry.client.write(data); return; }
    entry.queue.push(data); entry.queued += data.length;
    if (!this.videoConns.includes(entry)) this.videoConns.push(entry);
    if (entry.queued > 1024 * 1024) entry.up.pause(); // backpressure: do not buffer the whole file
  }
  killEvents() {
    const n = this.events.length;
    for (const e of this.events) { try { e.client.destroy(); } catch {} try { e.up.destroy(); } catch {} }
    this.events = [];
    return n;
  }
  listen() { return new Promise((res) => this.server.listen(this.port, '127.0.0.1', res)); }
  close() { try { this.server.close(); } catch {} }
}
