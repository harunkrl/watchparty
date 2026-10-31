// Package sync holds the master/follower sync engine: pure logic, no UI
// dependencies, Clock injected so tests simulate minutes instantly (E4).
package sync

import (
	"fmt"
	"sync"
	"time"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
)

const (
	PollSecs           = 2.0 // mpv poll period (master heartbeat cadence)
	Tolerance          = 1.0 // follower drift threshold before re-seek
	SeekScrubThreshold = 3.0 // |pos - last_pos - PollSecs| beyond this = scrub
	volStep            = 5.0 // F5 local volume nudge (ctrl+up/down), percent
)

// postQueueMax bounds the wire backlog (F6-A3): a wedged server must not
// grow the queue without end. Past the bound the oldest item dies — a
// queued position dies before any discrete event (the next heartbeat
// supersedes it anyway).
const postQueueMax = 64

// loadSettle bounds the off-lock half of a file change (F6-A1): how long
// continueLoad waits for mpv to take the file (mirrors mpv.loadWait).
const loadSettle = 5 * time.Second

// Smooth-sync band: inside (speedBand, Tolerance] the follower nudges mpv's
// speed instead of issuing a visible seek; the clamp keeps the tempo change
// inaudible (mpv audio-pitch-correction is on by default).
const (
	speedBand = 0.15
	speedAdj  = 0.05
)

// Dual-control gates (v1.4): followers also post discrete events; these
// keep remote-application echoes from re-posting (loop prevention).
const (
	// mpv delivers property-change echoes in ~50ms; 1s headroom covers
	// slow machines. shortcut: if mpv ever lags past 1s, user actions
	// right after remote ones could be swallowed — self-heals via poll.
	actuateQuiet = time.Second
	// master heartbeats land within one POLL_SECS tick; +1s network slack
	followerGrace = time.Duration(PollSecs+1.0) * time.Second
	seekSettle    = 150 * time.Millisecond // let time-pos reach the seek target
	loadQuiet     = 5 * time.Second        // no seek posts right after a load: mpv's head-of-file event is machinery, not a scrub
)

// Stall detection (R1): a follower persistently behind the live position
// is starving — typically a WAN link below the file's bitrate. A healthy
// player converges one tick after a catch-up seek; one that lands behind
// again on the very next tick is not playing at all. Two consecutive
// behind-live ticks trigger; mpv's paused-for-cache flag counts instantly
// but rarely fires while the seek loop keeps churning the cache (live
// measurement: .pi-state/wan-baseline.md).
const stallTicks = 2 // consecutive behind-live polls before a stall

// staleAge: a follower whose snapshot is older than this is no longer
// hearing the master's heartbeat — the room may be gone (or the master's
// SSE died). The UI says so instead of showing a confident drift.
const staleAge = 6 * time.Second

// Batch C (m2-S1, m2-S2): the stall detector may only fire against a
// LIVE master, and a resume is not a starvation.
//   - resumeJump: a drift this big means we were away (sleep, suspend, a
//     long outage), not that the room is starving. The follower re-seeks
//     once and never pauses the room for it.
//   - resumeBound: how long the resume episode may last. The episode
//     follows the CACHE (M2-b): while mpv is still filling there is no
//     seek storm and no room pause, and it ends as soon as the follower
//     converges. The bound is the honest backstop: a resume that never
//     converges must still reach the normal stall logic, or a broken link
//     would be pause-proof forever.
//     shortcut: a fixed 2 minutes — a refill slower than that is treated
//     as real starvation (raise it if the WAN baseline says otherwise).
const (
	resumeJump = 30.0
	// resumeBound: the honest backstop for a resume episode (M2-b).
	resumeBound = 120 * time.Second
	// endedRetry: how many polls between two 'ended' posts from a
	// follower whose master is dead. The server accepts the post only
	// for an ownerless room and releases the dead master's claim on its
	// own schedule (CLAIM_FRESH_MS) — the client learns of the death
	// earlier (staleAge) — so the post is repeated until the room takes
	// it. The episode ends when the room answers.
	endedRetry = 5
)

// Clock is the engine's time source; fake it in tests.
type Clock interface {
	Now() time.Time
	Since(time.Time) time.Duration
}

type RealClock struct{}

func (RealClock) Now() time.Time                  { return time.Now() }
func (RealClock) Since(t time.Time) time.Duration { return time.Since(t) }

// Player is the engine's view of mpv; *mpv.Player satisfies it.
type Player interface {
	// F6-A1: the load chain is split — StartLoad is the ONLY load step
	// allowed under the engine lock (a bounded tell); the wait for the file
	// to go live and the subtitle attach continue off it (guaranteed-signal
	// re-poll, never the drop-on-full events channel).
	StartLoad(name string) (string, error)
	WaitLive(name string, deadline time.Time) bool
	AttachSubs(name string)
	SetPause(paused bool) error
	Seek(t float64) error
	Speed(f float64) error
	ShowText(text string, ms int) error
	Pos() (float64, bool)
	// F5 local audio keys: nudge volume / cycle mute. The engine never
	// posts these — audio belongs to the viewer's ears, not the room.
	SetVolume(delta float64) error
	ToggleMute() error
}

// UiState feeds the M4 status bar (drift etc); non-blocking send, dropped
// when nobody consumes yet.
type UiState struct {
	Drift    float64  // follower pos - master expected, seconds
	Pos      *float64 // local playback position (master: own, follower: own)
	Paused   bool
	Cache    float64 // seconds of read-ahead buffered (demuxer-cache-time)
	Volume   float64 // audio volume percent (display only)
	Muted    bool    // mpv mute flag (display only)
	Stale    bool    // follower: the master's heartbeat is older than staleAge
	PausedBy string  // who paused the room ("" = our own action or unknown)
	// Mechanical marks a pause the room did not ask for: a stall the
	// follower detected on its own link. It names no human (m2-S2).
	Mechanical bool
	Sub        string // language of the active subtitle track (personal only)
	Layout     string // audio channel layout ("stereo"; "" = unknown)
}

