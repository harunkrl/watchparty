# Changelog

## Unreleased

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
