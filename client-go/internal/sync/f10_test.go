package sync

// F10 client pins (engine half) — each was RED on the v1.9.1 wiring
// (evidence: .pi-state/analysis10/*.md + /tmp/a10w1r phase-2 re-runs).
// C1 narrows the machinery gate (W2-01 + M2-09, one root); C2 rolls back
// a failed pause/play post (M2-05); C3 clears a failed load's claim
// (W2-05); C6 carries the small honesty fixes (M2-06 baseline, W2-04
// scrub log, W2-02 overflow drop log).

import (
	"errors"
	"sync"
	"testing"
	"time"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
)

// f10Post counts attempts and can fail every post (the outage probe).
type f10Post struct {
	mu       sync.Mutex
	attempts int
	fail     bool
	got      []room.Event
}

func (p *f10Post) post(ev room.Event) error {
	p.mu.Lock()
	p.attempts++
	fail, n := p.fail, len(p.got)
	p.mu.Unlock()
	if fail {
		return errors.New("net down")
	}
	p.mu.Lock()
	p.got = append(p.got, ev)
	p.mu.Unlock()
	_ = n
	return nil
}

func (p *f10Post) tried() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts
}

func (p *f10Post) wire() []room.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]room.Event(nil), p.got...)
}

func waitF10(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("never settled: %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// PIN F10.1 (C1 / W2-01): a genuine lone-follower pause INSIDE the load
// window must reach the room once the next poll proves mpv's real state.
// On v1.9.1 the flat 5s swallow ate it forever (room_pause RUN1: the room
// stayed paused:false and a master join silently resumed the user).
func TestF10C1GenuineWindowPausePostsAtPoll(t *testing.T) {
	e, _, po := newEngine(false)
	pos := 10.0
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false}) // playing alone
	waitTells(t, e)
	e.mu.Lock()
	e.loaded, e.loadedAt = "f.mkv", e.Clock.Now()
	e.mu.Unlock()

	e.OnMpvEvent("pause") // space in the mpv window, 2s after the load
	waitTells(t, e)
	if got := evType(t, po.all(t)); len(got) != 0 {
		t.Fatalf("the hold must not post before a poll proves the state: %v", got)
	}

	// the next poll: mpv REALLY paused (the user did it) → the held claim
	// reconciles and posts
	e.OnMpvp(mpv.State{Pos: &pos, Pause: true, Dur: 120, At: time.Now()})
	waitTells(t, e)
	if got := evType(t, po.all(t)); len(got) != 1 || got[0] != "pause" {
		t.Fatalf("C1: the genuine window pause never reached the room: %v", got)
	}
}

// PIN F10.2 (C1 / M2-09): the delayed machinery unpause (after the first
// duration-bearing poll materialized the snapshot) must NOT post "play"
// to a paused room: the poll's sample contradicts the claim → loadfile
// noise, discarded silently.
func TestF10C1PostSnapMachineryUnpauseDiscarded(t *testing.T) {
	e, _, po := newEngine(false)
	pos := 10.0
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false})
	waitTells(t, e)
	e.OnRoom(room.Event{Type: "hello", Now: "f.mkv", Pos: &pos, P: true}) // adopt paused
	waitTells(t, e)
	e.OnMpvp(mpv.State{Pos: &pos, Pause: true, Dur: 120, At: time.Now()}) // flush → snap
	waitTells(t, e)

	e.OnMpvEvent("unpause") // the delayed machinery claim (inside the window)
	waitTells(t, e)
	if got := evType(t, po.all(t)); len(got) != 0 {
		t.Fatalf("C1: the machinery unpause posted before any proof: %v", got)
	}
	e.OnMpvp(mpv.State{Pos: &pos, Pause: true, Dur: 120, At: time.Now().Add(time.Second)})
	waitTells(t, e)
	if got := evType(t, po.all(t)); len(got) != 0 {
		t.Fatalf("C1: the machinery unpause survived the reconciliation: %v", got)
	}
}

