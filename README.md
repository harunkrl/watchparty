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
- `client-go` (Go, single binary): a TUI that spawns `mpv`, connects to its
  JSON IPC socket, and keeps it in sync with the room. Exactly one client
  runs with `--master` (the host); its player is the clock authority.
  Followers apply the master's state, micro-adjust speed on small drift and
  re-seek when drift exceeds ~1s.
- Quality is preserved by never touching the video: mpv decodes the original
  stream. mpv buffers up to 5 minutes ahead, so transient upload dips don't
  stall playback; a link that cannot sustain the file's bitrate will (see
  "Watching over a WAN" below).

Tailnet and home LAN only by design: **no auth, no TLS. Do not port-forward this.**

## Requirements

- Host: [Node](https://nodejs.org) 18+, videos in a folder, [Tailscale](https://tailscale.com)
- Guests: the watchparty binary ([release zips](../../releases); the Windows
  zip bundles a portable mpv — nothing else to install) + Tailscale
- Building from source: Go 1.26+ (`cd client-go && go build`)
- On NixOS: `nix-shell -p mpv` (or add mpv to your profile)

## Quickstart

Host (the machine with the videos):

```sh
VIDEOS_DIR=~/Videos node server.js
client-go/watchparty --master --name ada http://localhost:8090
```

Guest (e.g. in Spain): unzip the release, then either double-click the
binary (it asks for the server URL) or:

```sh
./watchparty --name bo http://<your-tailscale-ip>:8090
```

Press `F1` in the client for the full key sheet (pause, seek, volume,
mute, the file picker) — every other key keeps working while it is open.

Everything you type goes to the room as chat. Commands start with `/`:

| Command | Effect |
|---|---|
| `/list` | list videos on the server |
| `/pick` | cursor list: browse and play/queue with the arrow keys |
| `/play` | open the cursor list in play mode (enter plays, `q` queues) |
| `/play <file>` | start playing a file for everyone (anyone can call it) |
| `/queue` | open the cursor list in queue mode (enter queues) |
| `/queue <file>` | append to the shared queue; auto-plays when the current file ends |
| `/sub` | list the subtitle files that belong to the current video |
| `/sub <name>` | attach that subtitle to the current video — no reload, only your screen |
| `/subdelay <±sec>` | shift your subtitle timing (`+0.35` delays subs, `0` resets) — local only |
| `/skip` | jump to the next file in the queue (same as the current file ending) |
| `/quit` | exit |

These slash commands are TUI features — the headless adapter (the test
harness) only knows the plain verbs (`list`, `play`, …) and sends
anything else to the room as chat.

The TUI has a few shortcuts beyond the slash commands (wide terminals
get a two-column dashboard: now-playing, watchers, queue and system log
on the left; chat and input on the right):

| Key | Effect |
|---|---|
| `↑` / `↓` | recall what you typed before (input history) |
| `Tab` | complete `/commands` and file names |
| `ctrl+p` | pick a film: browse and play/queue with the arrow keys |
| `ctrl+space` | play/pause for the whole room |
| `ctrl+←` / `ctrl+→` | seek everyone ∓5 s / ±5 s |
| `ctrl+shift+←` / `ctrl+shift+→` | seek everyone ∓30 s / ±30 s (whole-movie jumps) |
| `ctrl+↑` / `ctrl+↓` | your own volume ±5 % — local, the room never hears it |
| `alt+p` | play/pause for the whole room (where `ctrl+space` does not arrive) |
| `alt+←` / `alt+→` | seek everyone ∓5 s / ±5 s (same as `ctrl+←/→`) |
| `alt+↑` / `alt+↓` | your own subtitle timing ±0.5 s — local, the room never hears it |
| `alt+m` | mute your own player — local, the room never hears it |
| `F1` | a keys sheet: every other key keeps working while it is open |
| `ctrl+z` | movie mode: collapse to one status line + chat + input |
| `ctrl+g` | hide/show the system pane (join/leave/sync chatter) |
| mouse wheel | scroll the chat |
| click on the bar | seek everyone there (stacked layout) |

While the file list is open it owns the keys: `↑`/`↓` move, `enter` does
what the list was opened for (play or queue), `q` queues, `esc` closes it.
The `ctrl+` shortcuts above are inert until it closes.

Two terminals eat shortcuts before the app sees them: tmux uses `ctrl+z`
for suspend (so movie mode is unreachable there without rebinding it),
and the old Windows console may not deliver `ctrl+space` as a control
key. The `alt+` aliases exist for that case: `alt+p` pauses the room,
`alt+m` mutes, `alt+←/→` seeks — or use Windows Terminal. (The space bar
in the mpv window toggles your own player, not the room.)

Note: with mouse on, terminal text selection needs `shift` held.

You get a notice in the terminal when the other side joins or leaves.
`/skip` with an empty queue says so in the input row instead of doing
nothing (use `/list` to see what is queued).

Controls are shared: **either side** can pause (space in the mpv window),
seek and pick a new file — the master's heartbeat keeps everyone converged.
A follower with small drift (< 1 s) micro-adjusts playback speed (±5 %,
inaudible) instead of re-seeking, so convergence is smooth.

Voice: keep a Discord/WhatsApp call open alongside.

## Subtitles

Drop `.srt`/`.ass` files next to the video with the same basename
(`Movie.mkv` + `Movie.srt`). The client attaches it automatically for
everyone.

Language siblings (`Movie.tr.srt`, `Movie.es.srt`, ...) all load as
selectable tracks: each person presses `j` (cycle) or `J` (reverse) **in
the mpv window** to pick their own language — subtitle choice is local,
it never touches the sync layer. The plain `Movie.srt` (if present) is
the default selected track.

Chat is kept for late joiners too: the last 50 messages replay once to
new connections (an SSE blip reconnect does not re-print them; a server
restart clears history).

## Config

| Env var | Default | Meaning |
|---|---|---|
| `VIDEOS_DIR` | `./videos` | folder the server exposes (flat, no subdirs) |
| `PORT` | `8090` | server port |
| `WATCHPARTY_MPV` | — | extra mpv flags for the client, e.g. `"--vo=null"` |
| `STATE_FILE` | `state.json` next to `server.js` | where the room state persists across restarts |
| `SSE_MAX_BUFFER` | `4194304` (4 MB) | per-client SSE write backlog cap — a slower client is cut off instead of growing server memory |

`GET /health` answers `{"now":<file|null>,"queueLen":<n>,"paused":<bool>,
"listeners":<n>}` — a cheap diagnostic probe (curl only, no SSE join
needed). It carries room state, never a name or an id.

## Test

```sh
node tests/smoke.mjs   # spawns its own server on a random port
cd client-go && go build && go vet ./... && go test -race ./... && go test -tags audit3 ./internal/... && cd ..   # the mpv tests drive this binary
nix-shell -p mpv ffmpeg --run "node tests/integration.mjs"   # real-mpv Phase A (skips if mpv/ffmpeg missing)
nix-shell -p mpv ffmpeg --run "node tests/pause_sync.mjs"   # two real clients: pause/resume/seek sync, both directions
nix-shell -p mpv ffmpeg --run "node tests/soak.mjs"         # both sides act; no echo storms, convergence holds
nix-shell -p mpv ffmpeg --run "node tests/master_restart.mjs" # the room survives a master restart
node tests/persist.mjs   # room state survives a server restart
node tests/audit_server.mjs && node tests/audit2_server.mjs && node tests/audit3_server.mjs   # adversarial audit pins
node tests/f1_server.mjs   # v1.3.3 server-hardening pins
nix-shell -p mpv ffmpeg --run "node tests/f2_server.mjs"   # claim freshness + mechanical relay (real-client E2E)
nix-shell -p mpv ffmpeg --run "node tests/stall_family.mjs"  # silent master / resume / mechanical pause
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
Environment=STATE_FILE=/var/lib/watchparty/state.json
StateDirectory=watchparty
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
`list` immediately, no restart. Update code: `scp server.js` into
`/opt/watchparty`, then `systemctl restart watchparty`. The room (current
file, queue, position) survives that restart: state persists to `STATE_FILE`
(`/var/lib/watchparty/state.json` in the unit above).

Guests get the client from the GitHub releases page (tag a version and CI
builds the zips, Windows one with mpv bundled).

## Watching over a WAN (long distance)

Zero-transcode means the file's own bitrate must fit the host's upload —
the guest streams over it, while the host watches over the LAN for free:

| Host upload | Safe video bitrate | Fits |
|---|---|---|
| 10 Mbps | ~6–7 Mbps | streaming-grade 1080p encodes |
| 25 Mbps | ~17–18 Mbps | most 1080p encodes |
| 40 Mbps | ~27–30 Mbps | 1080p remux; UHD remux (50–80 Mbps) never |

Keep ~1 Mbps of headroom for the voice call running alongside; the sweet
spot is a 1080p HEVC/AV1 encode in the 8–15 Mbps range.

- Before movie night, check `tailscale status` shows a **direct**
  connection to the guest. A DERP relay caps throughput at a few Mbps and
  will not carry a high-bitrate file.
- If the guest's link cannot sustain the bitrate, the follower falls
  behind: after one catch-up attempt the client stops seeking, shows the
  backlog in the drift chip and pauses the room once — mechanically,
  labeled "paused (stall)" so nobody gets blamed — so the cache can
  refill. Press play when ready. This only fires while the master's
  heartbeat is fresh: a silent master never pauses the room. A resume
  jump (sleep, laptop lid, an outage — reconnect or not) re-seeks once
  with a settle window that follows the cache instead of pausing; a
  link that is genuinely starving still stalls, bounded by a ~2-minute
  backstop.
- Prefer MKV, or faststart MP4s (`ffmpeg -i in.mp4 -c copy -movflags
  +faststart out.mp4`): an MP4 with its index (moov) at the end must be
  fetched from the far end of the file before playback starts, which can
  time out on a slow link.

## Known limits

- One flat folder; filenames must be identical everywhere (they are, there is one source).
- One master; if the master's player dies, restart the client and `play` again.
  A second `--master` cannot duel for the clock: the server keeps the first
  heartbeat sender, drops the other's heartbeats and warns the room; the
  claim dies with the master's connection — or after 45 s without a
  heartbeat post — so restarts succeed cleanly. A master that dies
  silently no longer freezes the room: once the claim goes stale
  (~45 s after the last heartbeat) — or the master leaves cleanly —
  a follower whose file ended advances the queue; late joiners after
  the master dies or leaves land where the film actually stopped (in
  the room's own paused state), not extrapolated to its end.
- Chat history is in-memory only: the last 50 messages replay to each new
  joiner; a server restart clears it.
- Windows client (named-pipe IPC) is supported; use the release bundle
  zip (it ships mpv). If the dashboard glyphs look garbled, run it in
  Windows Terminal.
- Linux/macOS terminals only: at startup the TUI asks the terminal for
  its background color. Tty-less sessions and TERM=dumb/screen/tmux
  skip the query; a foreground terminal that never answers it (some
  serial/minimal IDE terminals) stalls ~5 s and can swallow the first
  typed line. Use a real terminal emulator.
- Browsers are unsupported by design: generic Content-Type, no CORS — use mpv.
- Follower drift correction is covered by `tests/pause_sync.mjs` (pause, resume,
  seek convergence). A reconnecting or late-joining client is seeked straight to
  the live room position: hello carries the age-adjusted master position and
  paused flag (works while the master is paused too).
