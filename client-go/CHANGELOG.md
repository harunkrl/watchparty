# Changelog

## v1.3.1 — named actors, honest meters

- client + server: every room action names its actor — system lines read
  "pause by bo", the NOW PLAYING stat row says "⏸ paused by bo", and your
  own pause never inherits the previous pauser's name
- server: hello carries the invite address only the server can know —
  tailscale first, then the LAN (a labeled second address when both
  exist) — so the empty-room chat shows a URL the far side can dial
- server: skip and EOF auto-advance broadcast the queue, so a client
  never shows the playing file as queued
- server: relays stamp the actor nick from the connection roster only —
  a client-supplied nick can no longer spoof attribution
- client: the QUEUE box can no longer overflow the dashboard (a long
  queue truncates with "+N more"); its title counts the items and hints
  "/skip next"
- client: sparse conversations sit at the bottom of the chat pane, next
  to the input, in every layout; zen mode fades messages older than
  15 minutes into the background
- client: the dashboard's progress bar is clickable (the stacked one
  already was); the band tells you so
- client: the Tab menu opens on an empty input too (browse every command
  without knowing the slash), grows up to 16 rows, and never pushes the
  dashboard past the terminal
- client: NOW PLAYING carries an explicit "▶ playing" / "⏸ paused" chip,
  a personal subtitle chip ("⊞ you: sub tr [j in mpv]", never synced)
  and the system tail grows to 30 lines with an "n-line tail" label
- client: the equalizer is state-honest — frozen and amber while paused
  (a decaying bar claimed playback), labeled with the real volume and
  channel layout, and its border follows playback state
- docs: the known-limits and test sections match reality again

## v1.3.0 — honest signals, quiet sync

- client: movie mode (ctrl+z) grows a sync strip in the header — the file,
  the time left in it and a drift chip colored by state (green/amber/red).
  One number, colored, instead of a second value to interpret
- client: a file's duration is remembered — reloading it paints the
  progress bar instantly instead of probing the player again
- client: the stat row says "♪ muted" when the local player is muted, and
  the flag survives every pause transition (it used to blink off for a
  tick when the room paused)
- client: the system pane reports "reconnected after 42s" after an outage
- client: a follower whose master heartbeat went quiet shows an amber
  "⚠ master heartbeat stale" hint instead of a confident drift number
- client: the player is polled in ONE IPC round trip (was seven) — a tick
  costs a single write instead of seven request/response pairs
- client + server: `t` and `p` are always on the wire — a seek to 0 and a
  paused=false heartbeat used to vanish, and the room self-healed ~2 s
  later
- client: headless mode speaks the TUI's language — bare text goes to the
  room as chat (a line that starts with a reserved word is still that
  command)
- server: file and subtitle listings sort naturally ("movie2" before
  "movie10")
- server: skipping or ending with nothing playing explains itself instead
  of silently doing nothing
- server: the master reports the media duration with its heartbeat, so a
  joiner returning after a long downtime lands before the end of the film
- server: `GET /library` lists the videos with their subtitle siblings
- server: `GET /health` reports `{now, queueLen, listeners}` for watchdog
  and manual checks

Room quality (P2):

- TUI dashboard: terminals of 100+ columns get a two-column layout —
  now-playing (bar, drift, pause state), watchers (join/leave list with
  offline tags), queue and the system log on the left; the conversation
  and a boxed input on the right; a hotkey hint bar at the bottom.
  Narrower terminals keep the stacked layout. New room-wide hotkeys:
  ctrl+space toggles play/pause, ctrl+arrows nudge ±5 s — both apply
  locally and post, because the server never echoes a poster's own
  events back
- TUI overhaul: chat lines carry a timestamp gutter and group under one
  nickname while consecutive; the system pane sits behind a thin left
  gutter so conversation pops; join/left/notice lines are color-coded;
  the seek bar gains a position head plus elapsed/total labels; the
  connecting screen got a spinner and an empty room shows the invite
  address to send. New keys: Tab completes commands and file names,
  ctrl+z is movie mode (one status line + chat + input), ctrl+g toggles
  the system pane, and a mouse click on the seek bar seeks the room
  (mouse is now enabled — hold shift to select terminal text)
- releases: the bundled mpv is pinned to a dated shinchiro build
  (MPV_TAG in the release workflow) and the download is verified against
  the GitHub asset digest — a repackaged/tampered archive fails the
  build; every release zip ships with a SHA256SUMS file (PowerShell:
  Get-FileHash) and the release title names the mpv build inside
- subtitles: all language siblings (`Movie.tr.srt`, `Movie.es.srt`, ...)
  load as selectable mpv tracks (new server `/subs` listing); each person
  switches with `j`/`J` in the player window — the plain `Movie.srt`
  stays the default selection, and subtitle choice never enters the sync
  layer
- chat history: the last 50 messages replay once to each NEW connection
  (late joiners see what they missed; same-id SSE reconnects do not
  re-receive them; in-memory only — a server restart clears it)

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
- server: single-master election — the first client to post a heartbeat
  owns the clock; a second --master's heartbeats (and EOF auto-advance)
  are dropped with a one-time room notice rendered in the TUI and on the
  video OSD. The claim dies with the master's connection, so restarts and
  failures succeed cleanly — the two-master seek duel is gone
- client: a notice naming our id demotes a running second --master to a
  follower (log + OSD); notices pass the client's own-echo filter so the
  named client actually receives them
- client: mpv's head-of-file seek event after a load is never posted to
  the room (5 s post-load quiet window) — closes the late-join rewind
  race and a demoted impostor's spurious rewind; real scrubs inside the
  window self-heal via the master heartbeat

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
