package sync

// F11 pins (client lane, base fec9807): every pin ports its A11 probe and
// ran RED against v1.9.2 before the fix landed (failing-test-first law).

import (
	"errors"
	"sync"
	"testing"
	"time"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
)

// f11Post records EVERY attempt (even failed ones — the direction of a
// failed post is exactly what C1 argues about) and fails on demand.
type f11Post struct {
	mu       sync.Mutex
	fail     bool
	attempts int
	events   []string
	gate     chan struct{} // when set, the POST hangs here (in-flight window)
}

func (p *f11Post) post(ev room.Event) error {
	p.mu.Lock()
	p.attempts++
	p.events = append(p.events, ev.Type)
	fail := p.fail
	gate := p.gate
	p.mu.Unlock()
	if gate != nil {
		<-gate // the POST is in flight: the room can move before it fails
	}
	if fail {
		return errors.New("net down")
	}
	return nil
}

func (p *f11Post) log() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.events...)
}

func (p *f11Post) triedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts
}

func p2f(p float64) *float64 { return &p }

// waitPostCountF11 polls the post log until it holds at least want entries.
// The post worker is a separate goroutine from the tell worker: waitTells
// returns when the ACTUATION drained, which does not imply the POST landed
// (the CI flake on slow -race runners). Deadlines hard-fail.
func waitPostCountF11(t *testing.T, g *f11Post, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n := 0
		for _, x := range g.log() {
			if x == "pause" || x == "play" {
				n++
			}
		}
		if n >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the post queue never reached %d pause/play entries: %v", want, g.log())
}

// waitRollbackF11 waits until the failed post has been ATTEMPTED and the
// retry stamp is on (the F11 contract: nothing is "rolled back" — the
// failure's observable is the stamp).
func waitRollbackF11(t *testing.T, e *Engine, g *f11Post, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		stamped := e.retryTyp != ""
		e.mu.Unlock()
		if g.triedCount() >= want && stamped {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the failure never stamped the retry (attempts=%d)", g.triedCount())
}

// PIN F11.1 (C1 / A11-F1, the headline, master-2's design): a failed human
// pause/play post does NOT restore anything — the mpv keeps the pressed
// state, the mirrors keep the toggle's writes, and the failure stamps
// e.retryTyp; the OSD-invited retry press takes its direction from THAT
// STAMP (never from a mirror), so it posts the SAME direction the user
// intended. v1.9.2-F11v1 restored mirrors and the retry inverted or
// clobbered a concurrent room pause (master-2's two defects).
func TestF11C1RetryKeepsIntentSnapshotFollower(t *testing.T) {
	e, pl, _ := newEngine(false)
	g := &f11Post{fail: true}
	e.Post = g.post
	pos := 10.0
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false})
	waitTells(t, e)
	e.OnRoom(room.Event{Type: "hello", Now: "f.mkv", Pos: &pos, P: false}) // the room plays
	waitTells(t, e)
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false, Dur: 120, At: time.Now()})
	waitTells(t, e)
	base := len(pl.pausesSnapshot()) // includes C2's load-reset play tell

	e.PauseToggle() // intent: PAUSE. The post fails; the local mpv STAYS paused.
	waitRollbackF11(t, e, g, 1)

	// F11.1a hardening (master-2): the failure touches NO mirror — the
	// toggle's localPause=true write STANDS (a restore-of-localPause
	// mutation must bite here), and the mpv was not touched again.
	tells := pl.pausesSnapshot()
	if len(tells) != base+1 || !tells[len(tells)-1] {
		t.Fatalf("C1: the failure must leave the mpv exactly as the user pressed it (one pause tell after the load reset %v): %v", tells[:base], tells)
	}
	// the echo guards STAY ARMED (a re-added postedPause/lastPost clear
	// mutation bites here): the press's own late echo must stay consumed.
	e.mu.Lock()
	armed := e.postedPause != nil && !e.lastPost.IsZero()
	stamp := e.retryTyp
	e.mu.Unlock()
	if !armed {
		t.Fatal("C1: the failure disarmed the echo guards (postedPause/lastPost)")
	}
	if stamp != "pause" {
		t.Fatalf("C1: the failed direction was not stamped: %q", stamp)
	}

	g.mu.Lock()
	g.fail = false
	g.mu.Unlock()
	e.PauseToggle() // the OSD-invited retry — must be PAUSE again
	waitPostCountF11(t, g, 2)
	got := g.log()
	if len(got) < 2 || got[0] != "pause" || got[len(got)-1] != "pause" {
		t.Fatalf("C1: the retry inverted the intent: posted %v, want [pause pause]", got)
	}
}

