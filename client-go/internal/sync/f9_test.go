package sync

// F9 pins — each was demonstrated RED on the v1.9.0 wiring before the
// engine changes (see .pi-state/f9b-worker1-raw.md for the captured runs).

import (
	"testing"
	"time"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
)

// anchorClock answers Since(t) with the real delta from a fixed anchor:
// one clock, two different ages (the fakeClock's fixed `since` cannot
// distinguish anchors, and F9's gate+grace need exactly that).
type anchorClock struct{ now time.Time }

func (c anchorClock) Now() time.Time                  { return c.now }
func (c anchorClock) Since(t time.Time) time.Duration { return c.now.Sub(t) }

// PIN F9.1 (A9 K1): a lone follower's ctrl+space WORKS — it toggles the
// local player AND posts, so the room hears the pause even while no master
// is connected. On v1.9.0 the follower branch returned silently: no tell,
// no post, no refusal.
func TestF9LoneFollowerPauseTogglePosts(t *testing.T) {
	e, pl, po := newEngine(false)
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false}) // playing alone
	waitTells(t, e)

	e.PauseToggle()
	waitTells(t, e)
	if got := evType(t, po.all(t)); len(got) != 1 || got[0] != "pause" {
		t.Fatalf("lone pause did not reach the room: %v", got)
	}
	if got := pl.pauseCount(); got != 1 {
		t.Fatalf("the local player was not paused: %d", got)
	}

	e.PauseToggle() // the double-press reads postedPause inside the grace
	waitTells(t, e)
	if got := evType(t, po.all(t)); len(got) != 2 || got[1] != "play" {
		t.Fatalf("the second press did not resume+post: %v", got)
	}
	if got := pl.pauseCount(); got != 2 {
		t.Fatalf("the local player was not resumed: %d", got)
	}
}

// PIN F9.1b: nothing loaded + nobody in the room — the key refuses WITH A
// VISIBLE reason instead of silently doing nothing.
func TestF9LoneFollowerPauseRefusesWhenIdle(t *testing.T) {
	e, _, po := newEngine(false) // idle: no sample ever arrived

	e.PauseToggle()
	waitTells(t, e)
	if got := evType(t, po.all(t)); len(got) != 0 {
		t.Fatalf("an idle engine must not post: %v", got)
	}
	if got := pl_snapshot(e); len(got) == 0 {
		t.Fatal("the refusal was silent — no OSD, no visible reason")
	}
}

// pl_snapshot keeps pin 1b independent of the player var above.
func pl_snapshot(e *Engine) []string {
	fp, ok := e.Player.(*fakePlayer)
	if !ok {
		return nil
	}
	return fp.showTextSnapshot()
}

// PIN F9.2 (A9 :1187): a lone follower pausing in the MPV WINDOW posts
// now; the value+recency dedup eats the echo of our own actuation, and an
// opposite pause still posts (a stale postedPause must not swallow it).
func TestF9MpvWindowLonePausePosts(t *testing.T) {
	e, _, po := newEngine(false)
	// a genuine mpv-window pause REQUIRES a playing file (F9 round 3):
	// without a load the pause events are observer machinery, swallowed.
	// anchorClock: Since computes REAL deltas from a fixed anchor — the
	// load-quiet gate (Since loadedAt = 6s: open) and the echo grace
	// (Since lastPost = 0: inside) must read DIFFERENT values, which the
	// fixed-since fakeClock cannot express.
	e.Clock = anchorClock{now: time.Unix(1000, 0)}
	e.loaded = "f.mkv"
	e.loadedAt = time.Unix(994, 0)
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false})
	waitTells(t, e)

	e.OnMpvEvent("pause") // the user pressed space in the player window
	waitTells(t, e)
	if got := evType(t, po.all(t)); len(got) != 1 || got[0] != "pause" {
		t.Fatalf("the lone mpv-window pause was not posted: %v", got)
	}

	e.OnMpvEvent("pause") // our own actuation echo: same value, in grace
	waitTells(t, e)
	if got := evType(t, po.all(t)); len(got) != 1 {
		t.Fatalf("the echo double-posted: %v", got)
	}

	e.OnMpvEvent("unpause") // opposite direction: must post despite postedPause
	waitTells(t, e)
	if got := evType(t, po.all(t)); len(got) != 2 || got[1] != "play" {
		t.Fatalf("the opposite pause did not post: %v", got)
	}
}

