package sync

// C5: a follower whose master heartbeat stopped must be able to say so.
// The engine owns the snapshot age; the UI only renders the flag.

import (
	"testing"
	"time"

	"watchparty/client-go/internal/mpv"
)

func TestFollowerFlagsStaleSnapshot(t *testing.T) {
	ui := make(chan UiState, 4)
	e, _, _ := newEngine(false)
	e.Ui = ui
	t0 := time.Unix(5000, 0)
	e.Clock = stepClock{now: t0}
	e.OnRoom(masterAt(10, false)) // snapshot anchored at t0
	e.Clock = stepClock{now: t0.Add(7 * time.Second)}
	e.OnMpvp(mpv.State{Pos: ptr(17), Pause: false}) // expected 17 -> drift 0
	st := <-ui
	if !st.Stale {
		t.Fatalf("a 7s old snapshot was not flagged stale: %+v", st)
	}
	// a fresh heartbeat clears the flag
	e.Clock = stepClock{now: t0.Add(7 * time.Second)}
	e.OnRoom(masterAt(17, false))
	e.Clock = stepClock{now: t0.Add(8 * time.Second)}
	e.OnMpvp(mpv.State{Pos: ptr(18), Pause: false})
	st = <-ui
	if st.Stale {
		t.Fatalf("a fresh snapshot was flagged stale: %+v", st)
	}
}

func TestMasterIsNeverStale(t *testing.T) {
	ui := make(chan UiState, 2)
	e, _, _ := newEngine(true)
	e.Ui = ui
	e.OnMpvp(mpv.State{Pos: ptr(10)})
	st := <-ui
	if st.Stale {
		t.Fatalf("the master was flagged stale: %+v", st)
	}
}
