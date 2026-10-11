package sync

import (
	"fmt"
	"sync"
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
	pos      *float64
}

func (f *fakePlayer) Load(name string) error { f.loads = append(f.loads, name); return nil }
func (f *fakePlayer) SetPause(p bool) error  { f.pauses = append(f.pauses, p); return nil }
func (f *fakePlayer) Seek(t float64) error   { f.seeks = append(f.seeks, t); return nil }
func (f *fakePlayer) Pos() (float64, bool) {
	if f.pos == nil {
		return 0, false
	}
	return *f.pos, true
}
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

// --- instant event path (plan Q1) -----------------------------------------

func countType(t *testing.T, evs []room.Event, typ string) int {
	t.Helper()
	n := 0
	for _, ev := range evs {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

func TestInstantPauseNoDoublePost(t *testing.T) {
	e, _, po := newEngine(true)
	e.OnMpvp(mpv.State{Pos: ptr(5), Pause: false}) // baseline heartbeat
	e.OnMpvEvent("pause")                          // instant path posts
	e.OnMpvp(mpv.State{Pos: ptr(5), Pause: true})  // poll must NOT re-post
	if n := countType(t, po.events, "pause"); n != 1 {
		t.Fatalf("pause posted %d times, want 1 (events: %v)", n, evType(t, po.events))
	}
}

func TestInstantSeekSinglePost(t *testing.T) {
	e, pl, po := newEngine(true)
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false}) // lastPos = 10
	pl.pos = ptr(60)                                // user scrubbed
	e.OnMpvEvent("seek")                            // instant path posts 60
	e.OnMpvp(mpv.State{Pos: ptr(60), Pause: false}) // poll scrub-check skipped
	if n := countType(t, po.events, "seek"); n != 1 {
		t.Fatalf("seek posted %d times, want 1 (events: %v)", n, evType(t, po.events))
	}
	last := po.events[len(po.events)-2] // seek followed by its heartbeat
	if last.Type != "seek" || last.T != 60 {
		t.Fatalf("instant seek payload wrong: %+v", last)
	}
}

// --- dual control (v1.4): followers post discrete events too -------------

func logSink() (*[]string, func(string, ...any)) {
	logs := &[]string{}
	return logs, func(f string, a ...any) {
		*logs = append(*logs, fmt.Sprintf(f, a...))
	}
}

func hasLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func TestFollowerNoSnapshotNeverPosts(t *testing.T) {
	e, _, po := newEngine(false)
	e.OnMpvEvent("pause") // no master heartbeat yet: nothing to sync against
	e.OnMpvEvent("seek")
	if len(po.events) != 0 {
		t.Fatalf("follower posted without snapshot: %v", evType(t, po.events))
	}
}

func TestFollowerPausePostsAndGrace(t *testing.T) {
	logs, sink := logSink()
	e, pl, po := newEngine(false)
	e.Logf = sink
	e.Clock = fakeClock{now: time.Unix(1000, 0), since: 0}
	e.OnRoom(masterAt(10, false))
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false}) // consistent baseline
	e.OnMpvEvent("pause")                           // user pauses -> room hears it
	if n := countType(t, po.events, "pause"); n != 1 {
		t.Fatalf("pause posted %d times, want 1", n)
	}
	if !hasLine(*logs, "> pause") {
		t.Fatalf("own post not logged: %v", *logs)
	}
	// master has not applied it; heartbeat still says playing
	e.OnRoom(masterAt(10, false))
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: true}) // within grace: no revert
	if len(pl.pauses) != 0 {
		t.Fatalf("reverted during grace: %v", pl.pauses)
	}
	// grace expires, master still playing -> failsafe revert (authority)
	e.Clock = fakeClock{now: time.Unix(1000, 0), since: 5 * time.Second}
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: true})
	if len(pl.pauses) != 1 || pl.pauses[0] {
		t.Fatalf("no failsafe resume after grace: %v", pl.pauses)
	}
}

func TestFollowerSnapStateGate(t *testing.T) {
	e, _, po := newEngine(false)
	e.Clock = fakeClock{since: 10 * time.Second} // outside every window
	e.OnRoom(masterAt(10, true))                 // room already paused
	e.OnMpvEvent("pause")                        // echo of the room's own pause
	if len(po.events) != 0 {
		t.Fatalf("posted a state the room already knows: %v", evType(t, po.events))
	}
}

func TestFollowerSeekPosts(t *testing.T) {
	logs, sink := logSink()
	e, pl, po := newEngine(false)
	e.Logf = sink
	e.Clock = fakeClock{since: 10 * time.Second}
	e.OnRoom(masterAt(10, false))
	pl.pos = ptr(60)
	e.OnMpvEvent("seek") // user scrubbed
	if n := countType(t, po.events, "seek"); n != 1 || po.events[0].T != 60 {
		t.Fatalf("seek post wrong: %v", po.events)
	}
	if !hasLine(*logs, "> seek @60.0s") {
		t.Fatalf("own seek not logged: %v", *logs)
	}
}