// The lone-follower branch (worker2's A11-W2-01 variant): the 2nd pause
// press must not RESUME the film — press 1's rollback already returns the
// mpv to playing (the room's truth, the refusal on screen), press 2 re-posts
// the same pause.
func TestF11C1RetryKeepsIntentLoneFollower(t *testing.T) {
	e, pl, _ := newEngine(false)
	g := &f11Post{fail: true}
	e.Post = g.post
	pos := 10.0
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false}) // lone: no snapshot
	waitTells(t, e)

	e.PauseToggle() // intent: PAUSE. Fails — the mpv STAYS paused (no restore).
	waitRollbackF11(t, e, g, 1)
	tells := pl.pausesSnapshot()
	if len(tells) != 1 || !tells[0] {
		t.Fatalf("C1: the lone-follower failure must leave the mpv as pressed: %v", tells)
	}
	e.mu.Lock()
	lp := e.localPause
	e.mu.Unlock()
	if !lp {
		t.Fatal("C1: the failure restored the lone branch's localPause mirror")
	}

	g.mu.Lock()
	g.fail = false
	g.mu.Unlock()
	e.PauseToggle() // the retry: PAUSE again
	waitPostCountF11(t, g, 2)
	got := g.log()
	if len(got) < 2 || got[0] != "pause" || got[len(got)-1] != "pause" {
		t.Fatalf("C1: the lone-follower retry inverted the intent: posted %v", got)
	}
}

// The master branch: lastPause is a touched mirror too — restored on
// failure, so the retry posts the same direction.
func TestF11C1RetryKeepsIntentMaster(t *testing.T) {
	e, _, _ := newEngine(true)
	g := &f11Post{fail: true}
	e.Post = g.post
	pos := 10.0
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false})
	waitTells(t, e)
	e.OnRoom(room.Event{Type: "hello", Now: "f.mkv", Pos: &pos, P: false}) // the room plays
	waitTells(t, e)

	e.PauseToggle() // intent: PAUSE (lastPause was false → want true). Fails.
	// master-2's condition (a): the stamp drives BOTH the direction and the
	// actuation — the master branch's !lastPause trap must not recompute it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		stamped := e.retryTyp == "pause"
		e.mu.Unlock()
		if g.triedCount() >= 1 && stamped {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	g.mu.Lock()
	g.fail = false
	g.mu.Unlock()
	e.PauseToggle()
	waitPostCountF11(t, g, 2)
	var pauses []string
	for _, x := range g.log() {
		if x == "pause" || x == "play" {
			pauses = append(pauses, x)
		}
	}
	if len(pauses) < 2 || pauses[0] != "pause" || pauses[len(pauses)-1] != "pause" {
		t.Fatalf("C1: the master retry inverted the intent: posted %v", g.log())
	}
}

// PIN F11.2 (C2 / A11-F2): a file change plays the new file LOCALLY —
// startLoad resets the pause mirror and actuates play within ONE poll (no
// grace wait, no room post; the server's S5 already reset the room).
// v1.9.2 carried the old film's pause until followerTick's self-heal
// expired the grace (~4-6s of a frozen new file — the A11-W2-02 window).
func TestF11C2LoadPlaysNewFileImmediately(t *testing.T) {
	e, pl, _ := newEngine(false)
	pos := 10.0
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false})
	waitTells(t, e)
	e.OnRoom(room.Event{Type: "hello", Now: "a.mkv", Pos: &pos, P: false}) // the room plays
	waitTells(t, e)
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false, Dur: 120, At: time.Now()})
	waitTells(t, e)

	e.PauseToggle() // the user paused: posts "pause", actuates SetPause(true), stamps lastPost
	waitTells(t, e)
	base := len(pl.pausesSnapshot())

	// the room skips to the next file WHILE the pause post is inside its grace
	// (the server's S5 reset the room's pause: the new file plays)
	e.OnRoom(room.Event{Type: "load", From: "M", Nick: "m", Name: "b.mkv"})
	// the heartbeat carries the room's new playing state
	e.OnRoom(room.Event{Type: "position", T: 1, P: false})
	// ONE poll tick: the new file must already be un-paused locally
	e.OnMpvp(mpv.State{Pos: p2f(0.1), Pause: true, Dur: 180, At: time.Now()})
	waitTells(t, e)
	tells := pl.pausesSnapshot()[base:]
	if len(tells) == 0 {
		t.Fatal("C2: the load did not actuate play on the new file within one poll (the old film's pause carried)")
	}
	for _, p := range tells {
		if p {
			t.Fatalf("C2: the load's reset is not a play (SetPause(true) after the load; tells: %v)", tells)
		}
	}
}

