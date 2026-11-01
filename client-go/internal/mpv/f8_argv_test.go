package mpv

import (
	"slices"
	"testing"
)

// F8 pin: the client's base mpv argv must not force a video window at
// boot — the TUI paints first and the window appears at the first
// playback (human request #3). --idle stays (the IPC socket must exist
// before anything plays). WATCHPARTY_MPV extras keep appending LAST, so
// a user can still override every base flag (later flag wins).
func TestF8ArgvTUIFirst(t *testing.T) {
	args := mpvArgs("sock", nil)
	if args[0] != "--idle" {
		t.Fatalf("--idle must stay first (the IPC socket exists before anything plays): %v", args)
	}
	if slices.Contains(args, "--force-window") {
		t.Fatalf("--force-window still in the base argv — a video window pops up before the TUI: %v", args)
	}
	if !slices.Contains(args, "--input-ipc-server=sock") {
		t.Fatalf("IPC socket flag lost: %v", args)
	}

	extra := mpvArgs("sock", []string{"--force-window=yes", "--vo=null"})
	n := len(extra)
	if n < 2 || extra[n-2] != "--force-window=yes" || extra[n-1] != "--vo=null" {
		t.Fatalf("WATCHPARTY_MPV extras must ride last (later flag wins): %v", extra)
	}
}
