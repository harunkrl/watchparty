package sync

// Audit round 2: the seams around file changes. A load swaps the file
// under mpv; the old file's EOF must not be mistaken for the new one's.

import (
	"testing"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
)

// Playing file A and starting file B must not end the room: the poll
// that lands inside the load window sees an idle player, and a stale
// "active" flag would turn that into an "ended" event — the server then
// advances the queue and the room jumps to a file nobody asked for.
func TestLoadDuringPlaybackDoesNotEndTheRoom(t *testing.T) {
	e, _, po := newEngine(true)
	e.OnMpvp(mpv.State{Pos: ptr(10)})                 // playing: the old file is active
	e.OnRoom(room.Event{Type: "load", Name: "b.mkv"}) // the user starts another file
	e.OnMpvp(mpv.State{Idle: true})                   // the poll lands in the load window
	if n := countType(t, po.events, "ended"); n != 0 {
		t.Fatalf("load posted %d ended events: %v", n, evType(t, po.events))
	}

	// our own /play takes the same path
	e2, _, po2 := newEngine(true)
	e2.OnMpvp(mpv.State{Pos: ptr(10)})
	e2.OwnLoad("c.mkv")
	e2.OnMpvp(mpv.State{Idle: true})
	if n := countType(t, po2.events, "ended"); n != 0 {
		t.Fatalf("own load posted %d ended events: %v", n, evType(t, po2.events))
	}
}

// The new file still gets its own EOF: after it plays, going idle ends
// the room exactly once.
func TestEndedAfterLoadStillFires(t *testing.T) {
	e, _, po := newEngine(true)
	e.OnRoom(room.Event{Type: "load", Name: "b.mkv"})
	e.OnMpvp(mpv.State{Pos: ptr(0.5)}) // the new file is live
	e.OnMpvp(mpv.State{Idle: true})    // its EOF
	if n := countType(t, po.events, "ended"); n != 1 {
		t.Fatalf("ended posted %d times after a load, want 1: %v", n, evType(t, po.events))
	}
}