type Engine struct {
	Master bool
	ID     string // our client id: a notice naming it demotes a second master
	Player Player
	Post   func(room.Event) error
	Clock  Clock
	Ui     chan<- UiState
	Logf   func(format string, args ...any) // nil = discard

	postOnce sync.Once  // the post worker starts with the first event
	posts    *postQueue // the events waiting for the network (ISSUE-6)

	lastPos   *float64
	lastPause bool
	muted     bool     // last polled mpv mute flag: the mute log names the state we expect
	polledPos *float64 // last polled mpv position: SeekBy's target without an IPC round trip (F6-A2)
	wasActive bool
	pausedBy  string    // who paused the room (the relay's nick); "" = our own action
	snap      *snapshot // follower: latest master position heartbeat

	loaded     string    // file our mpv is playing ("" = none; set on successful load)
	loadedAt   time.Time // when mpv took the file: post-load seek posts are noise
	behind     int       // follower: consecutive polls behind live (starvation signal)
	autoPaused bool      // follower: WE paused the room over a stall
	resumed    bool      // follower: a resume episode is OPEN (the cache is refilling)
	resumeSeen bool      // follower: a resume-sized jump is still unresolved (no NEW episode until it converges)
	resumeTil  time.Time // follower: the resume episode ends here (the honest bound)
	mechPause  bool      // the room is paused for a MECHANICAL reason (a stall), not a human's
	endedAt    time.Time // follower: when we last posted 'ended' for the current file

	dur          float64  // the locally known media duration (0 = unknown yet)
	pendingPos   *float64 // a hello position waiting for the duration to clamp it
	pendingPause bool     // the paused flag that came with the pending position

	lastActuate  time.Time // last programmatic SetPause/Seek (echo gate)
	actuatePause *bool     // pause state that actuation set (nil = seek); an
	// mpv event is only an echo when it matches this — the user pressing
	// space right after a remote apply must never be swallowed
	lastPost time.Time // last follower discrete post (grace window)
	// postedPause is the pause state this follower posted and the room has
	// not confirmed yet. A heartbeat that was already in flight overwrites
	// the snapshot mirror (snap.paused), so this pending state is what
	// keeps a late echo of our own apply from posting the same state twice.
	postedPause *bool

	speedSet  bool    // a Speed value has been sent (0.0 is a valid speed)
	lastSpeed float64 // current tempo nudge; cache avoids IPC spam per tick

	seekEcho   bool      // one mpv seek event per programmatic seek is ours (echo)
	lastSeekAt time.Time // when the programmatic seek was issued (echo expiry)

	mu sync.Mutex // guards engine state: poll + mpv-event + SSE goroutines
}

type snapshot struct {
	t      float64
	paused bool
	at     time.Time // when the snapshot was anchored (drift math; re-anchored on resume)
	beat   time.Time // when the last heartbeat arrived (the stale check uses this)
}

func (e *Engine) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

// post1 hands the event to the post worker: the CALLER must never wait
// for the network. The UI thread called the engine synchronously, and a
// dead server froze the whole interface for the 5s POST timeout
// (ISSUE-6, F4). The worker keeps the order (one queue, one worker) and
// the room's state never waits for us.
func (e *Engine) post1(ev room.Event) error {
	if e.Post == nil {
		return nil
	}
	e.postOnce.Do(func() {
		e.posts = newPostQueue()
		go e.postLoop()
	})
	e.posts.push(ev)
	return nil
}

// postLoop is the post worker: it owns the network for the engine.
func (e *Engine) postLoop() {
	for {
		ev, ok := e.posts.pop()
		if !ok {
			return
		}
		if err := e.Post(ev); err != nil {
			e.logf("post: %s", err)
			e.onPostFailed(ev)
		}
		e.posts.done()
	}
}

// onPostFailed reacts to an event that did not land. The stall pause
// rides the same starving link that caused the stall — so a failure is
// the EXPECTED one there: re-arm, or the follower never seeks again and
// never pauses the room: it would stay behind forever while the room
// plays on (W3).
func (e *Engine) onPostFailed(ev room.Event) {
	if ev.Type != "pause" || !ev.Mechanical {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.autoPaused {
		return // already re-armed or superseded: the state is not ours
	}
	if e.snap != nil && !e.snap.paused {
		return // the room moved on: do not clear a newer state
	}
	e.autoPaused = false
	if e.snap != nil {
		e.snap.paused = false
	}
	e.mechPause = false
}

// postQueue is an unbounded FIFO of events waiting for the network. It
// never blocks its producer: the room's events must not be dropped (a
// lost pause is a lost room state), and the queue only grows while the
// server is unreachable.
type postQueue struct {
	mu    sync.Mutex
	items []room.Event
	busy  bool // a post is in flight
	sig   chan struct{}
}

func newPostQueue() *postQueue {
	return &postQueue{sig: make(chan struct{}, 1)}
}

// push appends an event and wakes the worker.
// F6-A3: a queued position is superseded by any newer one — the heartbeat
// flood collapses to exactly the latest position, landed at the TAIL so
// the discrete events (seeks/pauses, which keep their order) execute
// first; a pure discrete flood clamps at postQueueMax (the oldest item
// dies, a queued position dying before any discrete).
func (q *postQueue) push(ev room.Event) {
	q.mu.Lock()
	if ev.Type == "position" {
		// F6-A3: the heartbeat flood collapses to the LATEST position,
		// landed at the tail (after the discrete events, which keep their
		// order)
		keep := q.items[:0]
		for _, it := range q.items {
			if it.Type != "position" {
				keep = append(keep, it)
			}
		}
		q.items = append(keep, ev)
	} else {
		q.items = append(q.items, ev)
	}
	for len(q.items) > postQueueMax {
		q.items = q.items[1:] // F6-A3: the oldest dies; the coalesced tail
		// position only reaches here once 64 newer events made it stale
	}
	q.mu.Unlock()
	select {
	case q.sig <- struct{}{}:
	default:
	}
}

// pop takes the next event, waiting for one to arrive.
func (q *postQueue) pop() (room.Event, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			ev := q.items[0]
			q.items = q.items[1:]
			q.busy = true
			q.mu.Unlock()
			return ev, true
		}
		q.mu.Unlock()
		<-q.sig
	}
}

