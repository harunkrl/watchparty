package sync

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
)

type fakePlayer struct {
	mu       sync.Mutex
	loads    []string
	pauses   []bool
	seeks    []float64
	speeds   []float64
	showText []string
	vols     []float64 // F5 local volume nudges
	mutes    int       // F5 mute toggles
	pos      *float64
	loadErr  error // returned by Load to simulate a failed loadfile
}

// pauseCount reads the recorded pauses safely: the post worker can apply
// a room event on its own goroutine (the cross-wired tests).
func (f *fakePlayer) pauseCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pauses)
}

// pausesSnapshot returns the recorded pauses safely.
func (f *fakePlayer) pausesSnapshot() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.pauses...)
}

func (f *fakePlayer) Load(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads = append(f.loads, name)
	return f.loadErr
}
func (f *fakePlayer) SetPause(p bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pauses = append(f.pauses, p)
	return nil
}
func (f *fakePlayer) Seek(t float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seeks = append(f.seeks, t)
	return nil
}
func (f *fakePlayer) Speed(v float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.speeds = append(f.speeds, v)
	return nil
}
func (f *fakePlayer) Pos() (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pos == nil {
		return 0, false
	}
	return *f.pos, true
}
func (f *fakePlayer) ShowText(s string, ms int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.showText = append(f.showText, s)
	return nil
}

// fakePost records the engine's events. The engine hands them to a post
// worker (the caller must never wait for the network — ISSUE-6), so the
// recorder is thread-safe and the tests wait for the posts to land.
type fakePost struct {
	mu     sync.Mutex
	events []room.Event
	at     time.Time // when the last event arrived
}

func (f *fakePost) post(ev room.Event) error {
	f.mu.Lock()
	f.events = append(f.events, ev)
	f.at = time.Now()
	f.mu.Unlock()
	return nil
}

