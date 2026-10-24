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

// Fixed numbers, plan section 1 (client.py parity; taken verbatim).
const (
	PollSecs           = 2.0 // mpv poll period (master heartbeat cadence)
	Tolerance          = 1.0 // follower drift threshold before re-seek
	SeekScrubThreshold = 3.0 // |pos - last_pos - PollSecs| beyond this = scrub
)

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
	Load(name string) error
	SetPause(paused bool) error
	Seek(t float64) error
	Speed(f float64) error
	ShowText(text string, ms int) error
	Pos() (float64, bool)
}

// UiState feeds the M4 status bar (drift etc); non-blocking send, dropped
// when nobody consumes yet.
type UiState struct {
	Drift  float64  // follower pos - master expected, seconds
	Pos    *float64 // local playback position (master: own, follower: own)
	Paused bool
}

type Engine struct {
	Master bool
	Player Player
	Post   func(room.Event) error
	Clock  Clock
	Ui     chan<- UiState
	Logf   func(format string, args ...any) // nil = discard

	lastPos   *float64
	lastPause bool
	wasActive bool
	snap      *snapshot // follower: latest master position heartbeat

	lastActuate  time.Time // last programmatic SetPause/Seek (echo gate)
	actuatePause *bool     // pause state that actuation set (nil = seek); an
	// mpv event is only an echo when it matches this — the user pressing
	// space right after a remote apply must never be swallowed
	lastPost time.Time // last follower discrete post (grace window)

	speedSet  bool    // a Speed value has been sent (0.0 is a valid speed)
	lastSpeed float64 // current tempo nudge; cache avoids IPC spam per tick

	lastSeekT  float64   // target of the last programmatic seek (echo match)
	lastSeekAt time.Time // when issued; slow echoes (file load) arrive late

	mu sync.Mutex // guards engine state: poll + mpv-event + SSE goroutines
}

type snapshot struct {
	t      float64
	paused bool
	at     time.Time
}

func (e *Engine) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

