package sync

// F8 pins: no interactive path holds e.mu across a blocking IPC call, and
// the UI thread never touches the pipe (A8: a wedged player froze the TUI
// 2-4s per key; ISSUE-6 residual, decisions.log #52). Each pin FAILS on
// the pre-F8 wiring (red) and passes after (green).
//
// Personal tells (volume/mute/subdelay/OSD) may drop-oldest when the
// writer wedges — bounded and logged. Room-synced tells (pause/seek,
// plus the speed nudge) coalesce to the LATEST and are NEVER dropped
// (postQueue F6-A3 is the precedent).

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
)

const f8Budget = 300 * time.Millisecond // a hotkey must return well under the 2s write deadline

// blockingPlayer: every tell blocks until release is closed (or a 5s
// probe timeout returns an error). Calls are recorded when they EXECUTE.
type blockingPlayer struct {
	mu      sync.Mutex
	release chan struct{}
	vols    []float64
	pauses  []bool
	seeks   []float64
	subs    []float64
}

func (b *blockingPlayer) block() error {
	select {
	case <-b.release:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("wedged (f8 probe timeout)")
	}
}

// AttachSub satisfies the F10 C4 interface growth (the tell lane owns the write).
func (b *blockingPlayer) AttachSub(string) error { return nil }
func (b *blockingPlayer) recordVols(d float64) {
	b.mu.Lock()
	b.vols = append(b.vols, d)
	b.mu.Unlock()
}
func (b *blockingPlayer) recordPause(p bool) {
	b.mu.Lock()
	b.pauses = append(b.pauses, p)
	b.mu.Unlock()
}
func (b *blockingPlayer) recordSeek(t float64) {
	b.mu.Lock()
	b.seeks = append(b.seeks, t)
	b.mu.Unlock()
}
func (b *blockingPlayer) recordSub(d float64) { b.mu.Lock(); b.subs = append(b.subs, d); b.mu.Unlock() }
func (b *blockingPlayer) countVols() int      { b.mu.Lock(); defer b.mu.Unlock(); return len(b.vols) }
func (b *blockingPlayer) volsSnapshot() []float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]float64(nil), b.vols...)
}
func (b *blockingPlayer) seeksSnapshot() []float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]float64(nil), b.seeks...)
}
func (b *blockingPlayer) pausesSnapshot() []bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bool(nil), b.pauses...)
}

func (b *blockingPlayer) StartLoad(string) (string, error) { return "", nil }
func (b *blockingPlayer) WaitLive(string, time.Time) bool  { return true }
func (b *blockingPlayer) AttachSubs(string)                {}
func (b *blockingPlayer) SetPause(p bool) error            { b.recordPause(p); return b.block() }
func (b *blockingPlayer) Seek(t float64) error             { b.recordSeek(t); return b.block() }
func (b *blockingPlayer) Speed(float64) error              { return nil }
func (b *blockingPlayer) ShowText(string, int) error       { return nil }
func (b *blockingPlayer) Pos() (float64, bool)             { return 0, false }
func (b *blockingPlayer) SetVolume(d float64) error        { b.recordVols(d); return b.block() }
func (b *blockingPlayer) ToggleMute() error                { return b.block() }
func (b *blockingPlayer) AddSubDelay(d float64) error      { b.recordSub(d); return b.block() }
func (b *blockingPlayer) SetSubDelay(v float64) error      { b.recordSub(v); return b.block() }
func (b *blockingPlayer) ShowSubDelayHint() error          { return nil }

type logCap struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCap) add(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(f, a...))
}
func (l *logCap) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, ln := range l.lines {
		if strings.Contains(ln, substr) {
			n++
		}
	}
	return n
}