// all waits until the engine's posts have settled and returns them: the
// window is at least 20ms (an absent post must be given time to arrive)
// and it ends 20ms after the last arrival.
func (f *fakePost) all(t *testing.T) []room.Event {
	t.Helper()
	start := time.Now()
	deadline := start.Add(2 * time.Second)
	for {
		f.mu.Lock()
		out := append([]room.Event(nil), f.events...)
		at := f.at
		f.mu.Unlock()
		if time.Since(start) > 20*time.Millisecond && time.Since(at) > 20*time.Millisecond {
			return out
		}
		if time.Now().After(deadline) {
			return out
		}
		time.Sleep(2 * time.Millisecond)
	}
}

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
	if len(po.all(t)) != 2 {
		t.Fatalf("events = %v, want 2 position heartbeats", evType(t, po.all(t)))
	}
	for i, want := range []float64{10, 12} {
		if got := po.all(t)[i]; got.Type != "position" || got.T != want || got.P {
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
	types := evType(t, po.all(t))
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
	for _, ev := range po.all(t) {
		if ev.Type == "seek" && ev.T == 20 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no seek event in %v", po.all(t))
	}
}

func TestMasterEndedOnIdleAfterActive(t *testing.T) {
	e, _, po := newEngine(true)
	e.OnMpvp(mpv.State{Pos: ptr(5)}) // active
	e.OnMpvp(mpv.State{Idle: true})  // EOF -> idle
	e.OnMpvp(mpv.State{Idle: true})  // still idle: ended fires once
	ended := 0
	for _, ev := range po.all(t) {
		if ev.Type == "ended" {
			ended++
		}
	}
	if ended != 1 {
		t.Fatalf("ended fired %d times, want 1 (events: %v)", ended, evType(t, po.all(t)))
	}
}

func TestEndedEmitsPausedUiState(t *testing.T) {
	for _, master := range []bool{true, false} {
		e, _, _ := newEngine(master)
		ui := make(chan UiState, 8)
		e.Ui = ui
		e.OnMpvp(mpv.State{Pos: ptr(5)}) // active
		e.OnMpvp(mpv.State{Idle: true})  // EOF
		var last UiState
		n := 0
		for {
			select {
			case last = <-ui:
				n++
				continue
			default:
			}
			break
		}
		if n == 0 {
			t.Fatalf("master=%v: no UI state emitted on EOF", master)
		}
		if !last.Paused || last.Pos != nil {
			t.Fatalf("master=%v: EOF UiState = %+v, want paused with no pos", master, last)
		}
	}
}

func lastUi(ui chan UiState) UiState {
	var last UiState
	for {
		select {
		case last = <-ui:
			continue
		default:
			return last
		}
	}
}

func TestFollowerUiTracksRoomPause(t *testing.T) {
	e, _, _ := newEngine(false)
	ui := make(chan UiState, 8)
	e.Ui = ui
	e.OnRoom(masterAt(5, true))                    // room paused
	e.OnMpvp(mpv.State{Pos: ptr(5), Pause: false}) // we still play: self-heal
	if last := lastUi(ui); !last.Paused {
		t.Fatalf("self-heal UiState = %+v, want paused", last)
	}
	e.OnMpvp(mpv.State{Pos: ptr(5), Pause: true}) // both paused: hold
	if last := lastUi(ui); !last.Paused || last.Pos == nil {
		t.Fatalf("hold UiState = %+v, want paused with pos", last)
	}
}

func TestMasterNothingLoadedIsSilent(t *testing.T) {
	e, _, po := newEngine(true)
	e.OnMpvp(mpv.State{}) // pos nil, not idle
	if len(po.all(t)) != 0 {
		t.Fatalf("events = %v, want none", evType(t, po.all(t)))
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

// Inside the smooth band the follower nudges tempo instead of seeking:
// behind -> speed up, ahead -> slow down, clamped to ±speedAdj.
func TestFollowerMidDriftSpeedNudges(t *testing.T) {
	e, pl, _ := newEngine(false)
	e.Clock = fakeClock{now: time.Unix(1000, 0), since: 3 * time.Second}
	e.OnRoom(masterAt(10, false))
	e.OnMpvp(mpv.State{Pos: ptr(12.5), Pause: false}) // expected 13: drift -0.5 -> 1.05
	if len(pl.speeds) != 1 || pl.speeds[0] != 1.05 {
		t.Fatalf("speeds = %v, want [1.05]", pl.speeds)
	}
	if len(pl.seeks) != 0 {
		t.Fatalf("no seek inside the band: %v", pl.seeks)
	}

	e2, pl2, _ := newEngine(false)
	e2.Clock = fakeClock{now: time.Unix(1000, 0), since: 3 * time.Second}
	e2.OnRoom(masterAt(10, false))
	e2.OnMpvp(mpv.State{Pos: ptr(13.5), Pause: false}) // drift +0.5 -> 0.95
	if len(pl2.speeds) != 1 || pl2.speeds[0] != 0.95 {
		t.Fatalf("ahead speeds = %v, want [0.95]", pl2.speeds)
	}
}

// Same drift on the next tick must not re-send the speed (IPC quiet).
func TestFollowerSpeedNudgeCached(t *testing.T) {
	e, pl, _ := newEngine(false)
	e.OnRoom(masterAt(10, false))
	e.OnMpvp(mpv.State{Pos: ptr(12.3), Pause: false}) // drift 0.3 -> 0.97
	e.OnRoom(masterAt(12, false))                     // heartbeat advances too
	e.OnMpvp(mpv.State{Pos: ptr(14.3), Pause: false}) // same drift again
	if len(pl.speeds) != 1 {                          // [0.97] only — second tick is silent
		t.Fatalf("speeds = %v, want exactly [0.97]", pl.speeds)
	}
}

// Hard seek beyond Tolerance; a later paused tick restores speed 1.
func TestFollowerBigDriftSeeksAndPauseResetsSpeed(t *testing.T) {
	e, pl, _ := newEngine(false)
	e.Clock = fakeClock{now: time.Unix(1000, 0), since: 5 * time.Second}
	e.OnRoom(masterAt(10, false))
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false}) // drift -5 -> seek 15
	if len(pl.seeks) != 1 || pl.seeks[0] != 15 {
		t.Fatalf("seeks = %v, want [15]", pl.seeks)
	}

	// master pauses; follower converges and the nudge must clear
	e.Clock = fakeClock{now: time.Unix(1010, 0), since: 0}
	e.OnRoom(room.Event{Type: "pause"})
	e.OnMpvp(mpv.State{Pos: ptr(15), Pause: false})
	e.OnMpvp(mpv.State{Pos: ptr(15), Pause: true}) // both paused: baseline speed
	last := pl.speeds[len(pl.speeds)-1]
	if last != 1 {
		t.Fatalf("speed after pause = %v, want 1", last)
	}
}

// Loading a new file clears any leftover tempo nudge.
func TestLoadResetsSpeed(t *testing.T) {
	e, pl, _ := newEngine(false)
	e.OnRoom(masterAt(10, false))
	e.OnMpvp(mpv.State{Pos: ptr(12.3), Pause: false}) // nudge 0.97
	e.OnRoom(room.Event{Type: "load", Name: "b.mkv"})
	last := pl.speeds[len(pl.speeds)-1]
	if last != 1 {
		t.Fatalf("speed after load = %v, want 1", last)
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
	if n := countType(t, po.all(t), "pause"); n != 1 {
		t.Fatalf("pause posted %d times, want 1 (events: %v)", n, evType(t, po.all(t)))
	}
}

func TestInstantSeekSinglePost(t *testing.T) {
	e, pl, po := newEngine(true)
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false}) // lastPos = 10
	pl.pos = ptr(60)                                // user scrubbed
	e.OnMpvEvent("seek")                            // instant path posts 60
	e.OnMpvp(mpv.State{Pos: ptr(60), Pause: false}) // poll scrub-check skipped
	if n := countType(t, po.all(t), "seek"); n != 1 {
		t.Fatalf("seek posted %d times, want 1 (events: %v)", n, evType(t, po.all(t)))
	}
	last := po.all(t)[len(po.all(t))-2] // seek followed by its heartbeat
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
	if len(po.all(t)) != 0 {
		t.Fatalf("follower posted without snapshot: %v", evType(t, po.all(t)))
	}
}