// The followerTick self-heal stays as the backstop for divergences that
// have no file change and no discrete event (a lost SSE pause): inside the
// grace it must NOT fire, after the grace it must.
func TestF11C2HealWindowBackstop(t *testing.T) {
	e, pl, _ := newEngine(false)
	e.Clock = RealClock{} // the grace is real time: the canned fakeClock.since would keep it open forever
	pos := 10.0
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false})
	waitTells(t, e)
	e.OnRoom(room.Event{Type: "hello", Now: "f.mkv", Pos: &pos, P: true})
	waitTells(t, e)
	e.OnMpvp(mpv.State{Pos: &pos, Pause: true, Dur: 120, At: time.Now()})
	waitTells(t, e)

	e.PauseToggle() // press play: posts, actuates, stamps lastPost
	waitTells(t, e)
	base := len(pl.pausesSnapshot())

	// the room re-paused but the discrete pause EVENT was lost (an SSE gap):
	// only the heartbeats carry it
	e.OnRoom(room.Event{Type: "position", T: 12, P: true})
	// a tick INSIDE the grace: no self-heal yet
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false, Dur: 120, At: time.Now()})
	waitTells(t, e)
	if n := len(pl.pausesSnapshot()) - base; n != 0 {
		t.Fatalf("the heal fired inside the grace (self-correction raced our own post): %d tells", n)
	}
	// the grace expires: the next tick converges to the room's truth
	time.Sleep(3200 * time.Millisecond)
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false, Dur: 120, At: time.Now()})
	waitTells(t, e)
	tells := pl.pausesSnapshot()[base:]
	if len(tells) == 0 || !tells[len(tells)-1] {
		t.Fatalf("the heal never converged to the room's paused state: %v", tells)
	}
}

// PIN F11.3 (C3 / A11-F3): the reconcile skips while the actuation TELL is
// IN FLIGHT — a completion signal, not the 3s enqueue clock. The red shape
// (two wedged tells outlasting the box) is reproduced by a SetPause that
// blocks past the 3s box: v1.9.2 proved the held claim against the stale
// pre-actuation sample and posted play to a paused room (master-2's race,
// re-opened by the box expiry).
type blockingPausePlayer struct {
	fakePlayer
	saw      []bool
	releases []chan struct{} // one channel per SetPause, in call order
}

// hold queues a release channel for the NEXT SetPause call.
func (p *blockingPausePlayer) hold(ch chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.releases = append(p.releases, ch)
}

func (p *blockingPausePlayer) SetPause(paused bool) error {
	p.mu.Lock()
	p.saw = append(p.saw, paused)
	var ch chan struct{}
	if len(p.releases) > 0 {
		ch = p.releases[0]
		p.releases = p.releases[1:]
	}
	p.mu.Unlock()
	if ch != nil {
		<-ch
	}
	return nil
}

