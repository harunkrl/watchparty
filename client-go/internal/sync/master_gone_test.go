package sync

// F6 D1: the "no room heartbeat" state must mean a MISSING master, not
// an ENDED film. When the local player goes idle the master stopped
// posting positions with the file — that is EOF, not a lost master, and
// the flag must not fire there (M1 semantics).

import (
	"testing"
	"time"

	"watchparty/client-go/internal/mpv"
)

func TestEndedFilmIsNotReportedAsAMissingMaster(t *testing.T) {
	ui := make(chan UiState, 4)
	e, _, _ := newEngine(false)
	e.Ui = ui
	t0 := time.Unix(5000, 0)
	e.Clock = stepClock{now: t0}
	e.OnRoom(masterAt(10, false)) // the master's heartbeat at t0
	e.Clock = stepClock{now: t0.Add(7 * time.Second)}
	// the local player is idle: the film ENDED -> the master's heartbeats
	// stopped with the file, nobody is missing
	e.OnMpvp(mpv.State{Pos: ptr(17), Pause: false, Idle: true})
	st := <-ui
	if st.Stale {
		t.Fatalf("an ended film was reported as a missing master: %+v", st)
	}
}

func TestLiveFilmWithASilentMasterIsReported(t *testing.T) {
	ui := make(chan UiState, 4)
	e, _, _ := newEngine(false)
	e.Ui = ui
	t0 := time.Unix(5000, 0)
	e.Clock = stepClock{now: t0}
	e.OnRoom(masterAt(10, false))
	e.Clock = stepClock{now: t0.Add(7 * time.Second)}
	// still playing (not idle) and no heartbeat for 7s: the master is gone
	e.OnMpvp(mpv.State{Pos: ptr(17), Pause: false})
	st := <-ui
	if !st.Stale {
		t.Fatalf("a silent master during playback was not flagged: %+v", st)
	}
}
