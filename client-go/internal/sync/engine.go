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
	Cache  float64 // seconds of read-ahead buffered (demuxer-cache-time)
	Volume float64 // audio volume percent (display only)
}

type Engine struct {
	Master bool
	ID     string // our client id: a notice naming it demotes a second master
	Player Player
	Post   func(room.Event) error
	Clock  Clock
	Ui     chan<- UiState
	Logf   func(format string, args ...any) // nil = discard

	lastPos   *float64
	lastPause bool
	wasActive bool
	snap      *snapshot // follower: latest master position heartbeat

	loaded     string    // file our mpv is playing ("" = none; set on successful load)
	loadedAt   time.Time // when mpv took the file: post-load seek posts are noise
	behind     int       // follower: consecutive polls behind live (starvation signal)
	autoPaused bool      // follower: WE paused the room over a stall

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
	e.lastSeekAt = e.lastActuate
	e.seekEcho = true // swallow exactly one echo (see OnMpvEvent)
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
		// only (re)load when the file actually differs from what our mpv is
		// already playing: an SSE blip must not restart a mid-film download (F3)
		if ev.Now != "" && ev.Now != e.loaded {
			e.load(ev.Now)
		}
		// a reconnect is a fresh stall window: a behind-tick from before
		// the outage must not trip the stall counter on the first tick
		// after it (the room would be paused for a stall that is over)
		e.behind, e.autoPaused = 0, false
		// Server may carry an age-adjusted media position + paused flag
		// from the room's live snapshot (set after every position relay,
		// cleared by load/ended/skip). When present, snap now and seek
		// before the first heartbeat would even arrive.
		if ev.Pos != nil {
			e.snap = &snapshot{t: *ev.Pos, paused: ev.P, at: e.Clock.Now()}
			// skip the seek when already there: on an SSE blip this is a
			// sub-second rewind that would flash the same frame (F3)
			if t, ok := e.Player.Pos(); !ok || abs(t-*ev.Pos) > Tolerance {
				e.seekTo(*ev.Pos)
			}
			if ev.P {
				e.setPause(true)
			}
		}
	case "load":
		if ev.Name != e.loaded {
			e.load(ev.Name)
		}
	case "pause":
		e.setPause(true)
		e.lastPause = true  // master dedup stays consistent with reality
		e.postedPause = nil // the room's state supersedes our pending post
		if e.snap != nil {
			e.snap.paused = true // discrete events are room truth; don't wait for the next heartbeat
		}
	case "play":
		e.autoPaused = false // the room resumed: our stall-pause (if any) is spent
		e.behind = 0         // fresh stall window: resume-into-dry-cache must not re-pause instantly
		e.setPause(false)
		e.lastPause = false
		e.postedPause = nil // the room's state supersedes our pending post
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
		// chat OSD over the video (client.py parity: 3s show-text)
		_ = e.Player.ShowText(fmt.Sprintf("%s: %s", ev.Nick, ev.Text), 3000)
	case "notice":
		// room-level warning (server election, R7). If it names US, a second
		// --master is running: demote to follower instead of dueling — the
		// owner's heartbeats are already arriving, sync resumes within a tick.
		if ev.From != "" && ev.From == e.ID && e.Master {
			e.Master = false
			e.logf("> demoted to follower: another master owns the clock")
		}
		// OSD so both users see it even with the TUI window hidden
		_ = e.Player.ShowText(ev.Text, 4000)
	}
}

func (e *Engine) load(name string) {
	e.setSpeed(1) // new file: clear any tempo nudge from the previous one
	e.behind, e.autoPaused = 0, false
	e.wasActive = false          // the new file gets its own EOF: the old one is over
	e.logf("> playing %s", name) // client.py Player.load parity (harness key)
	if err := e.Player.Load(name); err != nil {
		e.logf("mpv: %s", err)
		return
	}
	e.loaded = name // claim it only once mpv actually took the file
	e.loadedAt = e.Clock.Now()
	e.lastPos = nil // a new file starts at the head: the old scrub baseline is stale
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
}

