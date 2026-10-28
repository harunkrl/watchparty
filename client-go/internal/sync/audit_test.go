package sync

// Audit round (3-terminal bug hunt): multi-client correctness of the sync
// engine — applied-room-events must not be re-posted as local actions, and
// a reconnect must not inherit a stale stall window. Fake player/clock
// come from engine_test.go (same package).

import (
	"testing"
	"time"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
)

// The master applies a follower's seek to its own player. The applied
// seek moves the position, so the poll's scrub-check must not read that
// jump as a local scrub: the room already has this seek. Re-posting it
// makes every client seek again (a visible flush on a WAN link).
func TestMasterDoesNotRepostAppliedSeek(t *testing.T) {
	e, pl, po := newEngine(true)
	e.OnMpvp(mpv.State{Pos: ptr(10)})         // baseline heartbeat
	e.OnRoom(room.Event{Type: "seek", T: 60}) // a follower scrubbed: we apply it
	if len(pl.seeks) != 1 || pl.seeks[0] != 60 {
		t.Fatalf("applied seek not applied: %v", pl.seeks)
	}
	pl.pos = ptr(60)                  // our player is now at the target
	e.OnMpvp(mpv.State{Pos: ptr(60)}) // the poll must not call this a local scrub
	if n := countType(t, po.events, "seek"); n != 0 {
		t.Fatalf("applied seek re-posted %d times: %v", n, evType(t, po.events))
	}
}

// Same for a room load: the new file starts at 0, the old file's position
// is history. Without resetting the scrub baseline the master posts a
// phantom "seek" that drags the whole room to the new file's head.
func TestMasterDoesNotPostPhantomSeekAfterLoad(t *testing.T) {
	e, pl, po := newEngine(true)
	e.OnMpvp(mpv.State{Pos: ptr(300)})                // the old film's position
	e.OnRoom(room.Event{Type: "load", Name: "b.mkv"}) // the room moves on
	pl.pos = ptr(0.2)
	e.OnMpvp(mpv.State{Pos: ptr(0.2)}) // the new file starts at the head
	if n := countType(t, po.events, "seek"); n != 0 {
		t.Fatalf("phantom seek after load: %v", evType(t, po.events))
	}

	// our own /play (OwnLoad) takes the same path
	e2, pl2, po2 := newEngine(true)
	e2.OnMpvp(mpv.State{Pos: ptr(300)})
	e2.OwnLoad("c.mkv")
	pl2.pos = ptr(0.2)
	e2.OnMpvp(mpv.State{Pos: ptr(0.2)})
	if n := countType(t, po2.events, "seek"); n != 0 {
		t.Fatalf("phantom seek after own load: %v", evType(t, po2.events))
	}
}

// A follower's own action must be posted exactly once. A heartbeat that
// was already in flight when we posted overwrites the snapshot mirror, so
// the late echo of our own apply must not post the same state again —
// the room would apply it twice (e.g. pausing the room after the user
// already resumed it).
func TestFollowerDoesNotDoublePostOwnPause(t *testing.T) {
	e, _, po := newEngine(false)
	e.Clock = fakeClock{now: time.Unix(1000, 0), since: 2 * time.Second} // echo outside the actuation window
	e.OnRoom(masterAt(10, false))                                        // room playing
	e.PauseToggle()                                                      // user pauses: post + mirror
	e.OnRoom(masterAt(10, false))                                        // in-flight heartbeat: the mirror is lost
	e.OnMpvEvent("pause")                                                // the late echo of our own apply
	if n := countType(t, po.events, "pause"); n != 1 {
		t.Fatalf("pause posted %d times, want 1: %v", n, evType(t, po.events))
	}
}

// The same lost mirror must not make the next toggle compute the wrong
// target: our pending post is newer than the room's last heartbeat, so
// space means "resume" even though the stale snapshot says "playing".
func TestFollowerToggleUsesPendingPost(t *testing.T) {
	e, _, po := newEngine(false)
	e.Clock = fakeClock{now: time.Unix(1000, 0), since: 2 * time.Second}
	e.OnRoom(masterAt(10, false))
	e.PauseToggle()               // user pauses: posts "pause"
	e.OnRoom(masterAt(10, false)) // in-flight heartbeat: the mirror is lost
	e.PauseToggle()               // user presses space again: must resume
	if n := countType(t, po.events, "play"); n != 1 {
		t.Fatalf("play posted %d times, want 1: %v", n, evType(t, po.events))
	}
}

// A reconnect must not inherit the stall window: a behind-tick from
// before the outage would trip the counter on the first tick after the
// reconnect and pause the room for a stall that is already over.
func TestReconnectResetsStallCounter(t *testing.T) {
	e, _, po := newEngine(false)
	e.Clock = fakeClock{now: time.Unix(1000, 0), since: 5 * time.Second}
	e.OnRoom(room.Event{Type: "load", Name: "vid.mp4"}) // the file is already loaded
	e.OnRoom(masterAt(10, false))
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false})     // 5s behind: one behind-tick
	e.OnRoom(room.Event{Type: "hello", Now: "vid.mp4"}) // reconnect: same file -> no reload
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false})     // still behind: the window must have restarted
	if n := countType(t, po.events, "pause"); n != 0 {
		t.Fatalf("stall paused the room after a reconnect: %v", evType(t, po.events))
	}
}
