#!/usr/bin/env python3
"""Watchparty client: runs a local mpv and keeps it in sync with the room.

Host side (the machine with the videos, clock authority):
    python3 client.py http://<tailscale-ip>:8090 --master --name ada

Guest side (plays the same stream, follows the host):
    python3 client.py http://<tailscale-ip>:8090 --name bo

Terminal commands once running:
    list | play <file> | queue <file> | chat <msg> | quit

Zero dependencies beyond mpv itself (stdlib only).
"""

import argparse
import getpass
import json
import os
import random
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse
import urllib.request

TOLERANCE = 1.0  # seconds of allowed drift before forcing a re-seek
POLL_SECS = 2

def log(msg):
    print(msg, flush=True)

class Ipc:
    """Minimal mpv JSON-IPC client core; transport in subclasses."""

    def __init__(self):
        self.buf = b''
        self.id = 0
        self.lock = threading.Lock()

    def _write(self, data):
        raise NotImplementedError

    def _read(self, n):
        raise NotImplementedError

    def _send(self, obj):
        self._write((json.dumps(obj) + '\n').encode())

    def _readline(self):
        while b'\n' not in self.buf:
            chunk = self._read(4096)
            if not chunk:
                raise ConnectionError('mpv ipc closed')
            self.buf += chunk
        line, self.buf = self.buf.split(b'\n', 1)
        return line

    def cmd(self, *args):
        """Run a command and wait for its response."""
        with self.lock:
            self.id += 1
            rid = self.id
            self._send({'command': list(args), 'request_id': rid})
            while True:
                try:
                    msg = json.loads(self._readline())
                except ValueError:
                    continue  # skip malformed lines instead of killing the sync thread
                if msg.get('request_id') == rid:
                    if msg.get('error') != 'success':
                        raise RuntimeError(f'mpv: {msg["error"]}: {args}')
                    return msg.get('data')

    def tell(self, *args):
        """Fire-and-forget command (response is skipped on the next cmd)."""
        with self.lock:
            self._send({'command': list(args)})


class UnixIpc(Ipc):
    def __init__(self, path):
        super().__init__()
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect(path)

    def _write(self, data):
        self.sock.sendall(data)

    def _read(self, n):
        return self.sock.recv(n)


class PipeIpc(Ipc):
    """Windows named pipe transport."""

    def __init__(self, path):
        super().__init__()
        # pi-lens-ignore: ast-grep:unchecked-throwing-call-python
        self.f = open(path, 'r+b', buffering=0)

    def _write(self, data):
        os.write(self.f.fileno(), data)

    def _read(self, n):
        return os.read(self.f.fileno(), n)


class Player:
    def __init__(self, url_base, extra_args):
        self.url_base = url_base
        self.name = None
        # mkdtemp: unguessable dir so nobody can squat the socket path
        sock = (f'\\\\.\\pipe\\watchparty-{os.getpid()}' if os.name == 'nt'
                else os.path.join(tempfile.mkdtemp(prefix='watchparty-'),
                                  f'mpv-{os.getpid()}.sock'))
        args = [
            shutil.which('mpv') or 'mpv',
            '--idle', '--force-window', '--really-quiet', '--title=Watchparty',
            f'--input-ipc-server={sock}',
            # movie, not a call: buffer ahead aggressively so upload hiccups
            # never stall playback
            '--cache=yes', '--demuxer-readahead-secs=300', '--demuxer-max-bytes=2GiB',
        ] + extra_args
        # DEVNULL: otherwise mpv steals the terminal keyboard from the client
        self.proc = subprocess.Popen(args, stdin=subprocess.DEVNULL)
        for _ in range(50):
            try:
                self.ipc = (PipeIpc if os.name == 'nt' else UnixIpc)(sock)
                break
            except OSError:
                time.sleep(0.1)
        else:
            sys.exit('could not connect to mpv ipc (is mpv installed?)')

    def get(self, prop):
        try:
            return self.ipc.cmd('get_property', prop)
        except (RuntimeError, ConnectionError):
            return None

    def load(self, name):
        url = f'{self.url_base}/v/' + urllib.parse.quote(name)
        log(f'> playing {name}')
        self.ipc.tell('loadfile', url)
        self.name = name
        for _ in range(50):  # wait until loaded AND seekable: a seek issued
            # between loadfile and file-loaded is silently dropped by mpv
            if self.get('path') == url and self.get('seekable'):
                break
            time.sleep(0.1)
        # attach a sidecar subtitle if one sits next to the video;
        # 404 simply means "no subtitle" — not an error worth logging
        stem = os.path.splitext(name)[0]
        for ext in ('.srt', '.ass'):
            sub = f'{self.url_base}/sub/' + urllib.parse.quote(stem + ext)
            try:
                urllib.request.urlopen(sub, timeout=5).close()
                self.ipc.tell('sub-add', sub, 'auto')
                break
            except OSError:  # covers URLError/RemoteDisconnected too
                pass

    def apply(self, ev):
        if ev['type'] == 'load':
            self.load(ev['name'])
        elif ev['type'] == 'pause':
            self.ipc.tell('set_property', 'pause', True)
        elif ev['type'] == 'play':
            self.ipc.tell('set_property', 'pause', False)
        elif ev['type'] == 'seek':
            self.ipc.tell('seek', ev['t'], 'absolute')