// done marks the in-flight post as finished.
func (q *postQueue) done() {
	q.mu.Lock()
	q.busy = false
	q.mu.Unlock()
}

// idle reports that nothing is queued and nothing is in flight.
func (q *postQueue) idle() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items) == 0 && !q.busy
}

// pending counts the events that are queued or in flight.
func (q *postQueue) pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.items)
	if q.busy {
		n++
	}
	return n
}

// Drain waits until the queued events are on the wire, bounded by the
// timeout: a hung network must not lock the exit. It reports whether the
// queue drained; a lost event is logged — the human must know that the
// last action did not reach the room (the master ruling: DRAIN).
func (e *Engine) Drain(timeout time.Duration) bool {
	// the Once gives the happens-before: the queue is created (and the
	// worker started) exactly once, whoever gets here first
	e.postOnce.Do(func() {
		e.posts = newPostQueue()
		go e.postLoop()
	})
	deadline := time.Now().Add(timeout)
	for {
		if e.posts.idle() {
			return true
		}
		if time.Now().After(deadline) {
			e.logf("post: %d event(s) lost — the room did not answer", e.posts.pending())
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// setPause/seekTo mark lastActuate so the mpv events they trigger are
// recognized as our own echoes and never re-posted.
func (e *Engine) setPause(paused bool) {
	e.lastActuate = e.Clock.Now()
	p := paused
	e.actuatePause = &p
	_ = e.Player.SetPause(paused)
}

func (e *Engine) seekTo(t float64) {
	if t < 0 {
		t = 0 // the server rejects a negative target: never seek before the head
	}
	e.lastActuate = e.Clock.Now()
	e.actuatePause = nil // seek echo arrives as a "seek" mpv event
	e.lastSeekAt = e.lastActuate
	e.seekEcho = true // swallow exactly one echo (see OnMpvEvent)
	_ = e.Player.Seek(t)
}

// setSpeed applies a tempo nudge once per distinct value; repeated ticks
// with the same drift stay silent on the IPC. Goroutine-safe: the F6-A1
// continuation calls it with NO engine lock held (the CI race detector
// caught the unsynchronized engine fields).
func (e *Engine) setSpeed(f float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.setSpeedLocked(f)
}

// setSpeedLocked is setSpeed for callers that already hold e.mu.
func (e *Engine) setSpeedLocked(f float64) {
	if e.speedSet && f == e.lastSpeed {
		return
	}
	e.speedSet = true
	e.lastSpeed = f
	_ = e.Player.Speed(f)
}

// OnRoom feeds one SSE event through the engine (client.py on_event parity;
// position is stored, discrete events are applied to the player).
func (e *Engine) OnRoom(ev room.Event) {
	e.mu.Lock()
	deferred := e.onRoomLocked(ev)
	e.mu.Unlock()
	// F6-A1 condition 4: the OSD tells run AFTER the unlock — a wedged mpv
	// must not hold the engine lock (or the UI thread) for a chat bubble.
	for _, f := range deferred {
		f()
	}
}

// onRoomLocked is OnRoom under e.mu; player calls that may block are
// returned as deferred closures and run after the unlock.
func (e *Engine) onRoomLocked(ev room.Event) (deferred []func()) {
	switch ev.Type {
	case "position":
		now := e.Clock.Now()
		e.snap = &snapshot{t: ev.T, paused: ev.P, at: now, beat: now}
		e.pendingPos = nil // a fresh heartbeat supersedes a pending hello position
	case "hello":
		// Resume-by-design: a reconnecting client (follower OR master)
		// reloads the room's file and seeks to the live position. For a
		// restarted master this rewinds followers to the crash-time position
		// — bounded, self-consistent (the master is the authority), and far
		// better than starting empty while the room still carries state.
		// End-to-end: tests/master_restart.mjs.
		// only (re)load when the file actually differs from what our mpv is
		// already playing: an SSE blip must not restart a mid-film download (F3)
		if ev.Now != "" && ev.Now != e.loaded {
			e.startLoad(ev.Now)
		}
		// a reconnect is a fresh stall window: a behind-tick from before
		// the outage must not trip the stall counter on the first tick
		// after it (the room would be paused for a stall that is over)
		e.autoPaused = false
		// M2-a: the resume episode SURVIVES the reconnect. A sleep/resume
		// with an SSE drop (the common path — readIdle 25s) must not
		// replay the old stall: the hello-driven seek flushes the cache
		// and the refill is not starvation (m2-501). The episode ends when
		// the follower converges (M2-b), not because a hello arrived.
		// An UNRESOLVED resume keeps the behind-counter: the follower is
		// still behind, and a reconnect storm must not reset the counter
		// forever — the backstop would never fire (master-2 P2-1).
		if !e.resumeSeen {
			e.behind = 0 // a reconnect is a fresh stall window
		}
		// Server may carry an age-adjusted media position + paused flag
		// from the room's live snapshot (set after every position relay,
		// cleared by load/ended/skip). When present, snap now and seek
		// before the first heartbeat would even arrive.
		if ev.Pos != nil {
			// F1: clamp the room's position to the file we actually have.
			// mpv reports the duration only after the file is loaded, so
			// the duration is often unknown here: seeking then could land
			// past the end of the film (EOF). Wait for the first sample
			// that knows the duration.
			if e.dur > 0 {
				e.applySnapshot(*ev.Pos, ev.P)
			} else {
				p := *ev.Pos
				e.pendingPos, e.pendingPause = &p, ev.P
			}
			if ev.P {
				e.setPause(true)
				e.pausedBy = "" // the snapshot names no actor: honest empty
			}
		}
	case "load":
		if ev.Name != e.loaded {
			e.startLoad(ev.Name)
		}
	case "pause":
		e.setPause(true)
		e.lastPause = true   // master dedup stays consistent with reality
		e.postedPause = nil  // the room's state supersedes our pending post
		e.pausedBy = ev.Nick // the relay names the actor ("" = unstamped)
		// m2-S2: a mechanical pause (a stall on the other side's link)
		// names no human: the chip must not blame someone for it.
		e.mechPause = ev.Mechanical
		if ev.Mechanical {
			e.pausedBy = ""
		}
		if e.snap != nil {
			e.snap.paused = true // discrete events are room truth; don't wait for the next heartbeat
		}
	case "play":
		e.autoPaused = false // the room resumed: our stall-pause (if any) is spent
		e.behind = 0         // fresh stall window: resume-into-dry-cache must not re-pause instantly
		e.mechPause = false  // the room plays again: no mechanical pause any more
		e.setPause(false)
		e.lastPause = false
		e.postedPause = nil // the room's state supersedes our pending post
		e.pausedBy = ""     // playing: nobody holds the pause
		if e.snap != nil {
			e.snap.paused = false
			// re-anchor: the snapshot's age spans the pause; aging it into
			// expected right after resume causes a spurious forward jump (F5)
			e.snap.at = e.Clock.Now()
		}
	case "seek":
		e.seekTo(ev.T)
		e.lastPos = nil // the applied seek moved us: the scrub baseline is stale
	case "chat":
		// chat OSD over the video (client.py parity: 3s show-text) — F6-A1:
		// the tell runs after the unlock (condition 4)
		osd := fmt.Sprintf("%s: %s", ev.Nick, ev.Text)
		deferred = append(deferred, func() { _ = e.Player.ShowText(osd, 3000) })
	case "notice":
		// room-level warning (server election, R7). If it names US, a second
		// --master is running: demote to follower instead of dueling — the
		// owner's heartbeats are already arriving, sync resumes within a tick.
		if ev.From != "" && ev.From == e.ID && e.Master {
			e.Master = false
			e.logf("> demoted to follower: another master owns the clock")
		}
		// OSD so both users see it even with the TUI window hidden —
		// F6-A1: after the unlock (condition 4)
		osd := ev.Text
		deferred = append(deferred, func() { _ = e.Player.ShowText(osd, 4000) })
	}
	return deferred
}

// startLoad is the lock-held half of a file change (F6-A1): the
// bookkeeping and the loadfile tell — the ONLY blocking call allowed here
// (a bounded tell). The wait for the file to go live and the subtitle
// attach continue OFF the lock (continueLoad), so a wedged mpv cannot
// freeze the TUI for the whole load chain anymore (~25-35s before).
func (e *Engine) startLoad(name string) {
	// caller holds e.mu
	e.behind, e.autoPaused = 0, false
	e.resumed = false            // a new file: the next jump is a new resume
	e.resumeSeen = false         // a new file: no unresolved jump
	e.resumeTil = time.Time{}    // the resume episode must not outlive the file
	e.endedAt = time.Time{}      // the 'ended' post belongs to the old file
	e.mechPause = false          // the room plays a new file: no mechanical pause
	e.wasActive = false          // the new file gets its own EOF: the old one is over
	e.logf("> playing %s", name) // client.py Player.load parity (harness key)
	// F6-A1: claimed SPECULATIVELY — the settle window must not let a hello
	// reload the very file mpv is already taking (OwnLoad parity). If the
	// load never goes live, the next load/hello re-syncs (logged there).
	e.loaded = name
	e.loadedAt = e.Clock.Now()
	e.lastPos = nil // a new file starts at the head: the old scrub baseline is stale
	e.dur = 0       // the new file's duration is unknown until mpv reports it
	// the old snapshot describes the PREVIOUS file: keeping it would make
	// the follower chase a position that does not exist in this one (and
	// the room could be paused for a "stall" that is just the new head)
	e.snap = nil
	e.pendingPos = nil
	if _, err := e.Player.StartLoad(name); err != nil {
		e.logf("mpv: %s", err)
		return
	}
	go e.continueLoad(name, e.Clock.Now().Add(loadSettle))
}

// continueLoad is the off-lock half of a file change (F6-A1): wait until
// the file is live behind a GUARANTEED signal (WaitLive's own re-poll —
// the mpv events channel DROPS under stall, so it can never be the
// trigger), then clear the tempo nudge and attach subtitles. No e.mu is
// held here.
func (e *Engine) continueLoad(name string, deadline time.Time) {
	if !e.Player.WaitLive(name, deadline) {
		// shortcut: the speculative claim stays; a wedged mpv heals through
		// the next load/hello (the room was told either way)
		e.logf("mpv: load did not go live: %s", name)
		return
	}
	e.setSpeed(1) // new file: clear any tempo nudge from the previous one (no lock held here)
	e.Player.AttachSubs(name)
}

// OwnLoad records a file the local user started outside the room-event
// path (TUI/headless /play drive mpv directly): without it, a later hello
// would reload the very file mpv is already playing (F3, master half).
func (e *Engine) OwnLoad(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.loaded = name
	e.loadedAt = e.Clock.Now()
	e.behind, e.autoPaused = 0, false
	e.wasActive = false // same as load(): the new file owns the next EOF
	e.lastPos = nil     // same as load(): the position jumped, the baseline is stale
	e.dur = 0           // same as load(): the new duration is unknown yet
	e.endedAt = time.Time{}
	e.snap = nil // same as load(): the old snapshot belongs to the old file
	e.pendingPos = nil
}

// applySnapshot anchors the room's live position, clamped to the file we
// actually have: the room's position can be past the end of our file
// (a stale room snapshot), and seeking there would EOF the player.
func (e *Engine) applySnapshot(pos float64, paused bool) {
	if e.dur > 0 && pos > e.dur-1 {
		pos = e.dur - 1
	}
	now := e.Clock.Now()
	e.snap = &snapshot{t: pos, paused: paused, at: now, beat: now}
	// M2-a: a hello-driven jump that is resume-sized starts the resume
	// episode: the seek flushes the cache and the refill must not be
	// mistaken for starvation (m2-501). The deadline is seeded ONLY when
	// no episode is open AND the jump is unresolved: an open episode's
	// deadline must not be moved by a hello — a reconnect storm would
	// keep it alive forever and the room could never pause (master-2
	// P2-1). The unresolved jump is the per-file cap: no new episode
	// until the follower converges.
	if t, ok := e.Player.Pos(); ok && pos-t > resumeJump && !e.resumed && !e.resumeSeen {
		e.resumed = true
		e.resumeSeen = true
		e.resumeTil = now.Add(resumeBound)
	}
	// skip the seek when already there: on an SSE blip this is a
	// sub-second rewind that would flash the same frame (F3)
	if t, ok := e.Player.Pos(); !ok || abs(t-pos) > Tolerance {
		e.seekTo(pos)
	}
}

// PauseToggle is the TUI hotkey equivalent of a space-bar press in the
// mpv window: apply locally AND post — the server never echoes a poster's
// own event back, so posting alone would leave this player unsynced.
func (e *Engine) PauseToggle() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mechPause = false // a human pressed: the pause is not mechanical any more
	var want bool
	if e.Master {
		want = !e.lastPause
		e.lastPause = want
	} else {
		if e.snap == nil {
			return
		}
		paused := e.snap.paused
		// our pending post is newer than the room's last heartbeat: trust it
		// over the snapshot, or the toggle repeats the state we just posted
		// (the room would never resume)
		if e.postedPause != nil && !e.lastPost.IsZero() && e.Clock.Since(e.lastPost) < followerGrace {
			paused = *e.postedPause
		}
		want = !paused
		e.snap.paused = want // mirror: the echo event must not re-post
		p := want
		e.postedPause = &p
	}
	e.setPause(want)
	e.lastPost = e.Clock.Now()
	e.pausedBy = "" // our own action: nobody else holds the pause
	typ := "play"
	if want {
		typ = "pause"
	}
	e.postDiscrete(room.Event{Type: typ})
}

// SeekBy is the TUI hotkey nudge (ctrl+arrow): seek locally and post the
// target — seekTo marks the echo so the resulting mpv event is consumed,
// never re-posted. F6-A2: the target comes from the polled position field,
// NOT a Pos() IPC round trip (a wedged mpv would hold e.mu for ≤5s per
// keypress).
func (e *Engine) SeekBy(dt float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.polledPos == nil {
		return
	}
	t := max(0, *e.polledPos+dt)
	e.seekTo(t)
	e.lastPost = e.Clock.Now()
	e.postDiscrete(room.Event{Type: "seek", T: t})
	if e.Master {
		e.lastPos = nil // scrub-check restarts fresh (no double post)
	}
}

// SeekTo jumps to an absolute position: the local player moves AND the
// room is told. The clickable bar needs both halves — the server never
// echoes a poster's own event back, so a POST-only seek does nothing for
// the clicker (SeekBy parity).
func (e *Engine) SeekTo(t float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t = max(0, t)
	e.seekTo(t)
	e.lastPost = e.Clock.Now()
	e.postDiscrete(room.Event{Type: "seek", T: t})
	if e.Master {
		e.lastPos = nil // scrub-check restarts fresh (no double post)
	}
}

// VolumeUp/VolumeDown/VolumeMute are LOCAL audio keys: the volume belongs
// to the viewer's own machine and ears — the room is never told (F5 "keys
// for a whole movie": syncing personal audio would fight every other
// viewer's volume). The fire-and-forget player calls keep the hotkey
// instant; the next poll names the real value in the stats bar.
func (e *Engine) VolumeUp()   { e.volumeNudge(volStep) }
func (e *Engine) VolumeDown() { e.volumeNudge(-volStep) }

func (e *Engine) volumeNudge(delta float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Player == nil {
		return
	}
	if err := e.Player.SetVolume(delta); err != nil {
		e.logf("> vol: mpv refused (%v)", err)
		return
	}
	e.logf("> vol %+.0f", delta)
}

// VolumeMute cycles mpv's mute: the log names the state we EXPECT (the
// last poll's flag flipped); the next poll corrects any staleness.
func (e *Engine) VolumeMute() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Player == nil {
		return
	}
	if err := e.Player.ToggleMute(); err != nil {
		e.logf("> mute: mpv refused (%v)", err)
		return
	}
	e.muted = !e.muted
	if e.muted {
		e.logf("> muted")
	} else {
		e.logf("> unmuted")
	}
}

// OnMpvp is the poll tick (POLL_SECS cadence), client.py poll_loop parity.
func (e *Engine) OnMpvp(s mpv.State) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// SYNC2: a sample taken BEFORE the last load describes the previous
	// file. The poll samples in its own goroutine, so it can hand us an
	// old sample after the room already moved on: posting it would carry
	// the old position and duration into the new film's heartbeat.
	if !e.loadedAt.IsZero() && !s.At.IsZero() && s.At.Before(e.loadedAt) {
		return
	}
	if s.Dur > 0 {
		e.dur = s.Dur // the locally known media duration (F1 clamp)
	}
	e.muted = s.Muted // the mute log names the state we expect (F5)
	if s.Pos != nil { // F6-A2: SeekBy spends this, not an IPC round trip
		p := *s.Pos
		e.polledPos = &p
	} else {
		e.polledPos = nil
	}
	e.muted = s.Muted // the mute log names the state we expect (F5)
	// EOF transition (either role): at idle mpv keeps pause=false and drops
	// time-pos, so without this the UI sits on a stale ▶ forever. Ended
	// reads as paused until the next load.
	if s.Idle && e.wasActive {
		e.wasActive = false
		e.emitUi(mpv.State{Pause: true, Muted: s.Muted}, 0)
		if e.Master {
			e.post1(room.Event{Type: "ended"}) // server auto-advances the queue
		}
	} else if !s.Idle && s.Pos != nil {
		e.wasActive = true
	}
	// M1-A: our file is over and the room's master is really dead (its
	// heartbeat went stale — with M1-B a parked master keeps heartbeating,
	// so a stale snapshot means dead, not parked). Nobody else will ever
	// advance the queue, so we do it. The server accepts a follower's
	// 'ended' only for an ownerless room and releases the dead master's
	// claim on its own schedule — the client learns of the death earlier —
	// so the post is repeated until the room takes it. The episode ends
	// when the room answers (a new file resets the stamp).
	if !e.Master && s.Idle && e.loaded != "" && e.snap != nil && e.Clock.Since(e.snap.beat) > staleAge {
		if e.endedAt.IsZero() || e.Clock.Since(e.endedAt) >= time.Duration(endedRetry)*PollSecs*time.Second {
			e.endedAt = e.Clock.Now()
			e.post1(room.Event{Type: "ended"})
		}
	}
	// F1: the hello position waited for the duration — now it can be
	// clamped and applied.
	if e.pendingPos != nil && e.dur > 0 {
		pos := *e.pendingPos
		e.pendingPos = nil
		e.applySnapshot(pos, e.pendingPause)
	}
	if e.Master {
		e.masterTick(s)
		return
	}
	if s.Pos != nil && e.snap != nil {
		e.followerTick(s)
	}
}

