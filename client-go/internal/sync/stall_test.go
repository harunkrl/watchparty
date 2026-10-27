package sync

// R1/R2 unit tests: follower stall handling (behind-live starvation
// detector + paused-for-cache) and hello dedup. Fake player/clock come
// from engine_test.go (same package).

import (
	"testing"
	"time"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
)

// pastGrace returns a fake clock whose Since() clears every grace window.
// With masterAt(12, false) the expected live position is 12+10 = 22.
func pastGrace() fakeClock { return fakeClock{now: time.Unix(2000, 0), since: 10 * time.Second} }

// TestStallNoSeekStorm: a follower persistently behind live gets ONE probe
// seek; if it lands behind again it stalls — room paused exactly once, no
// further seeks. (Old behavior: a seek every 2s tick — the seek-storm.)
func TestStallNoSeekStorm(t *testing.T) {
	e, pl, po := newEngine(false)
	e.OnRoom(masterAt(12, false)) // playing master snapshot
	e.Clock = pastGrace()

	e.OnMpvp(mpv.State{Pos: ptr(22), Pause: false}) // baseline: on live
	e.OnMpvp(mpv.State{Pos: ptr(20), Pause: false}) // behind 1: probe seek, no pause
	if n := countType(t, po.events, "pause"); n != 0 {
		t.Fatalf("paused before stallTicks: %v", evType(t, po.events))
	}
	if len(pl.seeks) != 1 {
		t.Fatalf("probe seek: got %v, want exactly 1", pl.seeks)
	}

	e.OnMpvp(mpv.State{Pos: ptr(20), Pause: false}) // behind 2: stall -> one pause, no seek
	if n := countType(t, po.events, "pause"); n != 1 {
		t.Fatalf("pause posted %d times, want 1 (events: %v)", n, evType(t, po.events))
	}

	// further frozen ticks: no repeat pause, no more seeks
	e.OnMpvp(mpv.State{Pos: ptr(20), Pause: false})
	e.OnMpvp(mpv.State{Pos: ptr(20), Pause: false})
	if n := countType(t, po.events, "pause"); n != 1 {
		t.Fatalf("auto-pause repeated: %v", evType(t, po.events))
	}
	if len(pl.seeks) != 1 {
		t.Fatalf("seek-storm during stall: %v", pl.seeks)
	}
}

// TestBufferingFlagStallsInstantly: mpv's paused-for-cache counts as a
// stall on the very first tick (no behind-live runway needed).
func TestBufferingFlagStallsInstantly(t *testing.T) {
	e, pl, po := newEngine(false)
	e.OnRoom(masterAt(12, false))
	e.Clock = pastGrace()

	e.OnMpvp(mpv.State{Pos: ptr(22), Pause: false, Buffering: true})
	if n := countType(t, po.events, "pause"); n != 1 {
		t.Fatalf("buffering flag did not stall: %v", evType(t, po.events))
	}
	if len(pl.seeks) != 0 {
		t.Fatalf("seek during buffering: %v", pl.seeks)
	}
}

// TestShortStallNoPause: one behind-live tick (hiccup, probe seek) then
// recovery — no pause is posted and normal drift handling resumes.
func TestShortStallNoPause(t *testing.T) {
	e, pl, po := newEngine(false)
	e.OnRoom(masterAt(12, false))
	e.Clock = pastGrace()

	e.OnMpvp(mpv.State{Pos: ptr(22), Pause: false}) // baseline
	e.OnMpvp(mpv.State{Pos: ptr(20), Pause: false}) // hiccup: behind 1 -> probe seek
	if n := countType(t, po.events, "pause"); n != 0 {
		t.Fatalf("pause on a short hiccup: %v", evType(t, po.events))
	}

	// recovers to within the smooth band -> speed nudge, behind reset
	e.OnMpvp(mpv.State{Pos: ptr(21.7), Pause: false})
	if n := countType(t, po.events, "pause"); n != 0 {
		t.Fatalf("pause after short stall: %v", evType(t, po.events))
	}
	if len(pl.speeds) == 0 {
		t.Fatalf("normal drift handling did not resume: speeds=%v seeks=%v", pl.speeds, pl.seeks)
	}
	if len(pl.seeks) != 1 {
		t.Fatalf("expected only the probe seek: %v", pl.seeks)
	}
}

