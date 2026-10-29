package sync

// H4: the engine carries the room's actor into the UI state, so the stat
// row can say WHO paused the room. Our own pause (and the stall pause)
// has no actor.

import (
	"testing"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
)

func TestPauseActorReachesTheUi(t *testing.T) {
	e, _, _ := newEngine(true)
	ui := make(chan UiState, 8)
	e.Ui = ui

	// a remote pause names its actor
	e.OnRoom(room.Event{Type: "pause", Nick: "bo"})
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: true})
	st := <-ui
	if st.PausedBy != "bo" {
		t.Fatalf("PausedBy = %q, want bo", st.PausedBy)
	}

	// playing: nobody holds the pause any more
	e.OnRoom(room.Event{Type: "play", Nick: "bo"})
	e.OnMpvp(mpv.State{Pos: ptr(11)})
	st = <-ui
	if st.PausedBy != "" {
		t.Fatalf("PausedBy after play = %q, want empty", st.PausedBy)
	}

	// our own pause has no actor (the echo never comes back to us)
	e2, _, _ := newEngine(true)
	ui2 := make(chan UiState, 8)
	e2.Ui = ui2
	e2.PauseToggle()
	e2.OnMpvp(mpv.State{Pos: ptr(10), Pause: true})
	st = <-ui2
	if st.PausedBy != "" {
		t.Fatalf("our own pause must have no actor: %q", st.PausedBy)
	}
}

// FB3: a pause that arrives with the hello snapshot names no actor —
// the room did not tell us who, so the chip stays honest and empty.
func TestSnapshotPauseHasNoActor(t *testing.T) {
	e, _, _ := newEngine(true)
	ui := make(chan UiState, 8)
	e.Ui = ui
	pos := 10.0
	e.OnRoom(room.Event{Type: "hello", Now: "a.mkv", Pos: &pos, P: true})
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: true})
	st := <-ui
	if st.PausedBy != "" {
		t.Fatalf("PausedBy = %q, want empty (the snapshot names no actor)", st.PausedBy)
	}
}