// OnMpvEvent reacts to pushed mpv events (pause/unpause/seek). The master
// posts discrete transitions instantly (plan Q1). Followers post their own
// user actions too (v1.4 dual control): the master applies them via OnRoom
// and its heartbeat re-asserts a consistent room state. Events that merely
// echo the engine's own programmatic actuation are gated so applying
// remote state never re-posts it (loop prevention). The seek settle sleep
// sits on the POST path only: echoes return above it, so a stalled
// consumer can no longer back the event queue up and drop user seeks
// (soak flake, 2026-10-27).
func (e *Engine) OnMpvEvent(name string) {
	if e.Player == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if name == "seek" {
		if !e.lastSeekAt.IsZero() && e.Clock.Since(e.lastSeekAt) > 5*time.Second {
			e.seekEcho = false // the echo never came (or is ancient): never eat a user scrub
		}
		if e.seekEcho {
			// exactly one echo per programmatic seek; the NEXT seek inside the
			// window is a user scrub and must post. Target matching read a
			// stale time-pos on a loaded machine and ate real scrubs (soak
			// regression, 2026-10-27)
			e.seekEcho = false
			return
		}
	}
	if !e.lastActuate.IsZero() && e.Clock.Since(e.lastActuate) < actuateQuiet {
		// seeks are echo-gated one-shot by seekEcho above; a time window
		// here would also eat a user scrub landing soon after a remote apply
		// (the residual soak flake). Only pause echoes are direction-matched.
		if e.actuatePause != nil && *e.actuatePause == (name == "pause") {
			return // our own SetPause echoing back as an mpv event
		}
		// opposite-direction event inside the window: the user acted — post it
	}
	switch name {
	case "pause", "unpause":
		wantPause := name == "pause"
		typ := "pause" // wire types are play/pause, mpv says pause/unpause
		if !wantPause {
			typ = "play"
		}
		if e.Master {
			if wantPause != e.lastPause {
				e.postDiscrete(room.Event{Type: typ})
				e.lastPause = wantPause
			}
			return
		}
		// follower: post only when the room does not know this state yet
		// (a matching snapshot means this pause came from the room itself)
		if e.snap == nil || e.snap.paused == wantPause {
			return
		}
		// our own post is still pending: a heartbeat that was in flight
		// overwrote the snapshot mirror, and the late echo of our own
		// apply must not post the same state again (the room would
		// apply it twice)
		if e.postedPause != nil && *e.postedPause == wantPause &&
			!e.lastPost.IsZero() && e.Clock.Since(e.lastPost) < followerGrace {
			return
		}
		e.lastPost = e.Clock.Now()
		// mirror our post into the snapshot: the next heartbeat still carries
		// the pre-post state, and without the mirror a quick opposite-direction
		// user move is swallowed by the check above (python room_pause parity)
		e.snap.paused = wantPause
		p := wantPause
		e.postedPause = &p
		e.postDiscrete(room.Event{Type: typ})
	case "seek":
		if !e.loadedAt.IsZero() && e.Clock.Since(e.loadedAt) < loadQuiet {
			// a fresh loadfile makes mpv emit a seek at the file head;
			// posting it would rewind the room to ~0 (late-join race, a
			// demoted impostor's own /play). shortcut: user scrubs inside
			// the window self-heal via the master heartbeat
			return
		}
		time.Sleep(seekSettle) // posts only: let time-pos reach the scrub target
		t, ok := e.Player.Pos()
		if !ok {
			return
		}
		if e.Master {
			e.postDiscrete(room.Event{Type: "seek", T: t})
			e.lastPos = nil // poll scrub-check restarts fresh (no double post)
			return
		}
		if e.snap == nil {
			return // nothing to sync against yet
		}
		e.lastPost = e.Clock.Now()
		e.postDiscrete(room.Event{Type: "seek", T: t})
	}
}