// TestHelloBlipNoReload: a hello for the file we already play, with our own
// position within Tolerance of the room position, must not reload or seek —
// it only refreshes the snapshot (F3: SSE blip must not restart a download).
func TestHelloBlipNoReload(t *testing.T) {
	e, pl, _ := newEngine(false)
	e.OnRoom(room.Event{Type: "load", Name: "x.mkv"})
	pl.pos = ptr(30.5) // ~0.5s from the hello position: inside Tolerance

	p := 30.0
	e.OnRoom(room.Event{Type: "hello", Now: "x.mkv", Pos: &p})
	if len(pl.loads) != 1 {
		t.Fatalf("hello blip reloaded the file: loads=%v", pl.loads)
	}
	if len(pl.seeks) != 0 {
		t.Fatalf("hello blip seeked: %v", pl.seeks)
	}
	if e.snap == nil || e.snap.t != 30.0 {
		t.Fatalf("hello did not set snap: %+v", e.snap)
	}
}

// TestHelloDifferentFileLoads: hello carrying another file must reload (x→y).
func TestHelloDifferentFileLoads(t *testing.T) {
	e, pl, _ := newEngine(false)
	e.OnRoom(room.Event{Type: "load", Name: "x.mkv"})
	e.OnRoom(room.Event{Type: "hello", Now: "y.mkv"})
	if len(pl.loads) != 2 || pl.loads[0] != "x.mkv" || pl.loads[1] != "y.mkv" {
		t.Fatalf("loads = %v, want [x.mkv y.mkv]", pl.loads)
	}
}

// TestOwnLoadPreventsHelloReload (F3 master half): the local user's own
// /play drives mpv directly; OwnLoad records it so a later hello for the
// same file must not reload it.
func TestOwnLoadPreventsHelloReload(t *testing.T) {
	e, pl, _ := newEngine(true) // the master is the affected side
	e.OwnLoad("x.mkv")
	e.OnRoom(room.Event{Type: "hello", Now: "x.mkv"})
	if len(pl.loads) != 0 {
		t.Fatalf("hello reloaded the own-played file: loads=%v", pl.loads)
	}
}

// TestStallPauseResetsOnPlay: after an auto-pause, a room play clears
// autoPaused and the stall window, so a NEW stall may pause again — but
// only after the full behind-live runway (no instant re-pause).
func TestStallPauseResetsOnPlay(t *testing.T) {
	e, pl, po := newEngine(false)
	e.OnRoom(masterAt(12, false))
	e.Clock = pastGrace()

	e.OnMpvp(mpv.State{Pos: ptr(22), Pause: false}) // baseline
	e.OnMpvp(mpv.State{Pos: ptr(20), Pause: false}) // behind 1 -> probe seek
	e.OnMpvp(mpv.State{Pos: ptr(20), Pause: false}) // behind 2 -> pause #1
	if n := countType(t, po.events, "pause"); n != 1 {
		t.Fatalf("first stall pause missing: %v", evType(t, po.events))
	}

	e.OnRoom(room.Event{Type: "play"}) // room resumed: stall-pause is spent

	// resume straight into a dry cache: play reset the window, so the
	// follower needs the full runway again (no instant re-pause)
	e.OnMpvp(mpv.State{Pos: ptr(22), Pause: false}) // window reset
	if n := countType(t, po.events, "pause"); n != 1 {
		t.Fatalf("instant re-pause after play: %v", evType(t, po.events))
	}
	e.OnMpvp(mpv.State{Pos: ptr(20), Pause: false}) // behind 1 -> probe seek #2
	if n := countType(t, po.events, "pause"); n != 1 {
		t.Fatalf("re-pause before stallTicks: %v", evType(t, po.events))
	}
	e.OnMpvp(mpv.State{Pos: ptr(20), Pause: false}) // behind 2 -> pause #2
	if n := countType(t, po.events, "pause"); n != 2 {
		t.Fatalf("stall pause did not reset on play: %d pauses (events: %v)", n, evType(t, po.events))
	}
	if len(pl.seeks) != 2 {
		t.Fatalf("expected exactly the two probe seeks: %v", pl.seeks)
	}
}
