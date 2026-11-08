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
	waitTells(t, e)
	// F11 C2: the load resets the local pause (play) and the P:true adoption
	// actuates pause — TWO pause-kind tells. Coalesce-to-latest usually
	// collapses them to [true], but the tell worker may pop the reset in the
	// enqueue window (the v1.9.3 CI flake): then both run, [false true].
	// The CONTRACT is last-state-wins (F8 tell law): the final write wins,
	// the count is not an observable.
	pauses := pl.pausesSnapshot()
	if len(pauses) == 0 || !pauses[len(pauses)-1] {
		t.Fatalf("pause: got %v, want the final state paused", pauses)
	}
	// F1: the hello loaded a NEW file, so its duration is unknown until
	// mpv reports it. The position is held back: seeking now could land
	// past the end of the file.
	if len(pl.seeksSnapshot()) != 0 {
		t.Fatalf("seeked before the duration was known: %v", pl.seeksSnapshot())
	}
	// the first sample that knows the duration applies the position
	e.OnMpvp(mpv.State{Pos: ptr(0), Dur: 300})
	waitTells(t, e)
	if len(pl.seeksSnapshot()) != 1 || pl.seeksSnapshot()[0] != 123.5 {
		t.Fatalf("seek: got %v, want [123.5]", pl.seeksSnapshot())
	}
	if e.snap == nil || e.snap.t != 123.5 || !e.snap.paused {
		t.Fatalf("snap: got %+v, want t=123.5 paused=true", e.snap)
	}
}

// hello without Pos: legacy or first-join shape. Load but do not seek/snap.
func TestHelloWithoutPosDoesNotSnap(t *testing.T) {
	e, pl, _ := newEngine(false)
	e.OnRoom(room.Event{Type: "hello", Now: "x.mkv"})
	waitTells(t, e)
	// F11 C2 contract update: the legacy hello LOADS, and the load PLAYS
	// LOCALLY (the C2 reset actuates SetPause(false) — legitimate; the old
	// expectation pauses==0 only passed by asserting before the tell worker
	// drained). No seek (no Pos), no snapshot, and the final pause state is
	// playing.
	if e.loaded != "x.mkv" || len(pl.seeksSnapshot()) != 0 {
		t.Fatalf("legacy hello: got loaded=%q seeks=%v", e.loaded, pl.seeksSnapshot())
	}
	pauses := pl.pausesSnapshot()
	if len(pauses) == 0 || pauses[len(pauses)-1] {
		t.Fatalf("legacy hello: the C2 load must play locally, got pauses=%v", pauses)
	}
	if e.snap != nil {
		t.Fatalf("snap must be nil for hello without Pos: %+v", e.snap)
	}
}