// postDiscrete posts one discrete event and mirrors it in the local log:
// own SSE events are echo-filtered, so without this the actor's own
// terminal would never show their pause/play/seek.
func (e *Engine) postDiscrete(ev room.Event) error {
	err := e.post1(ev)
	switch ev.Type {
	case "pause":
		// our own pause: nobody else holds it. Without this the actor of
		// an earlier remote pause stayed stale and our own pause was
		// rendered as "paused by <them>" (the mpv window's space bar
		// posts through here).
		e.pausedBy = ""
		e.logf("> %s", ev.Type)
	case "play":
		// no reset needed: emitUi drops the actor when we are playing
		e.logf("> %s", ev.Type)
	case "seek":
		e.logf("> seek @%.1fs", ev.T)
	}
	return err
}

func (e *Engine) masterTick(s mpv.State) {
	if s.Partial {
		// MPV1: the property batch was truncated (mpv wedged mid-way).
		// The values are incomplete, so no heartbeat goes out: the room's
		// clock must not jump on a partial read.
		e.logf("> mpv sample truncated: no heartbeat this tick")
		return
	}
	if s.Pos == nil && !s.Idle {
		return // nothing loaded yet
	}
	if s.Pause != e.lastPause && !s.Idle {
		// a poll sampled while a remote pause was still landing can carry the
		// pre-pause state; posting it flaps play->pause room-wide (R-out1b).
		// Push events own transitions; the poll path is only a fallback for
		// missed events, so samples contradicting a fresh actuation are stale.
		if e.actuatePause != nil && *e.actuatePause != s.Pause && e.Clock.Since(e.lastActuate) < 3*time.Second {
			return
		}
		typ := "play"
		if s.Pause {
			typ = "pause"
		}
		e.postDiscrete(room.Event{Type: typ})
		e.lastPause = s.Pause
	}
	if s.Pos != nil {
		if e.lastPos != nil && abs(*s.Pos-*e.lastPos-PollSecs) > SeekScrubThreshold {
			e.postDiscrete(room.Event{Type: "seek", T: *s.Pos}) // user scrubbed
		}
		// S3: the heartbeat carries the media duration, so the server can
		// clamp a long-downtime join before EOF. Dur stays off the wire
		// while unknown (omitempty): an old server ignores it, and the
		// server keeps its last known value.
		e.post1(room.Event{Type: "position", T: *s.Pos, P: s.Pause, Dur: s.Dur, File: e.loaded})
		e.lastPos = s.Pos
		e.emitUi(s, 0) // master: no drift, but the status bar wants pos/paused
		return
	}
	// M1-B: the master is parked at EOF (the film ended, the queue may be
	// empty). It must keep heartbeating: these posts HOLD the room's
	// clock claim — a parked master used to lose it after 45s, and a
	// second --master could then steal the clock silently (M1-B) — and
	// they keep the followers from showing a false "stale" chip. The room
	// is at the end (p=true), so the server ages the position no further.
	if e.lastPos != nil {
		e.post1(room.Event{Type: "position", T: *e.lastPos, P: true, Dur: e.dur, File: e.loaded})
	}
}

