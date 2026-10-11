package sync

import (
	"testing"
	"time"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
)

type fakePlayer struct {
	loads    []string
	pauses   []bool
	seeks    []float64
	showText []string
}

func (f *fakePlayer) Load(name string) error { f.loads = append(f.loads, name); return nil }
func (f *fakePlayer) SetPause(p bool) error  { f.pauses = append(f.pauses, p); return nil }
func (f *fakePlayer) Seek(t float64) error   { f.seeks = append(f.seeks, t); return nil }
func (f *fakePlayer) ShowText(s string, ms int) error {
	f.showText = append(f.showText, s)
	return nil
}

type fakePost struct{ events []room.Event }

func (f *fakePost) post(ev room.Event) error { f.events = append(f.events, ev); return nil }

// fakeClock: fixed Now, configurable Since — instant 60s simulations.
type fakeClock struct {
	now   time.Time
	since time.Duration
}

func (c fakeClock) Now() time.Time                  { return c.now }
func (c fakeClock) Since(t time.Time) time.Duration { return c.since }

func ptr(f float64) *float64 { return &f }

func newEngine(master bool) (*Engine, *fakePlayer, *fakePost) {
	pl, po := &fakePlayer{}, &fakePost{}
	return &Engine{
		Master: master,
		Player: pl,
		Post:   po.post,
		Clock:  fakeClock{now: time.Unix(1000, 0), since: 2 * time.Second},
	}, pl, po
}

func evType(t *testing.T, evs []room.Event) []string {
	t.Helper()
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

// --- master ---------------------------------------------------------------

func TestMasterHeartbeatNoScrub(t *testing.T) {
	e, _, po := newEngine(true)
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false})
	e.OnMpvp(mpv.State{Pos: ptr(12), Pause: false}) // +2s: exactly PollSecs
	if len(po.events) != 2 {
		t.Fatalf("events = %v, want 2 position heartbeats", evType(t, po.events))
	}
	for i, want := range []float64{10, 12} {
		if got := po.events[i]; got.Type != "position" || got.T != want || got.P {
			t.Fatalf("event %d = %+v, want position{t:%v p:false}", i, got, want)
		}
	}
}

func TestMasterPausePlayTransitions(t *testing.T) {
	e, _, po := newEngine(true)
	e.OnMpvp(mpv.State{Pos: ptr(5), Pause: false})
	e.OnMpvp(mpv.State{Pos: ptr(7), Pause: true}) // user pressed space
	e.OnMpvp(mpv.State{Pos: ptr(7), Pause: true}) // steady: pause not repeated,
	// but the position heartbeat fires on every tick (client.py parity)
	types := evType(t, po.events)
	want := []string{"position", "pause", "position", "position"}
	if len(types) != len(want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("events = %v, want %v", types, want)
		}
	}
}

func TestMasterScrubPostsSeek(t *testing.T) {
	e, _, po := newEngine(true)
	e.OnMpvp(mpv.State{Pos: ptr(10)})
	e.OnMpvp(mpv.State{Pos: ptr(20)}) // |20-10-2| = 8 > 3: scrub
	found := false
	for _, ev := range po.events {
		if ev.Type == "seek" && ev.T == 20 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no seek event in %v", po.events)
	}
}

func TestMasterEndedOnIdleAfterActive(t *testing.T) {
	e, _, po := newEngine(true)
	e.OnMpvp(mpv.State{Pos: ptr(5)}) // active
	e.OnMpvp(mpv.State{Idle: true})  // EOF -> idle
	e.OnMpvp(mpv.State{Idle: true})  // still idle: ended fires once
	ended := 0
	for _, ev := range po.events {
		if ev.Type == "ended" {
			ended++
		}
	}
	if ended != 1 {
		t.Fatalf("ended fired %d times, want 1 (events: %v)", ended, evType(t, po.events))
	}
}

func TestMasterNothingLoadedIsSilent(t *testing.T) {
	e, _, po := newEngine(true)
	e.OnMpvp(mpv.State{}) // pos nil, not idle
	if len(po.events) != 0 {
		t.Fatalf("events = %v, want none", evType(t, po.events))
	}
}

