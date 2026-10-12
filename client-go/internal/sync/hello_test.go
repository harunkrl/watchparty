package sync

import (
	"testing"

	"watchparty/client-go/internal/room"
)

// Protocol contract (server emits hello {Now, Pos, P} when the room has a
// live snapshot). Follower must apply all three: load + seek + pause.
func TestHelloAppliesSnapshot(t *testing.T) {
	e, pl, _ := newEngine(false)
	pos := 123.5
	e.OnRoom(room.Event{Type: "hello", Now: "x.mkv", Pos: &pos, P: true})
	if len(pl.loads) != 1 || pl.loads[0] != "x.mkv" {
		t.Fatalf("load: got %v, want [x.mkv]", pl.loads)
	}
	if len(pl.seeks) != 1 || pl.seeks[0] != 123.5 {
		t.Fatalf("seek: got %v, want [123.5]", pl.seeks)
	}
	if len(pl.pauses) != 1 || !pl.pauses[0] {
		t.Fatalf("pause: got %v, want [true]", pl.pauses)
	}
	if e.snap == nil || e.snap.t != 123.5 || !e.snap.paused {
		t.Fatalf("snap: got %+v, want t=123.5 paused=true", e.snap)
	}
}

// hello without Pos: legacy or first-join shape. Load but do not seek/snap.
func TestHelloWithoutPosDoesNotSnap(t *testing.T) {
	e, pl, _ := newEngine(false)
	e.OnRoom(room.Event{Type: "hello", Now: "x.mkv"})
	if len(pl.loads) != 1 || len(pl.seeks) != 0 || len(pl.pauses) != 0 {
		t.Fatalf("legacy hello: got loads=%v seeks=%v pauses=%v",
			pl.loads, pl.seeks, pl.pauses)
	}
	if e.snap != nil {
		t.Fatalf("snap must be nil for hello without Pos: %+v", e.snap)
	}
}