func TestF11C3ReconcileSkipsWhileActuationInFlight(t *testing.T) {
	rel := make(chan struct{})
	blk := &blockingPausePlayer{}
	blk.hold(rel)
	g := &f11Post{}
	e := &Engine{Master: false, ID: "w1", Player: blk, Post: g.post,
		Clock: RealClock{}, Logf: func(string, ...any) {}}
	pos := 10.0
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false})
	time.Sleep(100 * time.Millisecond) // the seed's tells (no blocking kinds)
	// the hello's pause actuation tell is ENQUEUED — the worker blocks
	// inside SetPause for the whole scenario (pending the entire time).
	// The load is PRE-REGISTERED so the hello takes the same-file adoption
	// path: exactly ONE pause tell is enqueued. A fresh load would enqueue
	// the C2 reset (play) a few microseconds before the adoption (pause) —
	// the tell worker can pop the reset in that window and defeat the
	// coalesce-to-latest, leaving saw[0]=false (the CI flake on slow
	// -race runners; the product gate holds in BOTH interleavings).
	e.mu.Lock()
	e.loaded = "f.mkv"
	e.loadedAt = e.Clock.Now()
	e.mu.Unlock()
	e.OnRoom(room.Event{Type: "hello", Now: "f.mkv", Pos: &pos, P: true})
	time.Sleep(150 * time.Millisecond) // let the tell worker reach the blocked write

	e.OnMpvEvent("unpause") // machinery noise inside the window: held
	time.Sleep(100 * time.Millisecond)

	// a poll INSIDE the 3s box: skipped either way
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false, Dur: 120, At: time.Now()})
	time.Sleep(100 * time.Millisecond)

	// the box EXPIRES while the actuation is still in flight — v1.9.2
	// proved the claim against this pre-actuation sample and posted play
	time.Sleep(3200 * time.Millisecond)
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false, Dur: 120, At: time.Now()})
	time.Sleep(100 * time.Millisecond)
	if got := g.log(); len(got) != 0 {
		t.Fatalf("C3: the reconcile acted while the actuation tell was still in flight: posted %v", got)
	}

	// the actuation lands: the true state proves the claim WRONG (discard)
	close(rel)
	time.Sleep(150 * time.Millisecond) // the tell worker finishes the write
	e.OnMpvp(mpv.State{Pos: &pos, Pause: true, Dur: 120, At: time.Now()})
	time.Sleep(100 * time.Millisecond)
	if got := g.log(); len(got) != 0 {
		t.Fatalf("C3: the expired claim survived the true state: %v", got)
	}
	if len(blk.saw) == 0 || !blk.saw[0] {
		t.Fatalf("C3: the scenario lost its pending pause actuation: %v", blk.saw)
	}
}

// PIN F11.4 (C4 / A11-F6): the /sub OSD rides the attach tell — shown only
// on SUCCESS. v1.9.2 painted "sub X" at enqueue and buried a failing attach
// in the system pane (the W2-06 class the volume fix already closed).
func TestF11C4SubOSDRidesTheTell(t *testing.T) {
	e, pl, _ := newEngine(false)
	pl.mu.Lock()
	pl.attachErr = errors.New("probe: sub-add failed")
	pl.mu.Unlock()
	e.SubAttach("movie.tr.srt")
	waitTells(t, e)
	if got := pl.showTextSnapshot(); len(got) != 0 {
		t.Fatalf("C4: a FAILED attach still announced itself on the OSD: %v", got)
	}

	pl.mu.Lock()
	pl.attachErr = nil // the sub becomes attachable: the retry succeeds
	pl.mu.Unlock()
	e.SubAttach("movie.tr.srt")
	waitTells(t, e)
	got := pl.showTextSnapshot()
	if len(got) != 1 || got[0] != "sub movie.tr.srt" {
		t.Fatalf("C4: the successful attach lost its OSD: %v", got)
	}
}

// PIN F11.5 (C5 / A11-F7): the postQueue drop log must not run under q.mu —
// a blocking logf (the headless fmt.Printf full-pipe stall) must not hold
// the queue hostage. v1.9.2 logged INSIDE the critical section: push never
// returned and q.mu stayed held.
func TestF11C5DropLogLeavesTheQueueLock(t *testing.T) {
	q := newPostQueue(func(string, ...any) {}) // quiet filler
	for i := 0; i < postQueueMax; i++ {
		q.push(room.Event{Type: "chat", Text: "m"})
	}
	release := make(chan struct{})
	q.logf = func(string, ...any) { <-release }

	done := make(chan struct{})
	go func() {
		q.push(room.Event{Type: "chat", Text: "m"}) // overflow → drop → log
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("C5: push blocked inside the drop log (q.mu held through logf)")
	}
	if !q.mu.TryLock() {
		q.mu.Unlock()
		t.Fatal("C5: q.mu is still held after push returned")
	}
	close(release)
}