// --- follower -------------------------------------------------------------

func masterAt(t float64, paused bool) room.Event {
	return room.Event{Type: "position", T: t, P: paused}
}

func TestFollowerSelfHealPauseState(t *testing.T) {
	e, pl, _ := newEngine(false)
	e.OnRoom(masterAt(5, true))                    // master paused
	e.OnMpvp(mpv.State{Pos: ptr(5), Pause: false}) // we are still playing
	if len(pl.pauses) != 1 || !pl.pauses[0] {
		t.Fatalf("pauses = %v, want [true]", pl.pauses)
	}
	if len(pl.seeks) != 0 {
		t.Fatalf("no seek expected during self-heal, got %v", pl.seeks)
	}
}

func TestFollowerDriftSeek(t *testing.T) {
	e, pl, _ := newEngine(false)
	e.Clock = fakeClock{now: time.Unix(1000, 0), since: 5 * time.Second}
	e.OnRoom(masterAt(10, false))
	// Since = 5s -> expected 15; we are at 10: drift -5 > TOLERANCE
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false})
	if len(pl.seeks) != 1 || pl.seeks[0] != 15 {
		t.Fatalf("seeks = %v, want [15]", pl.seeks)
	}
}

func TestFollowerSmallDriftHolds(t *testing.T) {
	e, pl, _ := newEngine(false)
	e.OnRoom(masterAt(10, false))
	e.OnMpvp(mpv.State{Pos: ptr(12.3), Pause: false}) // expected 12: drift 0.3
	if len(pl.seeks) != 0 {
		t.Fatalf("seek fired on drift 0.3: %v", pl.seeks)
	}
}

func TestFollowerExpectedFrozenWhileMasterPaused(t *testing.T) {
	e, pl, _ := newEngine(false)
	e.OnRoom(masterAt(5, true))
	e.OnMpvp(mpv.State{Pos: ptr(5), Pause: true}) // both paused: hold
	if len(pl.seeks) != 0 || len(pl.pauses) != 0 {
		t.Fatalf("both-paused must hold: seeks=%v pauses=%v", pl.seeks, pl.pauses)
	}
}

func TestFollowerNeedsSnapshot(t *testing.T) {
	e, pl, _ := newEngine(false)
	e.OnMpvp(mpv.State{Pos: ptr(5), Pause: false}) // no master heartbeat yet
	if len(pl.seeks)+len(pl.pauses)+len(pl.loads) != 0 {
		t.Fatal("follower acted without a master snapshot")
	}
}

// --- engine plumbing ------------------------------------------------------

func TestOnRoomAppliesDiscreteEvents(t *testing.T) {
	e, pl, _ := newEngine(false)
	e.OnRoom(room.Event{Type: "load", Name: "a.mkv"})
	e.OnRoom(room.Event{Type: "pause"})
	e.OnRoom(room.Event{Type: "play"})
	e.OnRoom(room.Event{Type: "seek", T: 42})
	if len(pl.loads) != 1 || pl.loads[0] != "a.mkv" {
		t.Fatalf("loads = %v", pl.loads)
	}
	if len(pl.pauses) != 2 || !pl.pauses[0] || pl.pauses[1] {
		t.Fatalf("pauses = %v, want [true false]", pl.pauses)
	}
	if len(pl.seeks) != 1 || pl.seeks[0] != 42 {
		t.Fatalf("seeks = %v", pl.seeks)
	}
}

func TestUiStateEmittedOnFollowerTick(t *testing.T) {
	ui := make(chan UiState, 1)
	e, _, _ := newEngine(false)
	e.Ui = ui
	e.OnRoom(masterAt(10, false))
	e.OnMpvp(mpv.State{Pos: ptr(12.3), Pause: false}) // expected 12, drift 0.3
	select {
	case st := <-ui:
		if st.Drift < 0.29 || st.Drift > 0.31 {
			t.Fatalf("drift = %v, want 0.3", st.Drift)
		}
	default:
		t.Fatal("no UiState emitted")
	}
}
