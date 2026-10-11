# watchparty

Watch locally stored videos together in sync, at original quality, over a
Tailscale network. One machine hosts the files; everyone else streams the
original file byte-for-byte (no transcoding, no re-encoding) while a small
sync engine keeps all players on the same clock.

Built for two people who are tired of Discord/Telegram video quality.

## How it works

- `server.js` (Node, stdlib only): serves a videos folder over HTTP with
  Range support (`/v/<file>`), plus subtitles (`/sub/<file>`), and relays
  play/pause/seek/chat/queue events between clients over SSE.
- `client.py` (Python, stdlib only): spawns `mpv`, connects to its JSON IPC
  socket, and keeps it in sync with the room. Exactly one client runs with
  `--master` (the host); its player is the clock authority. Followers apply
  the master's state and re-seek when drift exceeds ~1s.
- Quality is preserved by never touching the video: mpv decodes the original
  stream. mpv buffers up to 5 minutes ahead, so upload hiccups don't stall
  playback.

Tailnet-only by design: **no auth, no TLS. Do not port-forward this.**

## Requirements

- Host: [Node](https://nodejs.org) 18+, videos in a folder, [Tailscale](https://tailscale.com)
- Guests: Python 3.9+, `mpv`, Tailscale
- On NixOS: `nix-shell -p mpv` (or add mpv to your profile)

## Quickstart

Host (the machine with the videos):

```sh
VIDEOS_DIR=~/Videos node server.js
python3 client.py http://localhost:8090 --master --name ada
```

Guest (e.g. in Spain):

```sh
python3 client.py http://<your-tailscale-ip>:8090 --name bo
```

Terminal commands in the client:

| Command | Effect |
|---|---|
| `list` | list videos on the server |
| `play <file>` | start playing a file for everyone (anyone can call it) |
| `queue <file>` | append to the shared queue; auto-plays when the current file ends |
| `skip` | jump to the next file in the queue (same as the current file ending) |
| `chat <msg>` | chat; shown in the other side's terminal and as an mpv OSD |
| `quit` | exit |

You get a notice in the terminal when the other side joins or leaves.
`skip` is a silent no-op when the queue is empty (use `list` to see what is queued).

Voice: keep a Discord/WhatsApp call open alongside.

## Subtitles

Drop a `.srt` or `.ass` next to the video with the same basename
(`Movie.mkv` + `Movie.srt`). The client attaches it automatically for
everyone.

## Config

| Env var | Default | Meaning |
|---|---|---|
| `VIDEOS_DIR` | `./videos` | folder the server exposes (flat, no subdirs) |
| `PORT` | `8090` | server port |
| `WATCHPARTY_MPV` | — | extra mpv flags for the client, e.g. `"--vo=null"` |

## Test

```sh
node tests/smoke.mjs   # spawns its own server on a random port
nix-shell -p mpv ffmpeg --run "node tests/integration.mjs"   # real-mpv Phase A (skips if mpv/ffmpeg missing)
nix-shell -p mpv ffmpeg --run "node tests/pause_sync.mjs"   # two real clients: pause/resume/seek sync
```

## Deployment (homelab)

Runs as a systemd service on the PVE host (`/opt/watchparty`), reachable on LAN
and tailnet only — enforced in-kernel by the unit's IP filters, not just by the
absence of port-forwarding:

```ini
# /etc/systemd/system/watchparty.service
[Unit]
Description=Watchparty video server (tailnet-only)
After=network.target

[Service]
ExecStart=/usr/bin/node /opt/watchparty/server.js
Environment=VIDEOS_DIR=/opt/watchparty/videos
Environment=PORT=8090
Restart=always
RestartSec=2
DynamicUser=yes
NoNewPrivileges=yes
ProtectSystem=strict
ProtectKernelTunables=yes
ProtectControlGroups=yes
ReadOnlyPaths=/opt/watchparty
PrivateTmp=yes
IPAddressAllow=localhost 100.64.0.0/10 192.168.1.0/24
IPAddressDeny=any

[Install]
WantedBy=multi-user.target
```

Add a video: `scp file.mp4 root@<host>:/opt/watchparty/videos/` — visible in
`list` immediately, no restart. Update code: `scp server.js client.py` into
`/opt/watchparty`, then `systemctl restart watchparty`.

Friends fetch the client from the server itself:
`curl -O http://<tailnet-ip>:8090/client.py`

## Known limits

- One flat folder; filenames must be identical everywhere (they are, there is one source).
- One master; if the master's player dies, restart the client and `play` again.
- Chat is fire-and-forget; there is no history.
- Windows client (named-pipe IPC) is untested; treat it as best-effort.
- Browsers are unsupported by design: generic Content-Type, no CORS — use mpv.
- Follower drift correction is covered by `tests/pause_sync.mjs` (pause, resume,
  seek convergence); SSE reconnect replay is not — a follower that reconnects
  catches up within one heartbeat (~2 s) via position convergence.
