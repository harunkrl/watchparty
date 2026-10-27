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

// stepClock measures Since against now, unlike fakeClock's fixed since,
// so snapshot re-anchoring (R5) is observable in tests.
type stepClock struct{ now time.Time }

func (c stepClock) Now() time.Time                  { return c.now }
func (c stepClock) Since(t time.Time) time.Duration { return c.now.Sub(t) }

// TestResumeDoesNotJump (R5): after a pause the snapshot's age spans the
// pause; without re-anchoring, expected over-ages on resume and the
// follower jumps forward to chase it. Here 10 s of pause must NOT turn
// into a seek when the follower sits 0.05 s off the master.
func TestResumeDoesNotJump(t *testing.T) {
	e, pl, po := newEngine(false)
	t0 := time.Unix(3000, 0)
	e.Clock = stepClock{now: t0}
	e.OnRoom(masterAt(12, true))                       // paused master snapshot, anchored at t0
	e.Clock = stepClock{now: t0.Add(10 * time.Second)} // the pause lasts 10 s
	e.OnRoom(room.Event{Type: "play"})                 // resume: snapshot re-anchored NOW
	e.Clock = stepClock{now: t0.Add(10*time.Second + 100*time.Millisecond)}
	e.OnMpvp(mpv.State{Pos: ptr(12.05), Pause: false})
	if len(pl.seeks) != 0 {
		t.Fatalf("resume raced into a seek (jump): %v", pl.seeks)
	}
	if len(po.events) != 0 {
		t.Fatalf("unexpected posts on resume: %v", evType(t, po.events))
	}
}

// TestBufferingCountsTowardStall: mpv's paused-for-cache feeds the same
// stall counter as behind-live — full runway before the pause, never an
// instant duplicate right after a resume (R-out1).
func TestBufferingCountsTowardStall(t *testing.T) {
	e, pl, po := newEngine(false)
	e.OnRoom(masterAt(12, false))
	e.Clock = pastGrace()

	e.OnMpvp(mpv.State{Pos: ptr(22), Pause: false, Buffering: true}) // behind 1
	if n := countType(t, po.events, "pause"); n != 0 {
		t.Fatalf("paused on the first buffering tick: %v", evType(t, po.events))
	}
	e.OnMpvp(mpv.State{Pos: ptr(22), Pause: false, Buffering: true}) // behind 2 -> stall
	if n := countType(t, po.events, "pause"); n != 1 {
		t.Fatalf("buffering did not stall after the runway: %v", evType(t, po.events))
	}
	if len(pl.seeks) != 0 {
		t.Fatalf("seek while buffering: %v", pl.seeks)
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

// TestNoticeDemotesImpostor (R7b): a notice naming OUR id demotes a
// second --master to follower; a notice naming someone else must not.
func TestNoticeDemotesImpostor(t *testing.T) {
	e, _, _ := newEngine(true)
	e.ID = "bo-1234"

	e.OnRoom(room.Event{Type: "notice", From: "bo-1234", Text: "two masters: ada owns the clock; ignoring bo"})
	if e.Master {
		t.Fatal("impostor was not demoted by its own notice")
	}

	e2, _, _ := newEngine(true)
	e2.ID = "bo-1234"
	e2.OnRoom(room.Event{Type: "notice", From: "someone-else", Text: "two masters: ada owns the clock; ignoring someone"})
	if !e2.Master {
		t.Fatal("demoted by a notice naming another client")
	}
}

// TestLoadQuietSuppressesSeekPost: mpv fires a seek event at the head of a
// freshly loaded file; posting it would rewind the room. Within loadQuiet
// the post is suppressed — a real scrub after the window still posts.
func TestLoadQuietSuppressesSeekPost(t *testing.T) {
	e, pl, po := newEngine(true)
	e.ID = "m-1"
	e.OwnLoad("x.mkv")
	pl.pos = ptr(1.4)
	e.OnMpvEvent("seek") // load echo: suppressed
	if len(po.events) != 0 {
		t.Fatalf("post-load head seek reached the room: %v", evType(t, po.events))
	}
	e.Clock = fakeClock{since: 6 * time.Second}
	e.OnMpvEvent("seek") // genuine scrub past the window: posted
	if n := countType(t, po.events, "seek"); n != 1 {
		t.Fatalf("scrub after the window must post: %v", evType(t, po.events))
	}
}

// TestSeekEchoConsumedOnce: a programmatic seek owns exactly ONE mpv echo
// event; a user scrub right after it — even to a stale time-pos — must
// still post (soak regression: target matching read stale positions).
func TestSeekEchoConsumedOnce(t *testing.T) {
	e, pl, po := newEngine(false)
	t0 := time.Unix(3000, 0)
	e.Clock = stepClock{now: t0} // real elapsed time: expiry must see a FRESH seek
	e.OnRoom(masterAt(12, false))

	e.OnRoom(room.Event{Type: "seek", T: 40}) // remote seek applied: echo pending
	pl.pos = ptr(40)
	e.OnMpvEvent("seek") // the echo itself: swallowed, never posted
	if len(po.events) != 0 {
		t.Fatalf("echo reached the room: %v", evType(t, po.events))
	}

	pl.pos = ptr(20)                                  // user scrub
	e.Clock = stepClock{now: t0.Add(4 * time.Second)} // soak timing: past actuateQuiet, inside the echo window
	e.OnMpvEvent("seek")
	if n := countType(t, po.events, "seek"); n != 1 {
		t.Fatalf("user scrub eaten by the echo gate: %v", evType(t, po.events))
	}

	// expiry: a pending echo that never arrives must not eat a later scrub
	e.OnRoom(room.Event{Type: "seek", T: 40})
	e.Clock = stepClock{now: t0.Add(10 * time.Second)} // 6s past the fresh pending echo
	pl.pos = ptr(25)
	e.OnMpvEvent("seek")
	if n := countType(t, po.events, "seek"); n != 2 {
		t.Fatalf("expired pending echo ate a scrub: %v", evType(t, po.events))
	}
}