// PauseToggle is the TUI hotkey equivalent of a space-bar press in the
// mpv window: apply locally AND post — the server never echoes a poster's
// own event back, so posting alone would leave this player unsynced.
func (e *Engine) PauseToggle() {
	e.mu.Lock()
	defer e.mu.Unlock()
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
	typ := "play"
	if want {
		typ = "pause"
	}
	e.postDiscrete(room.Event{Type: typ})
}

// SeekBy is the TUI hotkey nudge (ctrl+arrow): seek locally and post the
// target — seekTo marks the echo so the resulting mpv event is consumed,
// never re-posted.
func (e *Engine) SeekBy(dt float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, ok := e.Player.Pos()
	if !ok {
		return
	}
	t = max(0, t+dt)
	e.seekTo(t)
	e.lastPost = e.Clock.Now()
	e.postDiscrete(room.Event{Type: "seek", T: t})
	if e.Master {
		e.lastPos = nil // scrub-check restarts fresh (no double post)
	}
}

// OnMpvp is the poll tick (POLL_SECS cadence), client.py poll_loop parity.
func (e *Engine) OnMpvp(s mpv.State) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// EOF transition (either role): at idle mpv keeps pause=false and drops
	// time-pos, so without this the UI sits on a stale ▶ forever. Ended
	// reads as paused until the next load.
	if s.Idle && e.wasActive {
		e.wasActive = false
		e.emitUi(mpv.State{Pause: true}, 0)
		if e.Master {
			e.post1(room.Event{Type: "ended"}) // server auto-advances the queue
		}
	} else if !s.Idle && s.Pos != nil {
		e.wasActive = true
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
		e.post1(room.Event{Type: "position", T: *s.Pos, P: s.Pause})
		e.lastPos = s.Pos
		e.emitUi(s, 0) // master: no drift, but the status bar wants pos/paused
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
		// the follower UI sits on a stale ▶/⏸ while mpv is already there
		e.setPause(e.snap.paused)
		e.emitUi(mpv.State{Pos: s.Pos, Pause: e.snap.paused, Cache: s.Cache, Volume: s.Volume}, 0)
		return
	}
	if e.snap.paused {
		e.setSpeed(1)  // paused: clear any tempo nudge for a clean resume
		e.emitUi(s, 0) // both paused: hold position — the UI still tracks mpv
		return
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
		e.setSpeed(1)
		expected := e.snap.t + e.Clock.Since(e.snap.at).Seconds()
		e.emitUi(s, *s.Pos-expected)
		return
	}
	// age must not grow while the master stands still
	expected := e.snap.t + e.Clock.Since(e.snap.at).Seconds()
	drift := *s.Pos - expected
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
		e.seekTo(expected) // ahead of live: clock jump or a lagging master
	case abs(drift) > speedBand:
		e.behind = 0
		// smooth converge: a small tempo nudge instead of a visible seek
		e.setSpeed(1 + clamp(-drift*0.1, -speedAdj, speedAdj))
	default:
		e.behind = 0
		e.setSpeed(1) // inside the dead zone: plain 1x
	}
}

// stallBlock: the follower is starving while the room plays — typically a
// WAN link below the file's bitrate. Never hard-seek: the seek would
// flush the cache that is filling, re-stall and repeat every tick
// (seek-storm, F2). The true backlog shows as drift; the room is paused
// once so the cache can refill. No auto-resume: a wire pause carries no
// owner, auto-resuming could fight a deliberate pause — humans press play.
func (e *Engine) stallBlock(s mpv.State) {
	e.setSpeed(1)
	expected := e.snap.t + e.Clock.Since(e.snap.at).Seconds()
	e.emitUi(s, *s.Pos-expected)
	if e.autoPaused {
		return // we already paused the room for this stall episode
	}
	e.autoPaused = true
	e.snap.paused = true // mirror our post: heartbeats carry the pre-post state briefly
	e.lastPost = e.Clock.Now()
	e.postDiscrete(room.Event{Type: "pause"})
	_ = e.Player.ShowText("buffering - room paused; press play when ready", 4000)
}

func (e *Engine) emitUi(s mpv.State, drift float64) {
	if e.Ui == nil {
		return
	}
	select {
	case e.Ui <- UiState{Drift: drift, Pos: s.Pos, Paused: s.Pause, Cache: s.Cache, Volume: s.Volume}:
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