// PIN F11.6 (C1 defect 1, master-2's M2-P1): the failure must not create a
// new mpv echo source nor disarm the existing echo guards. The v1-F11
// rollback ACTUATED a restore (a new SetPause whose own echo, arriving past
// actuateQuiet, posted a stray "play" — probe-proven) and CLEARED
// postedPause/lastPost (the guards that consume the user's own late echo).
// The final design does neither: exactly one SetPause tell (the press's
// own), the guards stay armed, and the late own echo is consumed silently.
func TestF11C1LateOwnEchoIsConsumed(t *testing.T) {
	e, pl, _ := newEngine(false)
	g := &f11Post{fail: true}
	e.Post = g.post
	e.Logf = func(string, ...any) {}
	pos := 10.0
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false})
	waitTells(t, e)
	e.OnRoom(room.Event{Type: "hello", Now: "f.mkv", Pos: &pos, P: false}) // a loaded file: the machinery window is live
	waitTells(t, e)
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false, Dur: 120, At: time.Now()})
	waitTells(t, e)
	base := len(pl.pausesSnapshot()) // includes C2's load-reset play tell

	e.PauseToggle() // pause: the mpv pauses, the post fails
	waitRollbackF11(t, e, g, 1)

	// the guards stay armed: no restore tell, no mirror clear
	tells := pl.pausesSnapshot()
	if len(tells) != base+1 {
		t.Fatalf("C1: the failure actuated a restore (a new echo source): %v", tells)
	}
	e.mu.Lock()
	armed := e.postedPause != nil && !e.lastPost.IsZero()
	e.mu.Unlock()
	if !armed {
		t.Fatal("C1: the rollback disarmed the echo guards (postedPause/lastPost cleared)")
	}

	// the mpv's own "pause" echo arrives LATE (past the 1s echo gate —
	// a loaded machine or a wedged event pipe): consumed, no stray post
	time.Sleep(1200 * time.Millisecond)
	e.OnMpvEvent("pause")
	time.Sleep(100 * time.Millisecond)
	if got := g.log(); len(got) != 1 {
		t.Fatalf("C1: the user's own late echo posted a stray event: %v", got)
	}
}

// PIN F11.7 (C1 defect 2, master-2's M2-P4): a room pause that lands while
// the failed post is IN FLIGHT must survive the rollback — the failure
// stamps the retry, it never rewrites snap.paused (the room's truth moved;
// "pre-toggle = the room's truth" only held while the room stood still).
func TestF11C1ConcurrentRoomPauseSurvives(t *testing.T) {
	e, _, _ := newEngine(false)
	g := &f11Post{fail: true} // the post must FAIL after the gate: the rollback is the code under test
	gate := make(chan struct{})
	g.gate = gate
	e.Post = g.post
	pos := 10.0
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false})
	waitTells(t, e)
	e.OnRoom(room.Event{Type: "hello", Now: "f.mkv", Pos: &pos, P: false}) // the room plays
	waitTells(t, e)
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false, Dur: 120, At: time.Now()})
	waitTells(t, e)

	e.PauseToggle() // the POST hangs in flight (g.gate)
	time.Sleep(100 * time.Millisecond)
	// the room pauses CONCURRENTLY (a heartbeat carries it)
	e.OnRoom(room.Event{Type: "position", T: 12, P: true})
	close(gate) // the post now fails → the stamp runs
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		st := e.retryTyp
		e.mu.Unlock()
		if st == "pause" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	e.mu.Lock()
	sp := e.snap == nil
	if e.snap != nil {
		sp = e.snap.paused
	}
	stamp := e.retryTyp
	e.mu.Unlock()
	if !sp {
		t.Fatalf("C1: the rollback clobbered the room's concurrent pause (snap.paused=false; stamp=%q)", stamp)
	}
	if stamp != "pause" {
		t.Fatalf("C1: the failed direction was not stamped: %q", stamp)
	}
}