func TestFollowerUnpausePostsPlayWireType(t *testing.T) {
	e, _, po := newEngine(false)
	e.Clock = fakeClock{since: 10 * time.Second} // outside every window
	e.OnRoom(masterAt(10, true))                 // room paused
	e.OnMpvEvent("unpause")                      // user resumes
	if len(po.events) != 1 || po.events[0].Type != "play" {
		t.Fatalf("unpause must post wire type play, got %v", evType(t, po.events))
	}
}

func TestUserPauseRightAfterRemoteApplyPosts(t *testing.T) {
	e, _, po := newEngine(false)
	e.Clock = fakeClock{since: 0} // inside the actuate window on purpose
	e.OnRoom(masterAt(10, true))
	e.OnRoom(room.Event{Type: "play"}) // remote resume applied (actuated)
	e.OnMpvEvent("pause")              // user space-bar 200ms later: NOT an echo
	if n := countType(t, po.events, "pause"); n != 1 {
		t.Fatalf("user pause swallowed as echo (posts: %v)", evType(t, po.events))
	}
}

func TestRemotePauseEchoSwallowed(t *testing.T) {
	e, _, po := newEngine(false)
	e.Clock = fakeClock{since: 0}
	e.OnRoom(masterAt(10, false))
	e.OnRoom(room.Event{Type: "pause"}) // remote pause applied -> setPause(true)
	e.OnMpvEvent("pause")               // the apply's own mpv echo
	if len(po.events) != 0 {
		t.Fatalf("echo re-posted: %v", evType(t, po.events))
	}
}

func TestMasterRemotePauseNoRepost(t *testing.T) {
	e, _, po := newEngine(true)
	e.Clock = fakeClock{since: 0}
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false}) // lastPause = false
	e.OnRoom(room.Event{Type: "pause"})             // friend paused; we apply
	e.OnMpvEvent("pause")                           // our mpv echo
	if n := countType(t, po.events, "pause"); n != 0 {
		t.Fatalf("master re-posted a remote application: %v", evType(t, po.events))
	}
}

func TestSimultaneousPauseBothSides(t *testing.T) {
	mE, mPl, mPo := newEngine(true)
	fE, _, fPo := newEngine(false)
	mE.Clock = fakeClock{now: time.Unix(1000, 0), since: 0}
	fE.Clock = fakeClock{now: time.Unix(1000, 0), since: 0}
	// cross-wire with recording; sequential test calls only (a cross-engine
	// lock order could deadlock if two engines posted concurrently in-process)
	mE.Post = func(ev room.Event) error { mPo.post(ev); fE.OnRoom(ev); return nil }
	fE.Post = func(ev room.Event) error { fPo.post(ev); mE.OnRoom(ev); return nil }

	mE.OnMpvp(mpv.State{Pos: ptr(10), Pause: false}) // baseline heartbeat -> follower snap

	// follower's pause lands first (user acted before the master's event):
	fE.OnMpvEvent("pause") // posts; master applies it (his room state is now paused)
	mE.OnMpvEvent("pause") // master's own user pause: suppressed, room knows

	if n := countType(t, fPo.events, "pause"); n != 1 {
		t.Fatalf("follower posted %d pauses, want 1", n)
	}
	if n := countType(t, mPo.events, "pause"); n != 0 {
		t.Fatalf("master re-posted a pause the room already had: %v", evType(t, mPo.events))
	}
	if len(mPl.pauses) != 1 || !mPl.pauses[0] {
		t.Fatalf("master did not apply the follower's pause: %v", mPl.pauses)
	}
	// settled polls must not start a storm
	mE.OnMpvp(mpv.State{Pos: ptr(10), Pause: true})
	fE.OnMpvp(mpv.State{Pos: ptr(10), Pause: true})
	if n := countType(t, fPo.events, "pause"); n != 1 || countType(t, mPo.events, "pause") != 0 {
		t.Fatalf("pause storm: master=%v follower=%v", evType(t, mPo.events), evType(t, fPo.events))
	}
}

func TestConcurrentHammer(t *testing.T) {
	e, _, _ := newEngine(true)
	var wg sync.WaitGroup
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				switch g {
				case 0:
					e.OnMpvp(mpv.State{Pos: ptr(float64(i % 90)), Pause: i%7 == 0})
				case 1:
					if i%2 == 0 {
						e.OnMpvEvent("pause")
					} else {
						e.OnMpvEvent("unpause")
					}
				case 2:
					e.OnRoom(room.Event{Type: "position", T: float64(i % 90), P: i%3 == 0})
				}
			}
		}(g)
	}
	wg.Wait() // -race: no unsynchronized access may surface here
}
