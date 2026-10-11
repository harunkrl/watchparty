# Watchparty Go Client — Final Development Plan (v2, consensus)

Goal: replace the friend-facing experience with a single-binary TUI client
(Go + Bubble Tea), keeping the server and wire protocol 100% unchanged.
The Python client stays as v1 / master console / test driver.
Two new sessions (jelly-2 implementer, jelly-3 reviewer) start from this file.

## 0. Fixed constraints

- Zero transcode: video plays in external mpv, spawned and controlled by us.
- Server: unchanged (node, HTTP + SSE + JSON). Protocol frozen as-is.
- Tailnet-only, no auth. English-only code/docs/UI.
- Friend UX bar: unzip, double-click, type name, watch. One-time SmartScreen /
  Gatekeeper bypass accepted (no code signing).

## 1. Protocol contract (frozen — fixed numbers)

HTTP:
- `GET /files` -> JSON array of names
- `GET /events?id=&nick=` -> SSE, `data: {json}\n\n`; events: hello, join, bye,
  chat, load, play, pause, seek, position {t, p}, queue
- `POST /cmd` -> {type, ...}: chat, load, play, pause, seek, position,
  enqueue, ended, skip
- `GET /client.py` stays (python fallback); Go binary ships via release zip.

Fixed numbers (Python parity; Go takes them verbatim):
- POLL_SECS = 2 (master mpv poll period)
- TOLERANCE = 1.0 (follower drift threshold)
- SEEK_SCRUB_THRESHOLD = 3 (|pos - last_pos - POLL_SECS| > 3 -> seek event)
- MPV_DEMUXER_READAHEAD_SECS = 300 (LAN default; user-overridable)
- MPV_DEMUXER_MAX_BYTES = 2GiB
- CHAT_MAX_LEN = 500 (client-side guard; server does not limit)
- Reconnect backoff: min(30, 3 * 2^min(attempt,4))

Sync semantics (parity with Python client):
- master: discrete events on transitions, position {t, p} heartbeat every
  POLL_SECS, seek event when scrub > SEEK_SCRUB_THRESHOLD, ended on
  idle-after-active.
- follower: discrete events immediate; heartbeat self-heal converges pause
  state to the master's `p`; drift correction seeks when
  |pos - expected| > TOLERANCE; expected never grows while the master is
  paused (no age growth).

position JSON fields: `{t: float, p: bool}` — Go struct tag `json:"p"`.

## 2. Architecture — 4 channels + main.go injection (no event bus)

    client-go/
      main.go                flags: URL, --name, --master, --headless
      go.mod                 module watchparty/client-go; go 1.22
      internal/cmd/          command parser: list|play|queue|skip|chat|quit|help|pick
      internal/room/         SSE client + POST /cmd (room.Stream)
      internal/mpv/          spawn mpv; IPC transport: unix socket / named pipe
                             (go-winio on windows); poller (POLL_SECS)
      internal/sync/         sync engine (master+follower); pure (no UI deps);
                             Clock interface DI (Now, Since)
      internal/ui/           Bubble Tea model (headless: stdin/stdout adapter)

Channels (created in main.go, dependency-injected):
```
chRoom := make(chan room.Event, 16)      // SSE -> sync + ui
chMpv  := make(chan mpv.State, 8)        // mpv poller -> sync
chUi   := make(chan sync.UiState, 8)     // sync -> ui (drift, peers, connState)
post   := func(cmd any) error { ... }    // sync -> server (POST /cmd)
```

Headless mode: a separate adapter subscribing to the same channels
(stdin/stdout). The Go counterpart of pause_sync.mjs drives the identical
engine. Test mocks: fake events in/out on the channels + fake Clock.

## 3. TUI layout

    ┌ status: film · 12:34 · room: sen, partner · ● synced (0.4s drift) ┐
    │ [partner] bu sahne harika                     (chat + room log,  │
    │ > partner joined                                scrollable)      │
    ├────────────────────────────────────────────────────────────────────┤
    │ › play 2_                                            (input line) │
    └────────────────────────────────────────────────────────────────────┘

- Status bar variant: master = `synced to N listeners`, follower = `drift 0.4s`.
- bubbles: textinput (input + history up/down), viewport (log), lipgloss borders.
- Esc clears input. Ctrl+C -> orderly shutdown (see risk C).
- Incoming events update panes without stealing focus (core Bubble Tea pattern
  — the reason for the rewrite).
- The mpv window stays separate; the TUI is the control surface.

## 4. Distribution matrix

| Platform      | Binary             | mpv source                | CGO | Tested             |
|---------------|--------------------|---------------------------|-----|--------------------|
| Linux amd64   | static (CGO=0)     | system: apt/pacman/dnf    | no  | YES (NixOS, CachyOS) |
| Linux arm64   | static             | system: apt/dnf           | no  | artifact only      |
| Windows amd64 | exe + portable mpv | shinchiro winbuild        | —   | YES (the friend)   |
| macOS amd64   | exe                | system: brew              | no  | best-effort        |
| macOS arm64   | exe                | system: brew              | no  | best-effort        |

CachyOS note: mpv via pacman (`sudo pacman -S mpv`); binary built with
CGO_ENABLED=0 — zero runtime deps. M0 validates on three legs:
Windows (friend) + NixOS + CachyOS (user).

CI: GitHub Actions matrix (5-platform build artifacts). "Tested" = Windows +
Linux amd64 (the three legs). macOS best-effort artifacts (user has no Mac).
Plain `go build` to start; goreleaser optional (YAGNI). No auto-update (YAGNI).