// PIN F11.8 (C3, master-2's M2-P2): the completion signal must survive a
// SECOND in-flight pause tell — the first completion recomputes from the
// QUEUE (the second tell is still queued/executing), so the reconcile gate
// stays closed. The v1 boolean dropped on the first completion and the
// A11-M2-02 race fired (posted [play]).
func TestF11C3SecondPauseTellKeepsTheGateShut(t *testing.T) {
	// TWO blocked tells: tell #1 must complete while tell #2 is IN FLIGHT —
	// that is exactly the window where the v1 boolean dropped the gate and
	// the A11-M2-02 race posted [play]. The 3s actuation box is AGED past
	// expiry so the stale gate cannot mask the boolean (master-2's first
	// probe was masked by exactly that).
	rel1, rel2 := make(chan struct{}), make(chan struct{})
	blk := &blockingPausePlayer{}
	blk.hold(rel1)
	blk.hold(rel2)
	g := &f11Post{}
	e := &Engine{Master: false, ID: "w1", Player: blk, Post: g.post,
		Clock: RealClock{}, Logf: func(string, ...any) {}}
	pos := 10.0
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false})
	time.Sleep(100 * time.Millisecond)
	// tell #1: the hello's pause actuation — the worker BLOCKS inside it.
	// The load is PRE-REGISTERED (same-file adoption path): a fresh load's
	// C2 reset tell could be popped before the adoption tell lands, killing
	// the coalesce and leaving saw[0]=false (the CI flake class).
	e.mu.Lock()
	e.loaded = "f.mkv"
	e.loadedAt = e.Clock.Now()
	e.mu.Unlock()
	e.OnRoom(room.Event{Type: "hello", Now: "f.mkv", Pos: &pos, P: true})
	time.Sleep(150 * time.Millisecond)

	e.OnMpvEvent("unpause") // machinery noise inside the window: held
	time.Sleep(100 * time.Millisecond)

	// tell #2: a foreign discrete pause enqueues WHILE tell #1 executes
	// (the room re-asserts its pause; the queue is FIFO — it lands after #1)
	e.OnRoom(room.Event{Type: "pause", From: "M", Nick: "m"})
	time.Sleep(100 * time.Millisecond)

	// tell #1 completes: the recompute must see tell #2 (queued → then IN
	// FLIGHT) and KEEP the gate shut — the enqueue-stamped 3s box is aged
	// past expiry so only the completion signal can hold the gate
	close(rel1)
	time.Sleep(200 * time.Millisecond) // tell #1 done, tell #2 pops and blocks
	e.mu.Lock()
	e.lastActuate = e.lastActuate.Add(-4 * time.Second)
	e.mu.Unlock()

	// a poll with a pre-actuation sample (the actuations say pause; mpv
	// still reports false): the gate must be shut
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false, Dur: 120, At: time.Now()})
	time.Sleep(100 * time.Millisecond)
	if got := g.log(); len(got) != 0 {
		t.Fatalf("C3: the gate opened while the second pause tell was in flight: posted %v", got)
	}
	// tell #2 completes: the true state proves the held claim wrong (discard)
	close(rel2)
	time.Sleep(200 * time.Millisecond)
	e.OnMpvp(mpv.State{Pos: &pos, Pause: true, Dur: 120, At: time.Now()})
	time.Sleep(100 * time.Millisecond)
	if got := g.log(); len(got) != 0 {
		t.Fatalf("C3: the expired claim survived the true state: %v", got)
	}
	if len(blk.saw) < 2 || !blk.saw[0] || !blk.saw[1] {
		t.Fatalf("C3: the scenario lost its two blocked actuations: %v", blk.saw)
	}
}

// PIN F11.9 (C3, condition d): a NON-pause tell's completion must not
// recompute/clear the pause gate. Guard pin (green on arrival by design —
// it pins the binding condition, not a regression).
func TestF11C3OnlyPauseCompletionsRecompute(t *testing.T) {
	rel := make(chan struct{})
	blk := &blockingPausePlayer{}
	blk.hold(rel)
	g := &f11Post{}
	e := &Engine{Master: false, ID: "w1", Player: blk, Post: g.post,
		Clock: RealClock{}, Logf: func(string, ...any) {}}
	pos := 10.0
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false})
	time.Sleep(100 * time.Millisecond)
	e.OnRoom(room.Event{Type: "hello", Now: "f.mkv", Pos: &pos, P: true})
	time.Sleep(150 * time.Millisecond) // the pause tell blocks (pending)
	e.OnMpvEvent("unpause")            // machinery noise: held
	time.Sleep(100 * time.Millisecond)

	e.VolumeUp() // a NON-pause tell completes while the pause tell is in flight
	time.Sleep(100 * time.Millisecond)

	e.OnMpvp(mpv.State{Pos: &pos, Pause: false, Dur: 120, At: time.Now()})
	time.Sleep(100 * time.Millisecond)
	if got := g.log(); len(got) != 0 {
		t.Fatalf("C3: a volume completion dropped the pause gate: posted %v", got)
	}
	close(rel)
	time.Sleep(150 * time.Millisecond)
}

