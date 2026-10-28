package sync

// C3 fix: the mute flag must survive the two places where the engine
// rebuilds a state for the UI. Without it the chip blinked off for one
// tick on every pause transition.

import (
	"testing"

	"watchparty/client-go/internal/mpv"
)

func TestMuteSurvivesEofTransition(t *testing.T) {
	ui := make(chan UiState, 4)
	e, _, _ := newEngine(false)
	e.Ui = ui
	e.OnMpvp(mpv.State{Pos: ptr(5), Muted: true}) // playing, muted
	e.OnMpvp(mpv.State{Idle: true, Muted: true})  // EOF: the state is rebuilt
	st := <-ui
	if !st.Muted {
		t.Fatalf("the EOF transition dropped the mute flag: %+v", st)
	}
}

func TestMuteSurvivesFollowerSelfHeal(t *testing.T) {
	ui := make(chan UiState, 4)
	e, _, _ := newEngine(false)
	e.Ui = ui
	e.OnRoom(masterAt(10, true))                                 // the master is paused
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false, Muted: true}) // we are not: self-heal
	st := <-ui
	if !st.Muted {
		t.Fatalf("the follower self-heal dropped the mute flag: %+v", st)
	}
}