func TestLoadFailureLogged(t *testing.T) {
	logs, sink := logSink()
	e, pl, _ := newEngine(false)
	e.Logf = sink
	pl.loadErr = errors.New("load failed: nope.mkv")
	e.OnRoom(room.Event{Type: "load", Name: "nope.mkv"})
	if !hasLine(*logs, "> playing nope.mkv") || !hasLine(*logs, "mpv: load failed: nope.mkv") {
		t.Fatalf("failed load not surfaced: %v", *logs)
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
	if n := countType(t, po.all(t), "pause"); n != 1 {
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
	if len(po.all(t)) != 0 {
		t.Fatalf("posted a state the room already knows: %v", evType(t, po.all(t)))
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
	if n := countType(t, po.all(t), "seek"); n != 1 || po.all(t)[0].T != 60 {
		t.Fatalf("seek post wrong: %v", po.all(t))
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
	if len(po.all(t)) != 1 || po.all(t)[0].Type != "play" {
		t.Fatalf("unpause must post wire type play, got %v", evType(t, po.all(t)))
	}
}

func TestUserPauseRightAfterRemoteApplyPosts(t *testing.T) {
	e, _, po := newEngine(false)
	e.Clock = fakeClock{since: 0} // inside the actuate window on purpose
	e.OnRoom(masterAt(10, true))
	e.OnRoom(room.Event{Type: "play"}) // remote resume applied (actuated)
	e.OnMpvEvent("pause")              // user space-bar 200ms later: NOT an echo
	if n := countType(t, po.all(t), "pause"); n != 1 {
		t.Fatalf("user pause swallowed as echo (posts: %v)", evType(t, po.all(t)))
	}
}

func TestRemotePauseEchoSwallowed(t *testing.T) {
	e, _, po := newEngine(false)
	e.Clock = fakeClock{since: 0}
	e.OnRoom(masterAt(10, false))
	e.OnRoom(room.Event{Type: "pause"}) // remote pause applied -> setPause(true)
	e.OnMpvEvent("pause")               // the apply's own mpv echo
	if len(po.all(t)) != 0 {
		t.Fatalf("echo re-posted: %v", evType(t, po.all(t)))
	}
}

func TestMasterRemotePauseNoRepost(t *testing.T) {
	e, _, po := newEngine(true)
	e.Clock = fakeClock{since: 0}
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false}) // lastPause = false
	e.OnRoom(room.Event{Type: "pause"})             // friend paused; we apply
	e.OnMpvEvent("pause")                           // our mpv echo
	if n := countType(t, po.all(t), "pause"); n != 0 {
		t.Fatalf("master re-posted a remote application: %v", evType(t, po.all(t)))
	}
}

// waitFollowerSnap waits until the follower received the master's
// heartbeat (the cross-wire runs in the post worker).
func waitFollowerSnap(t *testing.T, e *Engine) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		e.mu.Lock()
		have := e.snap != nil
		e.mu.Unlock()
		if have {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the follower's snapshot never arrived")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitMasterApplied waits until the master applied the follower's pause
// (the cross-wire runs in the post worker).
func waitMasterApplied(t *testing.T, pl *fakePlayer) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for pl.pauseCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the master never applied the follower's pause")
		}
		time.Sleep(5 * time.Millisecond)
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
	waitFollowerSnap(t, fE)                          // the cross-wire runs in the post worker

	// follower's pause lands first (user acted before the master's event):
	// the post is asynchronous now (the caller must not wait for the
	// network — ISSUE-6), so the cross-wire is waited for explicitly
	fE.OnMpvEvent("pause") // posts; the master applies it when the post lands
	waitMasterApplied(t, mPl)
	mE.OnMpvEvent("pause") // master's own user pause: suppressed, room knows

	if n := countType(t, fPo.all(t), "pause"); n != 1 {
		t.Fatalf("follower posted %d pauses, want 1", n)
	}
	if n := countType(t, mPo.all(t), "pause"); n != 0 {
		t.Fatalf("master re-posted a pause the room already had: %v", evType(t, mPo.all(t)))
	}
	if ps := mPl.pausesSnapshot(); len(ps) != 1 || !ps[0] {
		t.Fatalf("master did not apply the follower's pause: %v", ps)
	}
	// settled polls must not start a storm
	mE.OnMpvp(mpv.State{Pos: ptr(10), Pause: true})
	fE.OnMpvp(mpv.State{Pos: ptr(10), Pause: true})
	if n := countType(t, fPo.all(t), "pause"); n != 1 || countType(t, mPo.all(t), "pause") != 0 {
		t.Fatalf("pause storm: master=%v follower=%v", evType(t, mPo.all(t)), evType(t, fPo.all(t)))
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
