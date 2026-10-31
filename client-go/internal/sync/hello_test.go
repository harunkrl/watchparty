package sync

import (
	"testing"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
)

// Protocol contract (server emits hello {Now, Pos, P} when the room has a
// live snapshot). Follower must apply all three: load + seek + pause.
func TestHelloAppliesSnapshot(t *testing.T) {
	e, pl, _ := newEngine(false)
	pos := 123.5
	e.OnRoom(room.Event{Type: "hello", Now: "x.mkv", Pos: &pos, P: true})
	// F6-A1: the load claim is speculative at StartLoad
	if e.loaded != "x.mkv" {
		t.Fatalf("load: got %q, want x.mkv", e.loaded)
	}
	if len(pl.pauses) != 1 || !pl.pauses[0] {
		t.Fatalf("pause: got %v, want [true]", pl.pauses)
	}
	// F1: the hello loaded a NEW file, so its duration is unknown until
	// mpv reports it. The position is held back: seeking now could land
	// past the end of the file.
	if len(pl.seeks) != 0 {
		t.Fatalf("seeked before the duration was known: %v", pl.seeks)
	}
	// the first sample that knows the duration applies the position
	e.OnMpvp(mpv.State{Pos: ptr(0), Dur: 300})
	if len(pl.seeks) != 1 || pl.seeks[0] != 123.5 {
		t.Fatalf("seek: got %v, want [123.5]", pl.seeks)
	}
	if e.snap == nil || e.snap.t != 123.5 || !e.snap.paused {
		t.Fatalf("snap: got %+v, want t=123.5 paused=true", e.snap)
	}
}

// hello without Pos: legacy or first-join shape. Load but do not seek/snap.
func TestHelloWithoutPosDoesNotSnap(t *testing.T) {
	e, pl, _ := newEngine(false)
	e.OnRoom(room.Event{Type: "hello", Now: "x.mkv"})
	if e.loaded != "x.mkv" || len(pl.seeks) != 0 || len(pl.pauses) != 0 {
		t.Fatalf("legacy hello: got loaded=%q seeks=%v pauses=%v",
			e.loaded, pl.seeks, pl.pauses)
	}
	if e.snap != nil {
		t.Fatalf("snap must be nil for hello without Pos: %+v", e.snap)
	}
}