// PIN F9.3 (hello.p without a position — the client half of CASE B): a
// joining client adopts the room's pause even when the hello carries no
// position, exactly once, and the next ctrl+space resumes toward the room.
func TestF9HelloPauseWithoutPositionHonored(t *testing.T) {
	e, pl, po := newEngine(false)
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false}) // playing alone
	waitTells(t, e)

	e.OnRoom(room.Event{Type: "hello", P: true}) // the room is paused; no pos
	waitTells(t, e)
	if got := pl.pauseCount(); got != 1 {
		t.Fatalf("the room's position-less pause was not adopted: %d", got)
	}
	if got := evType(t, po.all(t)); len(got) != 0 {
		t.Fatalf("adopting the room's own state must not post: %v", got)
	}

	e.PauseToggle() // resume toward the room
	waitTells(t, e)
	if got := evType(t, po.all(t)); len(got) != 1 || got[0] != "play" {
		t.Fatalf("the resume after an adopted pause did not post: %v", got)
	}
}

// PIN F9.6 (postScrub lone-skip honesty): a lone scrub still moves the
// local player, but the skipped post says so in the log.
func TestF9LoneScrubSkipLogs(t *testing.T) {
	pl := &fakePlayer{pos: ptr(30)}
	logs := &logCap{}
	eng := &Engine{Master: false, ID: "ada-1", Player: pl, Logf: logs.add,
		Clock: fakeClock{now: time.Unix(1000, 0), since: 6 * time.Second}}
	eng.loaded = "f.mkv"
	eng.loadedAt = time.Unix(994, 0) // past loadQuiet

	eng.OnMpvEvent("seek") // lone follower: no snapshot to sync against
	waitTells(t, eng)
	if logs.count("scrub") == 0 {
		t.Fatalf("the skipped scrub post was silent: %v", logs.lines)
	}
}

// PIN F9.8 (master-2 blocker, flip-server repro): a joiner that adopted a
// paused pos-less room must not let mpv's file-start noise flip the room.
// A phantom unpause (emitted by mpv, never actuated by the engine) inside
// the load-quiet window stays local; a genuine one after the window posts.
func TestF9JoinerPausedRoomStaysPaused(t *testing.T) {
	e, pl, po := newEngine(false)
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false})
	waitTells(t, e)

	e.OnRoom(room.Event{Type: "hello", P: true}) // paused, pos-less: adopted
	waitTells(t, e)

	// shape 1 (master-2 round 3, THE flaky repro): the phantom is the
	// observe_property INITIAL event at ENGINE START — loadedAt is still
	// ZERO when it races the hello. The zero case must be swallowed too
	// (an unloaded lone client's pause events are observer machinery; a
	// genuine mpv-window pause requires a playing file).
	if !e.loadedAt.IsZero() {
		t.Fatal("precondition: this shape is the loadedAt-ZERO case")
	}
	e.OnMpvEvent("unpause")
	waitTells(t, e)
	if got := evType(t, po.all(t)); len(got) != 0 {
		t.Fatalf("the startup phantom unpause flipped the room the joiner just paused: %v", got)
	}

	// shape 2: inside the load-quiet window on a FRESH load
	e.Clock = fakeClock{now: time.Unix(1000, 0), since: 2 * time.Second}
	e.loadedAt = time.Unix(999, 0) // 2s old (fakeClock.since is fixed)
	e.OnMpvEvent("unpause")
	waitTells(t, e)
	if got := evType(t, po.all(t)); len(got) != 0 {
		t.Fatalf("the fresh-load phantom unpause flipped the room: %v", got)
	}
	if got := pl.pauseCount(); got != 1 {
		t.Fatalf("the phantom must not actuate anything: %d", got)
	}

	// after the window: a genuine unpause posts again
	e.Clock = fakeClock{now: time.Unix(1100, 0), since: 6 * time.Second}
	e.OnMpvEvent("unpause")
	waitTells(t, e)
	if got := evType(t, po.all(t)); len(got) != 1 || got[0] != "play" {
		t.Fatalf("a genuine unpause after load-quiet did not post: %v", got)
	}
}