func (e *Engine) post1(ev room.Event) {
	if e.Post == nil {
		return
	}
	if err := e.Post(ev); err != nil {
		e.logf("post: %s", err)
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
	e.lastActuate = e.Clock.Now()
	e.actuatePause = nil // seek echo arrives as a "seek" mpv event
	e.lastSeekT = t
	e.lastSeekAt = e.lastActuate
	_ = e.Player.Seek(t)
}

// setSpeed applies a tempo nudge once per distinct value; repeated ticks
// with the same drift stay silent on the IPC.
func (e *Engine) setSpeed(f float64) {
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
	defer e.mu.Unlock()
	switch ev.Type {
	case "position":
		e.snap = &snapshot{t: ev.T, paused: ev.P, at: e.Clock.Now()}
	case "hello":
		// Resume-by-design: a reconnecting client (follower OR master)
		// reloads the room's file and seeks to the live position. For a
		// restarted master this rewinds followers to the crash-time position
		// — bounded, self-consistent (the master is the authority), and far
		// better than starting empty while the room still carries state.
		// End-to-end: tests/master_restart.mjs.
		if ev.Now != "" {
			e.load(ev.Now)
		}
		// Server may carry an age-adjusted media position + paused flag
		// from the room's live snapshot (set after every position relay,
		// cleared by load/ended/skip). When present, snap now and seek
		// before the first heartbeat would even arrive.
		if ev.Pos != nil {
			e.snap = &snapshot{t: *ev.Pos, paused: ev.P, at: e.Clock.Now()}
			e.seekTo(*ev.Pos)
			if ev.P {
				e.setPause(true)
			}
		}
	case "load":
		e.load(ev.Name)
	case "pause":
		e.setPause(true)
		e.lastPause = true // master dedup stays consistent with reality
		if e.snap != nil {
			e.snap.paused = true // discrete events are room truth; don't wait for the next heartbeat
		}
	case "play":
		e.setPause(false)
		e.lastPause = false
		if e.snap != nil {
			e.snap.paused = false
		}
	case "seek":
		e.seekTo(ev.T)
	case "chat":
		// chat OSD over the video (client.py parity: 3s show-text)
		_ = e.Player.ShowText(fmt.Sprintf("%s: %s", ev.Nick, ev.Text), 3000)
	}
}

func (e *Engine) load(name string) {
	e.setSpeed(1)                // new file: clear any tempo nudge from the previous one
	e.logf("> playing %s", name) // client.py Player.load parity (harness key)
	if err := e.Player.Load(name); err != nil {
		e.logf("mpv: %s", err)
	}
}

// OnMpvp is the poll tick (POLL_SECS cadence), client.py poll_loop parity.
func (e *Engine) OnMpvp(s mpv.State) {
	e.mu.Lock()
	defer e.mu.Unlock()
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
// happens outside the lock so the engine keeps serving during it.
func (e *Engine) OnMpvEvent(name string) {
	if e.Player == nil {
		return
	}
	if name == "seek" {
		time.Sleep(seekSettle)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if name == "seek" && !e.lastSeekAt.IsZero() && e.Clock.Since(e.lastSeekAt) < 5*time.Second {
		// match by target: during file loads mpv can deliver the seek echo
		// seconds later, past the actuateQuiet window; a user scrubbing to
		// within 1s of our own recent target is indistinguishable — the
		// heartbeat self-heals that rare case
		if t, ok := e.Player.Pos(); ok && abs(t-e.lastSeekT) < 1.0 {
			return
		}
	}
	if !e.lastActuate.IsZero() && e.Clock.Since(e.lastActuate) < actuateQuiet {
		if name == "seek" || (e.actuatePause != nil && *e.actuatePause == (name == "pause")) {
			return // our own SetPause/Seek echoing back as mpv events
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
		e.lastPost = e.Clock.Now()
		// mirror our post into the snapshot: the next heartbeat still carries
		// the pre-post state, and without the mirror a quick opposite-direction
		// user move is swallowed by the check above (python room_pause parity)
		e.snap.paused = wantPause
		e.postDiscrete(room.Event{Type: typ})
	case "seek":
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
func (e *Engine) postDiscrete(ev room.Event) {
	e.post1(ev)
	switch ev.Type {
	case "pause", "play":
		e.logf("> %s", ev.Type)
	case "seek":
		e.logf("> seek @%.1fs", ev.T)
	}
}

func (e *Engine) masterTick(s mpv.State) {
	if s.Pos == nil && !s.Idle {
		return // nothing loaded yet
	}
	if s.Pause != e.lastPause && !s.Idle {
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
		e.post1(room.Event{Type: "position", T: *s.Pos, P: s.Pause})
		e.lastPos = s.Pos
		e.emitUi(s, 0) // master: no drift, but the status bar wants pos/paused
	}
	if s.Idle && e.wasActive {
		e.post1(room.Event{Type: "ended"}) // EOF: server auto-advances the queue
		e.wasActive = false
	} else if !s.Idle && s.Pos != nil {
		e.wasActive = true
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
		// discrete play/pause event never arrived
		e.setPause(e.snap.paused)
		return
	}
	if e.snap.paused {
		e.setSpeed(1) // paused: clear any tempo nudge for a clean resume
		// both paused: hold position
		return
	}
	// age must not grow while the master stands still
	expected := e.snap.t + e.Clock.Since(e.snap.at).Seconds()
	drift := *s.Pos - expected
	e.emitUi(s, drift)
	switch {
	case abs(drift) > Tolerance:
		e.seekTo(expected) // hard seek; drift resets, next tick restores speed 1
	case abs(drift) > speedBand:
		// smooth converge: a small tempo nudge instead of a visible seek
		e.setSpeed(1 + clamp(-drift*0.1, -speedAdj, speedAdj))
	default:
		e.setSpeed(1) // inside the dead zone: plain 1x
	}
}

func (e *Engine) emitUi(s mpv.State, drift float64) {
	if e.Ui == nil {
		return
	}
	select {
	case e.Ui <- UiState{Drift: drift, Pos: s.Pos, Paused: s.Pause}:
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