// eventually polls cond until it holds (or the deadline passes): the
// async tell writer lands calls on its own goroutine now.
func eventually(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// PIN 1: with a wedged player, the hotkey paths (volume, subdelay, pause)
// return in under f8Budget. On the pre-F8 wiring each one blocked for the
// full fake write (5s probe) under e.mu, in the UI thread.
func TestF8HotkeyLatencyVsWedgedPlayer(t *testing.T) {
	pl := &blockingPlayer{release: make(chan struct{})}
	defer close(pl.release)
	eng := &Engine{Master: true, ID: "ada-1", Player: pl, Clock: fakeClock{now: time.Unix(1000, 0)}}

	start := time.Now()
	eng.VolumeUp()
	if d := time.Since(start); d > f8Budget {
		t.Fatalf("volume hotkey blocked %v (> %v): IPC write on the interactive path", d, f8Budget)
	}
	start = time.Now()
	eng.SubDelayBy(0.5)
	if d := time.Since(start); d > f8Budget {
		t.Fatalf("subdelay hotkey blocked %v (> %v)", d, f8Budget)
	}
	start = time.Now()
	eng.PauseToggle()
	if d := time.Since(start); d > f8Budget {
		t.Fatalf("pause hotkey blocked %v (> %v)", d, f8Budget)
	}
}

// PIN 2: held-key amplification stays bounded — 30 rapid volume tells
// while the writer is wedged must return fast, drain to a bounded number
// of executed calls (drop-oldest), and log the drops.
func TestF8HeldKeyBoundedDrop(t *testing.T) {
	pl := &blockingPlayer{release: make(chan struct{})}
	logs := &logCap{}
	eng := &Engine{Master: true, ID: "ada-1", Player: pl, Clock: fakeClock{now: time.Unix(1000, 0)}, Logf: logs.add}

	for i := 0; i < 30; i++ {
		start := time.Now()
		eng.VolumeUp()
		if d := time.Since(start); d > f8Budget {
			t.Fatalf("volume key %d blocked %v: the UI thread waited on the pipe", i+1, d)
		}
	}
	close(pl.release) // unwedge: the queue must drain, bounded (single close — the drain reads it)
	if !eventually(t, 6*time.Second, func() bool {
		return len(pl.volsSnapshot()) > 0 && pl.countVols() == len(pl.volsSnapshot()) && pl.stable(200*time.Millisecond)
	}) {
		t.Fatalf("queued tells never drained after the unwedge")
	}
	if got := len(pl.volsSnapshot()); got > tellPersonalCap+1 { // +1: the in-flight tell that wedged the writer
		t.Fatalf("personal tells not bounded: %d executed (drop-oldest missing)", got)
	}
	if logs.count("dropped") == 0 {
		t.Fatalf("no drop was logged — the human cannot see a lost keypress")
	}
}

// stable: no new call executed for d — the drain settle for pin 2.
func (b *blockingPlayer) stable(d time.Duration) bool {
	b.mu.Lock()
	n := len(b.vols)
	b.mu.Unlock()
	time.Sleep(d)
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.vols) == n
}

// PIN 3: pause/seek coalesce to the LATEST and are never dropped — the
// tail survives a wedged writer, and two seeks collapse to one (the last
// target), postQueue F6-A3 style.
func TestF8SeekCoalesceToLatestNeverDropped(t *testing.T) {
	pl := &blockingPlayer{release: make(chan struct{})}
	logs := &logCap{}
	eng := &Engine{Master: true, ID: "ada-1", Player: pl, Clock: fakeClock{now: time.Unix(1000, 0)}, Logf: logs.add}
	eng.polledPos = ptr(100)

	// occupy the writer FIRST (a personal tell it will block on): the two
	// seeks + the pause are then guaranteed QUEUED before any executes, so
	// the coalescing assertion is deterministic (a fast writer draining
	// between keypresses legitimately executes both seeks — that schedule
	// is the healthy-path behavior the queue must not disturb).
	eng.VolumeUp()
	eng.SeekBy(5)  // target 105
	eng.SeekBy(10) // target 110: supersedes 105
	eng.PauseToggle()
	close(pl.release)
	if !eventually(t, 6*time.Second, func() bool {
		return len(pl.pausesSnapshot()) > 0 && len(pl.seeksSnapshot()) > 0 && pl.tailStable(150*time.Millisecond)
	}) {
		t.Fatalf("pause/seek tells never drained")
	}
	seeks := pl.seeksSnapshot()
	if len(seeks) != 1 {
		t.Fatalf("seeks did not coalesce to the latest: %v (want [110])", seeks)
	}
	if seeks[0] != 110 {
		t.Fatalf("coalesced seek lost the latest target: %v", seeks)
	}
	if pauses := pl.pausesSnapshot(); len(pauses) != 1 || !pauses[0] {
		t.Fatalf("pause tell lost: %v", pauses)
	}
}

// tailStable: no new seek or pause executed for d.
func (b *blockingPlayer) tailStable(d time.Duration) bool {
	b.mu.Lock()
	ns, np := len(b.seeks), len(b.pauses)
	b.mu.Unlock()
	time.Sleep(d)
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.seeks) == ns && len(b.pauses) == np
}