class Client:
    def __init__(self, base, name, master, extra_args):
        self.base = base.rstrip('/')
        self.name = name
        self.master = master
        self.id = f'{name}-{random.randint(1000, 9999)}'
        self.master_pos = None    # (position, master_paused, monotonic time when received)
        self.room_pause = False   # last pause state the room (or our mirror) announced
        self.last_apply = 0.0     # monotonic clock of the last discrete apply (echo gate)
        self.last_pause_apply = 0.0  # last pause/play apply; a seek apply must not gate a pause post
        self.last_post = 0.0      # monotonic clock of our last dual-control post (grace)
        self.last_speed = 1.0     # last speed we told mpv (skip no-op IPC chatter)
        self.player = Player(self.base, extra_args)

    def post(self, obj):
        req = urllib.request.Request(
            self.base + '/cmd',
            data=json.dumps({**obj, 'from': self.id}).encode(),
            headers={'content-type': 'application/json'},
        )
        urllib.request.urlopen(req, timeout=5).close()

    # --- inbound events -----------------------------------------------------
    def on_event(self, ev):
        if ev.get('from') == self.id and ev['type'] != 'hello':
            return  # our own message echoed back
        t = ev['type']
        if t == 'hello':
            log(f"> room: now={ev['now']} queue={ev['queue']}")
            if ev['now']:
                self.player.load(ev['now'])
                # server may carry an age-adjusted media pos + paused
                # flag from the room's live snapshot; converge now instead
                # of waiting for the next heartbeat (avoids ~2s of wrong
                # playback while master_pos trickles in)
                if ev.get('pos') is not None:
                    p = bool(ev.get('p'))
                    self.player.ipc.tell('seek', ev['pos'], 'absolute')
                    if p:
                        self.player.ipc.tell('set_property', 'pause', True)
                    self.master_pos = (ev['pos'], p, time.monotonic())
                    # mirror the applied state so (a)/(c) treat it as room truth
                    self.room_pause = p
                    self.last_apply = time.monotonic()
                self.last_speed = 1.0  # new file: drop any cached micro-adjust
        elif t == 'queue':
            log(f'> queue: {ev["queue"]}')
        elif t == 'join':
            log(f'> {ev["nick"]} joined')
        elif t == 'bye':
            log(f'> {ev["nick"]} left')
        elif t == 'chat':
            log(f'[{ev["nick"]}] {ev["text"]}')
            self.player.ipc.tell('show-text', f'{ev["nick"]}: {ev["text"]}', 3000)
        elif t == 'position':
            self.master_pos = (ev['t'], bool(ev.get('p')), time.monotonic())
            self.room_pause = bool(ev.get('p'))  # heartbeat is the slow truth
        elif t in ('load', 'play', 'pause', 'seek'):
            log(f'> {t}' + (f' @{ev["t"]:.1f}s' if 't' in ev else ''))
            self.player.apply(ev)
            # mark the apply so the next poll won't re-post the mpv echo
            # (a) AND so (c) knows we just acted ourselves (gate window)
            if t in ('pause', 'play'):
                self.room_pause = (t == 'pause')
                self.last_apply = time.monotonic()
                self.last_pause_apply = self.last_apply
            elif t == 'seek':
                self.last_apply = time.monotonic()
            elif t == 'load':
                self.last_speed = 1.0  # new file: drop any cached micro-adjust

    def sse_loop(self):
        attempt = 0
        while True:
            try:
                r = urllib.request.urlopen(
                    f'{self.base}/events?id={self.id}&nick={urllib.parse.quote(self.name)}',
                    timeout=30)
                attempt = 0
                for raw in r:
                    line = raw.decode('utf-8', 'replace').strip()
                    if line.startswith('data: '):
                        self.on_event(json.loads(line[6:]))
            except Exception as e:
                delay = min(30, 3 * 2 ** min(attempt, 4))
                log(f'[conn] lost ({e}), retrying in {delay}s')
                attempt += 1
                time.sleep(delay)

    # --- sync engine --------------------------------------------------------
    def poll_loop(self):
        last_pos, last_pause, was_active = None, False, False
        while True:
            time.sleep(POLL_SECS)
            if self.player.proc.poll() is not None:
                log('> mpv exited (window closed?) - restart the client')
                os._exit(1)
            pos = self.player.get('time-pos')
            pause = self.player.get('pause')
            idle = self.player.get('idle-active')
            now = time.monotonic()  # shared by master gate + follower (a)..(d)
            if self.master:
                if pos is None and not idle:
                    continue  # nothing loaded yet
                # gate pause post: remote pause/play echoes must not re-post
                if pause != last_pause and not idle and (now - self.last_pause_apply) > 1.0:
                    self.post({'type': 'pause' if pause else 'play'})
                last_pause = pause  # unconditional: state tracking cannot stall
                if pos is not None:
                    # gate scrub post: a remote seek's position jump must not look
                    # like a scrub; 3s = one full poll past any remote apply (a
                    # 1s window lets ~half of the echoes escape between ticks)
                    if last_pos is not None and abs(pos - last_pos - POLL_SECS) > 3 and (now - self.last_apply) > 3.0:
                        self.post({'type': 'seek', 't': pos})  # user scrubbed
                    self.post({'type': 'position', 't': pos, 'p': bool(pause)})
                    last_pos = pos
                if idle and was_active:
                    self.post({'type': 'ended'})
                    was_active = False
                elif not idle and pos is not None:
                    was_active = True
            elif pos is not None and self.master_pos:
                t0, master_paused, recv = self.master_pos
                # (a) local pause transition (dual control): post only when
                # our pause diverges from the room's last announcement AND
                # we are outside the recent-apply window (which absorbs the
                # mpv echo of a remote apply as a fresh user move)
                if pause != last_pause:
                    # gate on pause applies only: a just-applied remote seek
                    # must not swallow the user's pause (go actuatePause parity)
                    if pause != self.room_pause and (now - self.last_pause_apply) > 1.0:
                        self.post({'type': 'pause' if pause else 'play'})
                        self.room_pause = pause
                        self.last_post = now
                    last_pause = pause
                # (b) scrub detection: a positional jump > 3s past the expected
                # 2s tick is a user seek (paused or not — dragging while paused
                # must reach the room too, go parity); 3s apply window so a
                # remote seek's echo is never mistaken for a fresh scrub
                if last_pos is not None and \
                   abs(pos - last_pos - POLL_SECS) > 3 and \
                   (now - self.last_apply) > 3.0:
                    self.post({'type': 'seek', 't': pos})
                    self.last_apply = now
                    self.last_post = now
                last_pos = pos
                # (c) self-heal: room is authoritative when we are settled;
                # grace window mirrors go's followerGrace (POLL_SECS+1) so a
                # just-posted user move is never stomped by a stale heartbeat
                # that still carries the pre-apply pause state
                if master_paused != pause and \
                   (now - max(self.last_apply, self.last_post)) > 3.0:
                    self.player.ipc.tell('set_property', 'pause', master_paused)
                    last_pause = master_paused
                    self.room_pause = master_paused
                    self.last_apply = now
                # (d) drift correction (playing only): |drift|>1 hard seek;
                # in the gentle band a speed micro-adjust so we converge
                # silently instead of jumping every POLL_SECS tick; last_speed
                # caches the last value so we only IPC when it actually moves
                if not master_paused:
                    expected = t0 + (now - recv)
                    drift = pos - expected
                    if abs(drift) > 1.0:
                        self.player.ipc.tell('seek', expected, 'absolute')
                        target = 1.0
                    elif abs(drift) > 0.15:
                        target = max(0.95, min(1.05, 1 - drift * 0.10))
                    else:
                        target = 1.0
                    if abs(target - self.last_speed) > 0.001:
                        self.player.ipc.tell('set_property', 'speed', target)
                        self.last_speed = target
                elif self.last_speed != 1.0:
                    # paused: snap speed back so the next play runs normally
                    self.player.ipc.tell('set_property', 'speed', 1.0)
                    self.last_speed = 1.0
                # both paused: hold position

    def shutdown(self):
        # hardening: guarantee mpv dies even if it ignores SIGTERM,
        # then hard-exit (daemon threads would block a clean exit)
        p = self.player.proc
        p.terminate()
        try:
            p.wait(timeout=2)
        except subprocess.TimeoutExpired:
            p.kill()
            p.wait()
        os._exit(0)

    # --- terminal -----------------------------------------------------------
    def terminal(self):
        log('commands: list | play <file> | queue <file> | skip | chat <msg> | quit')
        for line in sys.stdin:
            parts = line.strip().split(None, 1)
            if not parts:
                continue
            cmd, arg = parts[0], (parts[1] if len(parts) > 1 else '')
            try:
                if cmd == 'list':
                    with urllib.request.urlopen(self.base + '/files', timeout=5) as r:
                        for f in json.load(r):
                            log('  ' + f)
                elif cmd == 'play' and arg:
                    self.player.load(arg)  # server relays to others
                    self.last_speed = 1.0  # new file: drop cached micro-adjust
                    self.post({'type': 'load', 'name': arg})
                elif cmd == 'queue' and arg:
                    self.post({'type': 'enqueue', 'name': arg})
                elif cmd == 'skip':
                    self.post({'type': 'skip'})  # server broadcasts the next load to all
                elif cmd == 'chat' and arg:
                    log(f'[{self.name}] {arg}')
                    self.post({'type': 'chat', 'nick': self.name, 'text': arg})
                elif cmd == 'quit':
                    self.shutdown()
                else:
                    log('commands: list | play <file> | queue <file> | skip | chat <msg> | quit')
            except Exception as e:
                log(f'[err] {e}')


def main():
    ap = argparse.ArgumentParser(description='Watchparty sync client')
    ap.add_argument('server', help='e.g. http://100.101.102.103:8090')
    ap.add_argument('--name', default=getpass.getuser())
    ap.add_argument('--master', action='store_true',
                    help='run on the host side; this player is the clock authority')
    args = ap.parse_args()

    if not args.server.startswith(('http://', 'https://')):
        sys.exit('server must be an http(s) URL, e.g. http://100.101.102.103:8090')

    extra = os.environ.get('WATCHPARTY_MPV', '').split()
    c = Client(args.server, args.name, args.master, extra)
    threading.Thread(target=c.sse_loop, daemon=True).start()
    threading.Thread(target=c.poll_loop, daemon=True).start()
    try:
        c.terminal()
    except KeyboardInterrupt:
        c.shutdown()


if __name__ == '__main__':
    main()
