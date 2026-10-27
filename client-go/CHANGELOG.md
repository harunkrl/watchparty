# Changelog

## Unreleased

WAN livability (P0):

- follower: starvation-aware sync (F2 fix, live-measured) — a follower
  that stays behind the live position across two heartbeats (2 s apart)
  is starving: after one catch-up seek it stops seeking (a seek would
  flush the filling cache and re-stall every tick — the old seek-storm),
  the backlog shows in the drift chip, and the room is paused once so the
  cache can refill ("buffering - room paused" OSD; press play when ready —
  no auto-resume, a deliberate pause must never be fought). mpv's
  paused-for-cache counts as an instant stall signal
- client: an SSE blip no longer reloads the file on either side — hello only
  (re)loads when the room's file differs from what mpv already plays, and
  the hello seek is skipped when already within 1 s of the live position
  (mid-film download restarts and sub-second rewind flashes are gone)
- README: new "Watching over a WAN" section — uplink budget table,
  direct-vs-DERP check, faststart/MKV note
- CI: node test job (smoke + real-mpv integration/pause_sync/soak),
  `go test -race`, gofmt check; push paths now cover server.js and tests/
- server: room state persists across restarts (STATE_FILE, default
  state.json next to server.js; the systemd unit uses /var/lib/watchparty)
  — a mid-film server restart no longer empties the room; queue entries
  and now are dropped if the file vanished from disk meanwhile
- server: mid-body stream aborts are logged (`aborted <file> bytes ...`)
  — the first diagnostic for the rare both-streams-ended-early outlier
  seen once in live WAN runs
- follower: resume no longer jumps forward — the play event re-anchors
  the master snapshot, so the pause's duration cannot age into expected
  (spurious 1–3 s catch-up seek right after resuming)
- follower: paused-for-cache feeds the same stall runway as behind-live,
  ending the rare 2 s duplicate stall pause right after a resume

## v1.2.0 — slash commands, split panes

- commands now require the slash prefix (`/play`, `/queue`, `/pick`,
  `/list`, `/skip`, `/chat`, `/quit`, `/help`); plain text is ALWAYS chat —
  a sentence that merely starts with a reserved word can no longer fire a
  command. `//text` chats a literal leading slash; a lone `/` shows the
  command reference. The headless test harness keeps bare-verb parsing
  (cmd package unchanged, wire protocol unchanged)
- the chat pane is separated from machine output: chat is its own
  viewport, events and engine lines move to a 2-line dim "system" strip
  under a divider (run-collapse kept); command errors flash red on the
  input row (cleared by the next keystroke) instead of scrolling away

## v1.1.1 — chat-first input

- bare text at the prompt is sent to the room as chat — no "chat" prefix
  needed; the reserved verbs (list, pick, play, queue, skip, chat, quit,
  help) keep their command meaning and their parse errors ("play" without
  an argument errors instead of chatting the word). A line that merely
  STARTS with a verb still runs the command ("skip it" skips) — prefix
  "chat ..." for sentences that begin with a reserved word; headless
  harness parsing unchanged

## v1.1.0 — UI redesign

- A+ layout: two-line header (identity/connection with up-time and retry
  backoff, now playing + clock), full-width progress bar with elapsed/
  total (duration via bounded mpv retry), chat-first viewport, footer with
  queue preview and drift chip (green/amber/red), input on its own line
- engine log lines stay in the flow but dimmed and collapsed (+N marker);
  speed-nudge lines move to the drift chip instead of the chat flow
- long chat lines word-wrap aligned under the nick (truncate stays as the
  pathological-token fallback); 80x24 safe, single render path
- master's own play now reflects in the UI immediately (loadedMsg) — the
  server never echoes our own load back, so "now" used to stick at "-"
- pick overlay on ctrl+p in addition to the command; headless parity kept

## Unreleased (v1.0.x line)

Hardening and sync fixes after a two-agent review; wire protocol stays
backward compatible (hello gained optional `pos`/`p` fields).

- server: malformed percent-encoding in /v and /sub paths answers 400
  instead of crashing the whole process (a single `GET /v/%` killed it)
- server: suffix Range requests (`bytes=-N`) serve the last N bytes per
  RFC 9110 (previously wrong end offset or a spurious 416)
- server + clients: hello carries the live room position (age-adjusted)
  and paused flag; late joiners and reconnecting followers seek straight
  to it — also while the master is paused
- go: bundled Windows mpv.exe next to watchparty.exe starts again
  (Go 1.19+ ErrDot refused the cwd lookup the friend bundle relies on)
- go: subtitle probe uses a 5s timeout (a hung probe no longer stalls
  the sync engine)
- go: SSE stream has a keepalive-aware idle watchdog (45s) so silently
  dead connections reconnect instead of hanging forever
- tests: smoke covers bad encodings/ranges and hello pos; go covers the
  watchdog and the hello snapshot
- clients: dual control everywhere — the python follower now honors (and
  posts) pause/play/seek instead of having them reverted by the master's
  heartbeat; the go follower's own pause mirrors into its snapshot so a
  quick opposite-direction move is no longer swallowed
- clients: smooth-sync band — drift between 0.15 s and 1 s converges via a
  ±5 % tempo nudge (mpv audio-pitch-correction keeps it inaudible) instead
  of a visible re-seek; hard seeks stay beyond 1 s
- tests: pause_sync covers both directions (follower pauses/seeks, master
  follows) across the full interop matrix
- removed the python client — the Go binary is the only client (one engine
  to fix, not two; the interop matrix collapses to go x go). Guests get
  the release zips; the Windows one bundles mpv. Server no longer serves
  /client.py

## v1.0.0

First release of the Go TUI client. Server and wire protocol unchanged;
the Python client stays as v1 / master console / test driver.

- M0 toolchain + three-leg CI artifacts (windows/linux/darwin amd64)
- M1 room view: SSE connect, join/bye/chat, command parser, `list`
- M2 mpv playback: spawn + JSON IPC (unix socket / windows named pipe),
  `play`/`queue`/`skip`, hardened shutdown (TERM -> 2s -> Kill)
- M3 sync engine: master/follower parity with client.py, Clock DI,
  `--master`, `--headless`; interop matrix {python,go} x {master,follower}
- M4 polish: reconnect backoff (3..30s), drift status bar, cursor picker,
  chat OSD in mpv, colors
- M5 release: 5-platform artifacts, Windows bundle with portable mpv,
  README-friend.txt, tag-triggered release workflow

Server fix during development: 0-byte video files no longer crash the
streaming path (serveFile guard); protocol unchanged.