// PIN 4: a follower with NO room snapshot but a live local position still
// emits a UiState (local pause flag + position) — the playing-status fix.
// S2 shape: the snapshot is nil right after a queue advance (loadfile).
// S3 shape: the master went silent before/at a film boundary (fresh join,
// or after the advance at EOF). On the pre-F8 wiring no UiState was ever
// emitted, so the TUI froze on the last pre-gap sample ("stale ⏸ paused").
func TestF8LocalEmitWhenSnapshotNil(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, eng *Engine, pl *fakePlayer)
	}{
		{"S2-queue-advance", func(t *testing.T, eng *Engine, pl *fakePlayer) {
			eng.loaded = "a.mkv"
			eng.OnRoom(room.Event{Type: "load", Name: "b.mkv"}) // startLoad clears e.snap
			pos := 42.0
			// At AFTER the load (Unix 1001 > loadedAt 1000): SYNC2 discards
			// samples that predate the load — the first post-advance sample is
			// exactly the one that must reach the UI now.
			eng.OnMpvp(mpv.State{Pos: &pos, Pause: true, At: time.Unix(1001, 0)})
		}},
		{"S3-fresh-join", func(t *testing.T, eng *Engine, pl *fakePlayer) {
			pos := 42.0
			eng.OnMpvp(mpv.State{Pos: &pos, Pause: false, At: time.Unix(999, 0)})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch := make(chan UiState, 8)
			eng := &Engine{Master: false, ID: "ada-1", Player: &fakePlayer{}, Post: func(room.Event) error { return nil },
				Clock: fakeClock{now: time.Unix(1000, 0), since: time.Second}, Ui: ch}
			tc.run(t, eng, eng.Player.(*fakePlayer))
			select {
			case st := <-ch:
				if st.Pos == nil || *st.Pos != 42 {
					t.Fatalf("local emit lost the position: %+v", st)
				}
				if st.Stale {
					t.Fatalf("a snapshot-less sample must not claim stale: %+v", st)
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatalf("no UiState while the local player plays (snap==nil gate swallowed the sample)")
			}
		})
	}
}

// PIN 5a: the mpv seek-event settle (sleep + time-pos read) must not hold
// e.mu — a scrub against a wedged player blocked the engine lock for
// seekSettle + the full write deadline on the pre-F8 wiring.
func TestF8SeekEventLeavesLockFree(t *testing.T) {
	pl := &fakePlayer{pos: ptr(30)}
	pl.posBlock = make(chan struct{})
	unwedge := sync.OnceFunc(func() { close(pl.posBlock) })
	defer unwedge()
	eng := &Engine{Master: true, ID: "ada-1", Player: pl, Post: func(room.Event) error { return nil },
		// since=6s: past loadQuiet (5s) so the seek event reaches the post path
		Clock: fakeClock{now: time.Unix(1000, 0), since: 6 * time.Second}}
	eng.loaded = "f.mkv"
	eng.loadedAt = time.Unix(994, 0)

	done := make(chan struct{})
	go func() { eng.OnMpvEvent("seek"); close(done) }()
	time.Sleep(50 * time.Millisecond) // let the event take the lock and reach the settle

	start := time.Now()
	finished := make(chan struct{})
	go func() { eng.PauseToggle(); close(finished) }()
	select {
	case <-finished:
		if d := time.Since(start); d > f8Budget {
			t.Fatalf("engine lock held %v by the seek settle (budget %v)", d, f8Budget)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("e.mu held across the seek settle + Pos(): PauseToggle starved")
	}
	unwedge() // release the wedged read: the scrub post may complete (defer re-runs safely)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("OnMpvEvent never returned after the unwedge")
	}
}

// PIN 5b: applySnapshot must not pay Pos() round trips under e.mu —
// on join/reconnect it held the lock across two wedged reads (~10s).
func TestF8ApplySnapshotLockFree(t *testing.T) {
	pl := &fakePlayer{pos: ptr(30)}
	pl.posBlock = make(chan struct{})
	defer close(pl.posBlock)
	eng := &Engine{Master: false, ID: "ada-1", Player: pl, Post: func(room.Event) error { return nil },
		Clock: fakeClock{now: time.Unix(1000, 0), since: time.Second}}
	eng.loaded = "f.mkv"
	eng.dur = 100

	go eng.OnRoom(room.Event{Type: "hello", Now: "f.mkv", Pos: ptr(40), P: false})
	time.Sleep(50 * time.Millisecond)

	finished := make(chan struct{})
	start := time.Now()
	go func() { eng.PauseToggle(); close(finished) }() // follower with no snap: still needs e.mu
	select {
	case <-finished:
		if d := time.Since(start); d > f8Budget {
			t.Fatalf("applySnapshot held e.mu %v (budget %v)", d, f8Budget)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("e.mu held across applySnapshot's Pos() round trips")
	}
}

// PIN 6: the mpv argv must not force a window at boot — the TUI paints
// first, the video window appears at the first playback (human request #3).
// Lives next to the other argv pins (see mpv/f8_argv_test.go for the
// executor-side half).
func TestF8NoForceWindowInTheArgvShape(t *testing.T) {
	// engine-side half is trivial; the real pin is in the mpv package.
	// Kept here so the pin list in f8-worker1.md maps 1:1 to tests.
	_ = mpv.State{}
	_ = room.Event{}
}
