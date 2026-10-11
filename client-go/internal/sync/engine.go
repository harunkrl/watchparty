// Package sync holds the master/follower sync engine: pure logic, no UI
// dependencies, Clock injected so tests simulate minutes instantly (E4).
package sync

import (
	"fmt"
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
	ShowText(text string, ms int) error
}

// UiState feeds the M4 status bar (drift etc); non-blocking send, dropped
// when nobody consumes yet.
type UiState struct {
	Drift float64 // follower pos - master expected, seconds
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

// OnRoom feeds one SSE event through the engine (client.py on_event parity;
// position is stored, discrete events are applied to the player).
func (e *Engine) OnRoom(ev room.Event) {
	switch ev.Type {
	case "position":
		e.snap = &snapshot{t: ev.T, paused: ev.P, at: e.Clock.Now()}
	case "hello":
		if ev.Now != "" {
			e.load(ev.Now)
		}
	case "load":
		e.load(ev.Name)
	case "pause":
		_ = e.Player.SetPause(true)
	case "play":
		_ = e.Player.SetPause(false)
	case "seek":
		_ = e.Player.Seek(ev.T)
	case "chat":
		// chat OSD over the video (client.py parity: 3s show-text)
		_ = e.Player.ShowText(fmt.Sprintf("%s: %s", ev.Nick, ev.Text), 3000)
	}
}

func (e *Engine) load(name string) {
	e.logf("> playing %s", name) // client.py Player.load parity (harness key)
	if err := e.Player.Load(name); err != nil {
		e.logf("mpv: %s", err)
	}
}

// OnMpvp is the poll tick (POLL_SECS cadence), client.py poll_loop parity.
func (e *Engine) OnMpvp(s mpv.State) {
	if e.Master {
		e.masterTick(s)
		return
	}
	if s.Pos != nil && e.snap != nil {
		e.followerTick(s)
	}
}

func (e *Engine) masterTick(s mpv.State) {
	if s.Pos == nil && !s.Idle {
		return // nothing loaded yet
	}
	if s.Pause != e.lastPause && !s.Idle {
		if s.Pause {
			e.post1(room.Event{Type: "pause"})
		} else {
			e.post1(room.Event{Type: "play"})
		}
		e.lastPause = s.Pause
	}
	if s.Pos != nil {
		if e.lastPos != nil && abs(*s.Pos-*e.lastPos-PollSecs) > SeekScrubThreshold {
			e.post1(room.Event{Type: "seek", T: *s.Pos}) // user scrubbed
		}
		e.post1(room.Event{Type: "position", T: *s.Pos, P: s.Pause})
		e.lastPos = s.Pos
	}
	if s.Idle && e.wasActive {
		e.post1(room.Event{Type: "ended"}) // EOF: server auto-advances the queue
		e.wasActive = false
	} else if !s.Idle && s.Pos != nil {
		e.wasActive = true
	}
}

func (e *Engine) followerTick(s mpv.State) {
	if e.snap.paused != s.Pause {
		// self-heal: converge to the master's playback state even if the
		// discrete play/pause event never arrived
		_ = e.Player.SetPause(e.snap.paused)
		return
	}
	if !e.snap.paused {
		// age must not grow while the master stands still
		expected := e.snap.t + e.Clock.Since(e.snap.at).Seconds()
		drift := *s.Pos - expected
		e.emitUi(drift)
		if abs(drift) > Tolerance {
			_ = e.Player.Seek(expected)
		}
	}
	// both paused: hold position
}

func (e *Engine) emitUi(drift float64) {
	if e.Ui == nil {
		return
	}
	select {
	case e.Ui <- UiState{Drift: drift}:
	default:
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