func (e *Engine) followerTick(s mpv.State) {
	if !e.lastPost.IsZero() && e.Clock.Since(e.lastPost) < followerGrace {
		// we just acted (dual control): give the master's heartbeat time
		// to reflect it before self-correcting back
		e.emitUi(s, 0)
		return
	}
	if e.snap.paused != s.Pause {
		// self-heal: converge to the master's playback state even if the
		// discrete play/pause event never arrived. Emit with the TARGET state:
		// s.Pause is pre-actuation and the next poll is 2s away — without this
		// the follower UI sits on a stale ▶/⏸ while mpv is already there.
		// This runs BEFORE the stale gate on purpose: converging to the
		// room's pause is local and reversible (setPause posts nothing) —
		// the gate exists to stop WIRE actions, not to keep the follower
		// playing in a paused room.
		e.setPause(e.snap.paused)
		e.emitUi(mpv.State{Pos: s.Pos, Pause: e.snap.paused, Cache: s.Cache, Volume: s.Volume, Muted: s.Muted, Sub: s.Sub, Layout: s.Layout}, 0)
		return
	}
	if e.snap.paused {
		e.setSpeedLocked(1) // paused: clear any tempo nudge for a clean resume
		e.emitUi(s, 0)      // both paused: hold position — the UI still tracks mpv
		return
	}
	// m2-S1: only a LIVE master can starve us. A silent heartbeat means
	// the room's clock is unknown: pausing it would punish the follower
	// for the master's silence, and chasing the stale snapshot would
	// flush the cache for nothing. The UI shows the stale chip instead.
	if e.Clock.Since(e.snap.beat) > staleAge {
		e.behind = 0
		e.emitUi(s, 0)
		return
	}
	// age must not grow while the master stands still
	expected := e.snap.t + e.Clock.Since(e.snap.at).Seconds()
	drift := *s.Pos - expected
	// m2-S2: a resume (sleep, suspend, a long outage) is a jump, not
	// starvation: re-seek once, give the cache time to refill, and never
	// pause the room for it.
	if drift < -resumeJump && !e.resumeSeen {
		e.resumed = true
		e.resumeSeen = true
		e.behind = 0
		e.resumeTil = e.Clock.Now().Add(resumeBound)
		e.seekTo(expected)
		e.emitUi(s, drift)
		return
	}
	// M2-b: the resume episode follows the CACHE, not the clock. While
	// mpv is still filling there is no seek storm and no room pause (a
	// slow link needs more than the old fixed 30s — W5); the episode ends
	// as soon as the follower converges. The bound is the honest backstop:
	// a resume that never converges must still reach the normal stall
	// logic (a broken link must not be pause-proof forever).
	if e.resumed {
		switch {
		case !e.Clock.Now().Before(e.resumeTil):
			// the bound expired: the episode is over (the flag is cleared —
			// P3-3) but the jump stays unresolved, so the SAME jump is not
			// mistaken for a new resume (which would move the deadline
			// forever — master-2 P2-1)
			e.resumed = false
			e.resumeTil = time.Time{}
		case !s.Buffering && abs(drift) <= Tolerance:
			e.resumed = false // converged: the episode is over
			e.resumeSeen = false
			e.resumeTil = time.Time{}
		default:
			e.behind = 0
			e.emitUi(s, drift)
			return
		}
	}
	if s.Buffering {
		// mpv says the cache is dry. Feed the same stall counter as the
		// behind-live detector: the play event resets it, so a resume into a
		// still-dry cache re-pauses only after the full runway (no 2 s
		// duplicate), and we never seek while the cache is dry (F2).
		e.behind++
		if e.behind >= stallTicks {
			e.stallBlock(s)
			return
		}
		e.setSpeedLocked(1)
		e.emitUi(s, drift)
		return
	}
	e.emitUi(s, drift)
	switch {
	case drift < -Tolerance:
		// behind live. A healthy player converges one tick after the seek
		// below; one that lands behind again is starving (WAN link below the
		// file's bitrate) — chasing it with seeks would flush the filling
		// cache every 2s (seek-storm, F2), so stall after stallTicks.
		e.behind++
		if e.behind >= stallTicks {
			e.stallBlock(s)
			return
		}
		e.seekTo(expected) // probe seek: one chance to converge before stalling
	case abs(drift) > Tolerance:
		e.behind = 0
		e.resumed = false // we caught up: the next jump is a new resume
		e.resumeSeen = false
		e.seekTo(expected) // ahead of live: clock jump or a lagging master
	case abs(drift) > speedBand:
		e.behind = 0
		e.resumed = false
		e.resumeSeen = false
		// smooth converge: a small tempo nudge instead of a visible seek
		e.setSpeedLocked(1 + clamp(-drift*0.1, -speedAdj, speedAdj))
	default:
		e.behind = 0
		e.resumed = false
		e.resumeSeen = false
		e.setSpeedLocked(1) // inside the dead zone: plain 1x
	}
}