// PIN F10.13 (master-2's pre-actuation race, CONFIRMED-mechanism): a held
// claim must not be proved against a sample taken while the room's own
// actuation is still landing — that sample is PRE-actuation (it disagrees
// with the pending SetPause). Pre-fix the reconcile matched the stale
// "playing" sample and posted `play` to a PAUSED room (the room resumed,
// then the actuation landed and re-paused the mpv under it — the M2-05
// class, born from the reconcile). The skip keeps the hold; the claim is
// proved (and discarded) against the TRUE state once the actuation lands.
func TestF10C1ActuationPendingReconcileSkips(t *testing.T) {
	e, _, po := newEngine(false)
	pos := 10.0
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false})
	waitTells(t, e)
	e.OnRoom(room.Event{Type: "hello", Now: "f.mkv", Pos: &pos, P: true}) // the room is PAUSED — actuation issued
	waitTells(t, e)
	e.OnMpvp(mpv.State{Pos: &pos, Pause: true, Dur: 120, At: time.Now()}) // flush → snap
	waitTells(t, e)

	e.OnMpvEvent("unpause") // machinery noise lands inside the window → held
	waitTells(t, e)
	// the poll samples "playing": the pause actuation has NOT landed yet
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false, Dur: 120, At: time.Now().Add(time.Second)})
	waitTells(t, e)
	if got := evType(t, po.all(t)); len(got) != 0 {
		t.Fatalf("pre-actuation race: the engine posted %v to a PAUSED room (the sample was pre-actuation)", got)
	}

	// the actuation lands — the next sample is the TRUE state; the claim
	// (unpause) mismatches and dies, silently
	e.OnMpvp(mpv.State{Pos: &pos, Pause: true, Dur: 120, At: time.Now().Add(2 * time.Second)})
	waitTells(t, e)
	if got := evType(t, po.all(t)); len(got) != 0 {
		t.Fatalf("the stale claim survived into the true state: %v", got)
	}
}

// PIN F10.3 (F9 regression guard): the observer-registration initial event
// at ENGINE START (no file, loadedAt zero) stays discarded — a held claim
// here could un-pause a persisted room.
func TestF10C1EngineStartInitialNeverPosts(t *testing.T) {
	e, _, po := newEngine(false)
	e.OnMpvEvent("pause")
	waitTells(t, e)
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false, At: time.Now()})
	waitTells(t, e)
	if got := evType(t, po.all(t)); len(got) != 0 {
		t.Fatalf("F9 regression: the engine-start initial posted: %v", got)
	}
}

// PIN F10.4 (F9 regression guard): outside the window the mpv-window pause
// posts IMMEDIATELY (no hold) and the echo dedup still works.
func TestF10C1OutsideWindowPauseImmediate(t *testing.T) {
	e, _, po := newEngine(false)
	// anchorClock (F9.2's lesson): the window age (6s: closed) and the echo
	// grace (0s: inside) must read DIFFERENT values — the fixed fakeClock
	// cannot express both.
	e.Clock = anchorClock{now: time.Unix(1000, 0)}
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false})
	waitTells(t, e)
	e.mu.Lock()
	e.loaded, e.loadedAt = "f.mkv", time.Unix(994, 0) // fakeClock.since=6s: window closed
	e.mu.Unlock()

	e.OnMpvEvent("pause")
	waitTells(t, e)
	if got := evType(t, po.all(t)); len(got) != 1 || got[0] != "pause" {
		t.Fatalf("F9 regression: the outside-window pause did not post: %v", got)
	}
	e.OnMpvEvent("pause") // our own actuation echo: same value, in grace
	waitTells(t, e)
	if got := evType(t, po.all(t)); len(got) != 1 {
		t.Fatalf("F9 regression: the echo double-posted: %v", got)
	}
}

// PIN F10.5 (C2 / M2-05): a failed pause post rolls the mirrors back and
// says so — the next toggle re-posts freely (no echo gate, no stickiness).
func TestF10C2FailedPausePostRollsBack(t *testing.T) {
	logs := &logCap{}
	pl := &fakePlayer{}
	g := &f10Post{fail: true}
	e, pl, _ := newEngine(false)
	e.Logf, e.Post = logs.add, g.post
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false})
	waitTells(t, e)

	e.PauseToggle() // the POST fails
	waitTells(t, e)
	// wait for the RETRY STAMP, not the attempt: tried()==1 can be observed
	// in a tiny window before onPostFailed stamps (the flake master-2's
	// -race stress caught). F11 C1 r2: the failure STAMPS the direction —
	// postedPause stays set by design (the echo guards stay armed).
	waitF10(t, "the failed attempt and its stamp", func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return g.tried() == 1 && e.retryTyp == "pause"
	})
	// the refusal line is logged by the POST worker AFTER the stamp (F11 C1
	// order): the stamp wait does not imply the line landed — poll it
	// (time.Sleep(50ms) here lost to taskset contention on slow runners)
	waitF10(t, "the refusal line", func() bool {
		return logs.count("never reached the room") > 0
	})
	if got := pl.showTextSnapshot(); len(got) == 0 {
		t.Fatal("C2: no OSD refusal for the failed pause")
	}

	// F11 C1 (A11-F1, master-2's design) updated the contract AGAIN: the
	// failure restores NOTHING — the mpv stays paused, the failure stamps
	// the retry direction, and the retry posts the SAME direction the user
	// intended (pause). v1.9.2-F11v1's "posts play" and F11v1's "restores
	// the mpv" were both superseded.
	tells := pl.pausesSnapshot()
	if len(tells) != 1 || !tells[0] {
		t.Fatalf("C2/F11: the failure must leave the mpv exactly as pressed: %v", tells)
	}

	g.fail = false
	e.PauseToggle() // the retry: must post PAUSE (the intended direction)
	waitTells(t, e)
	waitF10(t, "the retry", func() bool { return g.tried() == 2 })
	wire := g.wire()
	if len(wire) == 0 || wire[len(wire)-1].Type != "pause" {
		t.Fatalf("C2: the second toggle did not re-post the intended pause: %v", wire)
	}
}