// PIN F11.10 (C1, condition b + iii): the retry stamp clears on every path
// where the room supersedes the user's intent — a foreign discrete pause,
// the self-heal convergence — and survives a failure of the retry itself.
func TestF11C1StampClearsWhenRoomSupersedes(t *testing.T) {
	e, pl, _ := newEngine(false)
	// anchorClock (the trap comment in engine_test.go): the canned fakeClock
	// since=2s keeps followerGrace open forever — the heal can never fire.
	e.Clock = anchorClock{now: time.Unix(5000, 0)}
	g := &f11Post{fail: true}
	e.Post = g.post
	pos := 10.0
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false})
	waitTells(t, e)
	e.OnRoom(room.Event{Type: "hello", Now: "f.mkv", Pos: &pos, P: false})
	waitTells(t, e)
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false, Dur: 120, At: time.Now()})
	waitTells(t, e)

	e.PauseToggle() // fails → stamped
	waitRollbackF11(t, e, g, 1)
	e.mu.Lock()
	if e.retryTyp != "pause" {
		e.mu.Unlock()
		t.Fatal("C1: the failure did not stamp the retry")
	}
	e.mu.Unlock()

	// path 1: a FOREIGN discrete pause supersedes the user's intent
	e.OnRoom(room.Event{Type: "pause", From: "M", Nick: "m"})
	e.mu.Lock()
	cleared := e.retryTyp == ""
	e.mu.Unlock()
	if !cleared {
		t.Fatal("C1: the foreign pause did not clear the retry stamp")
	}

	// path 2: the heartbeat-driven self-heal supersedes the stamp. Get a
	// LIVE stamp: press play (the room is paused — the intent is play); the
	// post fails → stamped "play", the mpv plays. The standing divergence
	// (snap.paused=true from the foreign pause vs the playing mpv) then
	// converges via the self-heal once the grace expires: the mpv returns
	// to the room's truth AND the stamp clears (the adoption supersedes).
	e.PauseToggle() // press play: fails → stamped "play"; the mpv plays;
	// the optimistic snap.paused=write says playing — the ROOM's next
	// heartbeat re-asserts the truth (a lost discrete pause event)
	waitRollbackF11(t, e, g, g.triedCount())
	e.OnRoom(room.Event{Type: "position", T: 14, P: true})
	e.mu.Lock()
	e.lastPost = e.Clock.Now().Add(-4 * time.Second) // the grace expires
	e.mu.Unlock()
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false, Dur: 120, At: time.Now()})
	waitTells(t, e) // the heal actuates pause=true (the room's truth)
	e.mu.Lock()
	cleared = e.retryTyp == ""
	pauses := pl.pausesSnapshot()
	healed := len(pauses) > 0 && pauses[len(pauses)-1]
	e.mu.Unlock()
	if !cleared || !healed {
		t.Fatalf("C1: the self-heal did not adopt the room's pause (cleared=%v healed=%v tells=%v)", cleared, healed, pauses)
	}

	// the stamp also survives a failed press: after the heal the room is
	// paused and the mpv paused — a NORMAL press would post play (the
	// inversion); seed the stamp (a failed press) and check the NEXT press
	// re-posts the stamped direction and re-stamps on failure
	e.mu.Lock()
	e.retryTyp = "pause" // the failed press's stamp
	e.mu.Unlock()
	e.PauseToggle()
	waitRollbackF11(t, e, g, g.triedCount())
	e.mu.Lock()
	restamped := e.retryTyp == "pause"
	e.mu.Unlock()
	if !restamped {
		t.Fatal("C1: a failed retry did not re-stamp the direction")
	}
}

// The three clearing paths master-2's M7/M10/M12 called out as pinless:
// the stamp must NOT outlive the thing it retries.