// stallBlock: the follower is starving while the room plays — typically a
// WAN link below the file's bitrate. Never hard-seek: the seek would
// flush the cache that is filling, re-stall and repeat every tick
// (seek-storm, F2). The true backlog shows as drift; the room is paused
// once so the cache can refill. No auto-resume: a wire pause carries no
// owner, auto-resuming could fight a deliberate pause — humans press play.
func (e *Engine) stallBlock(s mpv.State) {
	e.setSpeedLocked(1)
	expected := e.snap.t + e.Clock.Since(e.snap.at).Seconds()
	if !e.autoPaused {
		e.autoPaused = true
		e.snap.paused = true // mirror our post: heartbeats carry the pre-post state briefly
		e.pausedBy = ""      // our own stall pause: no actor
		e.lastPost = e.Clock.Now()
		// the marker travels on the wire: the other side must not blame a
		// human for a pause the room asked for by itself (m2-S2)
		// W3: the post rides the same starving link that caused the stall
		// — so a failure is the EXPECTED one there. The re-arm happens in
		// onPostFailed (the post is asynchronous: the caller must not wait
		// for the network, ISSUE-6).
		e.postDiscrete(room.Event{Type: "pause", Mechanical: true})
		e.mechPause = true // mechanical: nobody pressed anything (m2-S2)
		_ = e.Player.ShowText("buffering - room paused; press play when ready", 4000)
	}
	e.emitUi(s, *s.Pos-expected)
}

