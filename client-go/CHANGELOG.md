# Changelog

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