func TestF11C1SuccessfulPostClearsStamp(t *testing.T) {
	e, pl, _ := newEngine(false)
	pos := 10.0
	e.OnRoom(room.Event{Type: "hello", Now: "f.mkv", Pos: &pos, P: false})
	waitTells(t, e)

	// a failed press left the stamp alive
	e.mu.Lock()
	e.retryTyp = "pause"
	e.mu.Unlock()

	// the RETRY lands: the press re-posts pause and the post SUCCEEDS —
	// the stamp must be spent (a lingering stamp would hijack the press
	// AFTER the retry into re-posting pause forever)
	e.PauseToggle()
	waitTells(t, e)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		stamp := e.retryTyp
		e.mu.Unlock()
		if stamp == "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.mu.Lock()
	tells := pl.pausesSnapshot()
	stamp := e.retryTyp
	e.mu.Unlock()
	t.Fatalf("M7: the successful post did not spend the stamp (%q, tells=%v)", stamp, tells)
}

func TestF11C1FileChangeClearsStamp(t *testing.T) {
	e, _, _ := newEngine(false)
	pos := 10.0
	e.OnRoom(room.Event{Type: "hello", Now: "f.mkv", Pos: &pos, P: false})
	waitTells(t, e)

	// a failed press left the stamp alive — then the room changes the film
	e.mu.Lock()
	e.retryTyp = "pause"
	e.mu.Unlock()
	e.OnRoom(room.Event{Type: "hello", Now: "other.mkv", Pos: &pos, P: false})

	e.mu.Lock()
	stamp := e.retryTyp
	e.mu.Unlock()
	if stamp != "" {
		t.Fatalf("M10: the file change did not clear the stamp (%q) — the next press would re-post the old film's direction", stamp)
	}
}

func TestF11C1HelloAdoptionClearsStamp(t *testing.T) {
	e, _, _ := newEngine(false)
	pos := 10.0
	e.OnRoom(room.Event{Type: "hello", Now: "f.mkv", Pos: &pos, P: false})
	waitTells(t, e)

	// a failed press left the stamp alive — then a hello re-adopts the
	// room's snapshot (same file: no startLoad, the adoption branch)
	e.mu.Lock()
	e.retryTyp = "pause"
	e.mu.Unlock()
	e.OnRoom(room.Event{Type: "hello", Now: "f.mkv", Pos: &pos, P: true})

	e.mu.Lock()
	stamp := e.retryTyp
	e.mu.Unlock()
	if stamp != "" {
		t.Fatalf("M12: the hello adoption did not clear the stamp (%q) — the adopted room truth lost to a stale retry", stamp)
	}
}

// PIN M7b: the postLoop success-clear's real reachable window — the user's
// stamp is consumed by their own next press BEFORE any post completes, so
// the only way a successful pause/play post lands while a stamp is live is
// a MACHINERY post (the follower reconcile) racing it. That completion must
// spend the stamp too: the room adopted the direction by any means.
func TestF11C1MachineryPostSpendsLiveStamp(t *testing.T) {
	e, _, _ := newEngine(false)
	// anchorClock (the trap in engine_test.go): the canned fakeClock since
	// keeps every real-time window open — here we NEED the load-settled
	// regime (the pause event posts directly, not held as load noise).
	e.Clock = anchorClock{now: time.Unix(5000, 0)}
	g := &f11Post{}
	e.Post = g.post
	pos := 10.0
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false})
	waitTells(t, e)
	e.OnRoom(room.Event{Type: "hello", Now: "f.mkv", Pos: &pos, P: false})
	waitTells(t, e)

	// a failed press left the stamp alive; age the load window CLOSED
	// (anchorClock's frozen Now makes every since 0 — manual aging is how
	// anchor scenarios move time; the trap comment in engine_test.go)
	e.mu.Lock()
	e.retryTyp = "pause"
	e.loadedAt = e.Clock.Now().Add(-6 * time.Second)
	e.mu.Unlock()

	// the load is settled: the mpv's own pause event posts DIRECTLY (not
	// held as load noise) — the room is playing (P:false) so the follower
	// pause post goes out and SUCCEEDS; that completion spends the stamp
	e.OnMpvEvent("pause")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		stamp := e.retryTyp
		e.mu.Unlock()
		if stamp == "" && g.triedCount() >= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.mu.Lock()
	stamp := e.retryTyp
	e.mu.Unlock()
	t.Fatalf("M7b: the successful machinery pause post did not spend the live stamp (%q, attempts=%d)", stamp, g.triedCount())
}