## 5. Milestones (time-targeted)

- **M0** (target 0.5h, hard cap 2h): Bubble Tea hello-world TUI + CI builds
  three artifacts (windows/amd64, linux/amd64, darwin/amd64). Friend runs it
  in Windows Terminal + conhost. De-risks toolchain + SmartScreen/Gatekeeper
  prompts. Three-leg validation is the M0 exit criterion.
- **M1** (~200 LOC, half session): room view — SSE connect, join/bye/chat
  visible, input parser + help. `list` prints file names.
  Test: live server + scripted SSE observation.
- **M2** (~250 LOC, half session): mpv spawn (verify Go default stdin =
  os.DevNull), IPC (Unix socket / named pipe via go-winio), `play <name>`.
  Test: Linux real mpv; friend smoke zip.
- **M3** (~300 LOC product + ~200 LOC tests, one session): sync engine parity
  + Clock DI, --headless mode, **interop matrix {python,go} x {master,follower}**.
  Scenarios: pause/resume < 3s, no oscillation, seek-to-60s converges follower
  into [59,65], resume after pause.
- **M4** (~300 LOC, one session): polish — picker mode (cursor list; `list`
  text command stays), chat OSD via mpv show-text, reconnect backoff, status
  bar (drift + peers + connection state), colors.
- **M5** (CI yaml + bundle script + README, 2-3h): release 1.0, Actions matrix,
  README-friend.txt (3 lines: unzip, double-click, type name, watch; one-time
  SmartScreen "Run anyway" / Gatekeeper "Open Anyway" note).

Total: ~1100-1500 LOC, 3-4 focused sessions.

## 6. Test strategy

- Unit: command parser (table-driven), sync engine (table-driven, fake Clock).
- Integration (node harness, pause_sync.mjs pattern): Go client --headless +
  real local server + real mpv; assert via SSE observation + a second IPC
  connection into mpv.
- Interop matrix (M3): {python, go} x {master, follower} — every combination
  runs the pause/resume/seek scenarios. pause_sync.mjs is reused and extended
  with a --engine go option.
- No HTTP mocking (spin the real server; it is 150 LOC).

## 7. Decision ledger (open questions closed)

| #  | Topic                        | Decision                                              | Rationale                       |
|----|------------------------------|-------------------------------------------------------|---------------------------------|
| Q1 | observe-property vs polling  | M3 polling; observe-property in M4 only if measurement justifies | Parity; TOLERANCE=1s hides 2s poll latency |
| Q2 | Windows mpv source           | always-bundled; Linux/mac system PATH                 | Deterministic + easy            |
| Q3 | file picker vs list          | M1 `list` text; M4 cursor picker                      | MVP first, polish later         |
| Q4 | chat multiline               | single-line, 500-char client-side limit               | YAGNI; bubbles textarea is a separate component |
| Q5 | go-winio vs x/sys/windows    | go-winio                                              | Cross-platform, ~50 LOC, stdlib-like |
| Q6 | macOS build                  | yes, best-effort artifacts (amd64+arm64)              | Trivial Go cross-compile        |
| E1 | event bus vs channels        | 4 channels + main.go injection; NO bus package        | Go-idiomatic; bus is a god-object risk |
| E2 | module path                  | `watchparty/client-go` (local); rename if published   | First-commit decision           |
| E3 | go version                   | 1.22+                                                 | Modern stdlib                   |
| E4 | clock DI                     | Clock interface (Now, Since); fake clock in tests     | Instant 60s simulations         |
| E5 | CachyOS distribution         | static binary (CGO_ENABLED=0), `pacman -S mpv`        | Arch-family friendly            |

## 8. Risks (A-I, placed)

- **A** Go stdlib SSE quirks (bufio line reader, skip `:` comments, defer
  close, ctx cancel) -> exercised in M1 against real pings + data.
- **B** Bubble Tea Windows quirks (no mouse-motion, no altscreen initially)
  -> de-risked in M0 on three legs.
- **C** Process cleanup (Go counterpart of Python shutdown(): SIGTERM ->
  2s wait -> Kill; one context cancel stops all goroutines) -> unit-tested in
  M2; orphaned mpv must be impossible (we cleaned two orphan outbreaks in
  this project already).
- **D** stdin=DEVNULL: Go's default when cmd.Stdin is nil is already
  os.DevNull -> verify once in M2, no extra code.
- **E** macOS Gatekeeper (`xattr -d com.apple.quarantine watchparty` or
  Privacy & Security -> Open Anyway) -> README-friend note, best-effort.
- **F** module path -> E2.
- **G** fake clock -> E4.
- **H** orderly shutdown: context cancel FIRST, then tea.QuitMsg; headless
  exits on Ctrl+D / stdin EOF -> implemented in M2.
- **I** monorepo: subdir `client-go/`, no separate repo (jelly-2's call).

## 9. Handoff — new sessions

New jelly-2 (implementer) session:
- Working set: this plan + server.js + client.py + tests/
- First commit: M0 toolchain + three-leg validation
- Per milestone: code -> tests -> diff -> implementer cap
- Protocol conformance = pause_sync + interop matrix green
- Interpret the spec; free improvisation is YAGNI

New jelly-3 (reviewer) session:
- LOC delta + diff review + per-item semantic checks
- Interop matrix results (every combination)
- Runtime: `nix-shell -p mpv ffmpeg go --run "node tests/*.mjs"`

Deployment on pve is out of scope for this project — server.js / client.py
stay untouched.