func (e *Engine) emitUi(s mpv.State, drift float64) {
	if e.Ui == nil {
		return
	}
	// follower only: a heartbeat older than staleAge means the master's
	// heartbeats stopped — the drift number below is then meaningless.
	// The heartbeat age is measured on its own stamp (snap.beat): the
	// snapshot's anchor (snap.at) is re-anchored by a discrete play event
	// (R5), which would hide a dead master.
	// F6 D1: an IDLE local player means the film ENDED — the master's
	// heartbeats stop with the file (m2-S1's rule: only a LIVE master can
	// starve us). An ended film is not a missing master, so the flag must
	// not fire there (M1 semantics: EOF != a lost master).
	stale := !e.Master && e.snap != nil && !s.Idle && e.Clock.Since(e.snap.beat) > staleAge
	by := e.pausedBy
	if !s.Pause {
		by = "" // playing: the chip is not shown, the actor is moot
	}
	select {
	case e.Ui <- UiState{Drift: drift, Pos: s.Pos, Paused: s.Pause, Cache: s.Cache, Volume: s.Volume, Muted: s.Muted, Stale: stale, PausedBy: by, Mechanical: e.mechPause, Sub: s.Sub, Layout: s.Layout}:
	default:
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func clamp(f, lo, hi float64) float64 {
	if f < lo {
		return lo
	}
	if f > hi {
		return hi
	}
	return f
}