// PIN F10.6 (C3 / W2-05): a failed load clears the speculative claim — a
// same-file hello MUST trigger the retry (the promised heal becomes real).
func TestF10C3FailedLoadHelloRetries(t *testing.T) {
	logs := &logCap{}
	pl := &f10FailStart{fail: errors.New("mpv wedged")}
	e := &Engine{Master: false, ID: "f", Player: pl, Logf: logs.add,
		Clock: fakeClock{now: time.Unix(1000, 0), since: 2 * time.Second}}

	e.OnRoom(room.Event{Type: "hello", Now: "x.mkv"})
	waitTells(t, e)
	e.mu.Lock()
	claimed := e.loaded
	e.mu.Unlock()
	if claimed != "" {
		t.Fatalf("C3: the failed load kept the claim: %q", claimed)
	}
	e.OnRoom(room.Event{Type: "hello", Now: "x.mkv"}) // the heal
	waitTells(t, e)
	if n := pl.attempt; n != 2 {
		t.Fatalf("C3: the same-file hello did not retry the load (%d attempts)", n)
	}
}

type f10FailStart struct {
	fakePlayer
	fail    error
	attempt int
}

func (p *f10FailStart) StartLoad(name string) (string, error) {
	p.mu.Lock()
	p.attempt++
	p.mu.Unlock()
	if p.fail != nil {
		return "", p.fail
	}
	return p.fakePlayer.StartLoad(name)
}

// PIN F10.7 (C6 / M2-06): a master adopting hello.p=true gets the toggle
// baseline from the room — the first toggle posts "play" (the "> pause by
// M" wrong-direction re-post is dead).
func TestF10C6MasterHelloAdoptionBaseline(t *testing.T) {
	e, pl, po := newEngine(true)
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false})
	waitTells(t, e)
	e.OnRoom(room.Event{Type: "hello", P: true}) // joining a paused room
	waitTells(t, e)
	if pl.pauseCount() != 1 {
		t.Fatalf("precondition: the room's pause was not adopted: %v", pl.pausesSnapshot())
	}

	e.PauseToggle()
	waitTells(t, e)
	var got []string
	for _, ty := range evType(t, po.all(t)) {
		if ty != "position" { // the master's own heartbeats are legitimate noise
			got = append(got, ty)
		}
	}
	if len(got) != 1 || got[0] != "play" {
		t.Fatalf("C6: the first toggle after an adopted pause posted %v (want [play])", got)
	}
}

// PIN F10.8 (C6 / W2-04): an mpv-EVENT scrub inside the load window is
// swallowed WITH a log line (the lone-scrub skip's shape).
func TestF10C6SwallowedScrubLogs(t *testing.T) {
	logs := &logCap{}
	pl := &fakePlayer{pos: ptr(30)}
	e := &Engine{Master: false, ID: "ada-1", Player: pl, Logf: logs.add,
		Clock: fakeClock{now: time.Unix(1000, 0), since: 2 * time.Second}}
	e.OnMpvp(mpv.State{Pos: ptr(30), Pause: false})
	waitTells(t, e)
	e.loaded, e.loadedAt = "f.mkv", time.Unix(998, 0)

	e.OnMpvEvent("seek")
	waitTells(t, e)
	waitF10(t, "the swallowed scrub line", func() bool {
		return logs.count("scrub") > 0
	})
}

// PIN F10.9 (W2-02): the postQueue overflow drop is LOUD — the oldest
// pending post dies with a logged line (v1.9.1 dropped silently).
func TestF10OverflowDropIsLogged(t *testing.T) {
	// Drive the postQueue DIRECTLY (master-2's verified shape, ×50 stable):
	// through the Engine, the async post worker drains CONCURRENTLY with the
	// enqueue loop, so the overflow itself was timing-dependent — on a fast
	// runner the queue never crossed the cap and the drop never fired (the
	// count=5 flake). No worker here: postQueueMax+2 pushes overflow for
	// certain. The drop note is DETACHED (go q.logf — F11 C5): poll it.
	logs := &logCap{}
	q := newPostQueue(logs.add)
	for i := 0; i < postQueueMax+2; i++ {
		q.push(room.Event{Type: "chat", Text: "m"})
	}
	waitF10(t, "the overflow drop line", func() bool {
		return logs.count("dropped") > 0
	})
}
