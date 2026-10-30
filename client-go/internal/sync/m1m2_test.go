package sync

// Batch C pins (M1 claim-freshness family, M2 stall family, W3).
// Fake player/clock come from engine_test.go + stall_test.go (same
// package).

import (
	"errors"
	"testing"
	"time"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
)

// M1-B: a master parked at EOF (the film ended, the queue is empty) must
// keep heartbeating: the posts hold the room's clock claim (a parked
// master used to lose it after 45s and a second --master could steal the
// clock silently) and they keep the followers from showing a false
// "stale" chip.
func TestIdleMasterKeepsHeartbeating(t *testing.T) {
	e, _, po := newEngine(true)
	t0 := time.Unix(5000, 0)
	e.Clock = stepClock{now: t0}
	e.OnMpvp(mpv.State{Pos: ptr(100), Pause: false}) // playing: one heartbeat
	e.OnMpvp(mpv.State{Idle: true})                  // EOF: mpv is idle
	for i := 0; i < 3; i++ {
		e.Clock = stepClock{now: t0.Add(time.Duration(2+i*2) * time.Second)}
		e.OnMpvp(mpv.State{Idle: true})
	}
	// the parked master must keep posting the room's clock
	pos := 0
	for i, ev := range po.events {
		if ev.Type != "position" {
			continue
		}
		if i == 0 {
			continue // the heartbeat from while it was still playing
		}
		pos++
		if !ev.P {
			t.Fatalf("the idle heartbeat must carry the paused flag: %+v", ev)
		}
		if ev.T != 100 {
			t.Fatalf("the idle heartbeat must carry the last position: %+v", ev)
		}
	}
	if pos < 4 {
		t.Fatalf("the parked master posted %d heartbeats, want the idle ones too: %v", pos, evType(t, po.events))
	}
}

// M1-B: the follower's snapshot stays fresh while the master is parked —
// no false "stale" chip at the end of a film.
func TestParkedMasterKeepsTheFollowerFresh(t *testing.T) {
	ui := make(chan UiState, 8)
	e, _, _ := newEngine(false)
	e.Ui = ui
	t0 := time.Unix(5000, 0)
	e.Clock = stepClock{now: t0}
	e.OnRoom(masterAt(100, false))
	// the master is parked: it keeps heartbeating (paused at the end)
	for i := 1; i <= 5; i++ {
		e.Clock = stepClock{now: t0.Add(time.Duration(i*2) * time.Second)}
		e.OnRoom(masterAt(100, true))
		e.OnMpvp(mpv.State{Pos: ptr(100), Pause: true})
	}
	// drain and look at the LAST state (P3-2: the first one was 2s old —
	// the stale chip only shows after 6s)
	var last UiState
	for draining := true; draining; {
		select {
		case st := <-ui:
			last = st
		default:
			draining = false
		}
	}
	if last.Stale {
		t.Fatalf("a parked master must not look stale: %+v", last)
	}
}

// master-2 P2-1: a reconnect storm must not move the resume deadline: N
// hellos may not extend the episode, and the backstop must still fire the
// stall (a reconnecting follower must not hold the room hostage).
func TestReconnectStormDoesNotExtendTheDeadline(t *testing.T) {
	e, pl, po := newEngine(false)
	e.dur = 100            // we know the duration: the hello is applied at once
	e.loaded = "movie.mkv" // the same file: the hello does not reload it
	pl.pos = ptr(40)       // our position is known: the hello jump is measurable
	t0 := time.Unix(5000, 0)
	e.Clock = stepClock{now: t0}
	e.OnRoom(masterAt(100, false))
	e.Clock = stepClock{now: t0.Add(2 * time.Second)}
	e.OnMpvp(mpv.State{Pos: ptr(40), Pause: false}) // a resume (60s behind)
	deadline := e.resumeTil
	// the SSE keeps dropping: a hello every 25s (the readIdle window)
	clock := t0.Add(2 * time.Second)
	for i := 0; i < 8; i++ {
		clock = clock.Add(25 * time.Second)
		e.Clock = stepClock{now: clock}
		e.OnRoom(room.Event{Type: "hello", Now: "movie.mkv", Pos: ptr(160 + float64(i*25))})
		e.OnMpvp(mpv.State{Pos: ptr(40), Pause: false})
	}
	if e.resumeTil.After(deadline) {
		t.Fatalf("the hellos moved the resume deadline: %v > %v", e.resumeTil, deadline)
	}
	// the backstop must still fire the stall
	for i := 0; i < 20 && countType(t, po.events, "pause") == 0; i++ {
		clock = clock.Add(5 * time.Second)
		e.Clock = stepClock{now: clock}
		e.OnRoom(masterAt(100+clock.Sub(t0).Seconds(), false))
		e.OnMpvp(mpv.State{Pos: ptr(40), Pause: false})
	}
	if n := countType(t, po.events, "pause"); n == 0 {
		t.Fatalf("the reconnect storm held the room hostage: %v", evType(t, po.events))
	}
}

// M1-A: a follower whose master is really dead (the heartbeat went stale)
// advances the queue — otherwise nobody ever would.
func TestFollowerPostsEndedWhenTheMasterIsDead(t *testing.T) {
	e, _, po := newEngine(false)
	e.loaded = "movie.mkv" // we played the file to its end
	t0 := time.Unix(5000, 0)
	e.Clock = stepClock{now: t0}
	e.OnRoom(masterAt(100, false)) // the master's last heartbeat
	// our file is over (mpv idle) and the master's heartbeat is stale
	e.Clock = stepClock{now: t0.Add(10 * time.Second)}
	e.OnMpvp(mpv.State{Idle: true})
	if n := countType(t, po.events, "ended"); n != 1 {
		t.Fatalf("the dead room was not advanced: %v", evType(t, po.events))
	}
}

// P3-1: a follower that never played anything must not post 'ended' — the
// nothing-playing path would spam the room's notice every 10s.
func TestNothingPlayingFollowerDoesNotPostEnded(t *testing.T) {
	e, _, po := newEngine(false)
	t0 := time.Unix(5000, 0)
	e.Clock = stepClock{now: t0}
	e.OnRoom(masterAt(100, false))
	e.Clock = stepClock{now: t0.Add(10 * time.Second)}
	e.OnMpvp(mpv.State{Idle: true})
	if n := countType(t, po.events, "ended"); n != 0 {
		t.Fatalf("a follower with no file posted 'ended': %v", evType(t, po.events))
	}
}

// M1-A: while the master is alive (a parked master still heartbeats), the
// follower must NOT post 'ended' — that would end a film that is only
// waiting for the next one.
func TestFollowerDoesNotEndALiveRoom(t *testing.T) {
	e, _, po := newEngine(false)
	e.loaded = "movie.mkv" // we played the file to its end
	t0 := time.Unix(5000, 0)
	e.Clock = stepClock{now: t0}
	for i := 1; i <= 5; i++ {
		e.Clock = stepClock{now: t0.Add(time.Duration(i*2) * time.Second)}
		e.OnRoom(masterAt(100, true)) // the parked master still heartbeats
		e.OnMpvp(mpv.State{Idle: true})
	}
	if n := countType(t, po.events, "ended"); n != 0 {
		t.Fatalf("a live room must not be ended: %v", evType(t, po.events))
	}
}

// M2-a: a sleep/resume WITH an SSE drop (hello lands mid-resume) must
// not mechanically pause the room: the hello clears the old window, the
// refill looks like starvation (m2-501).
func TestHelloMidResumeDoesNotPauseTheRoom(t *testing.T) {
	e, pl, po := newEngine(false)
	e.dur = 100            // we know the duration: the hello is applied at once
	e.loaded = "movie.mkv" // the same file: the hello does not reload it
	pl.pos = ptr(40)       // our position is known: the hello jump is measurable
	t0 := time.Unix(5000, 0)
	e.Clock = stepClock{now: t0}
	e.OnRoom(masterAt(100, false))
	// we are 60s behind: a resume (the seek flushes the cache)
	e.Clock = stepClock{now: t0.Add(2 * time.Second)}
	e.OnMpvp(mpv.State{Pos: ptr(40), Pause: false})
	// the SSE drops and comes back: the hello carries the live position
	e.Clock = stepClock{now: t0.Add(4 * time.Second)}
	e.OnRoom(room.Event{Type: "hello", Now: "movie.mkv", Pos: ptr(160)})
	// the cache is filling: two buffering ticks — the old code stalled here
	e.Clock = stepClock{now: t0.Add(6 * time.Second)}
	e.OnMpvp(mpv.State{Pos: ptr(40), Pause: false, Buffering: true})
	e.Clock = stepClock{now: t0.Add(8 * time.Second)}
	e.OnMpvp(mpv.State{Pos: ptr(40), Pause: false, Buffering: true})
	if n := countType(t, po.events, "pause"); n != 0 {
		t.Fatalf("the reconnect paused the room: %v", evType(t, po.events))
	}
}

// M2-b (W5): the resume episode follows the CACHE, not the clock: a cache
// that is still filling after the old fixed 30s must not be treated as
// starvation.
func TestResumeStaysOpenWhileTheCacheFills(t *testing.T) {
	e, _, po := newEngine(false)
	t0 := time.Unix(5000, 0)
	e.Clock = stepClock{now: t0}
	e.OnRoom(masterAt(100, false))
	e.Clock = stepClock{now: t0.Add(2 * time.Second)}
	e.OnMpvp(mpv.State{Pos: ptr(40), Pause: false}) // a resume
	// 40s of filling: past the old 30s window, still buffering
	for i := 1; i <= 10; i++ {
		e.Clock = stepClock{now: t0.Add(time.Duration(2+i*4) * time.Second)}
		e.OnRoom(masterAt(100+float64(2+i*4), false)) // the master plays on
		e.OnMpvp(mpv.State{Pos: ptr(40), Pause: false, Buffering: true})
	}
	if n := countType(t, po.events, "pause"); n != 0 {
		t.Fatalf("the filling cache was treated as starvation: %v", evType(t, po.events))
	}
}

// M2-b: the honest backstop — a resume that never converges must still
// reach the normal stall logic (a broken link must not be pause-proof
// forever).
func TestResumeThatNeverConvergesStillStalls(t *testing.T) {
	e, _, po := newEngine(false)
	t0 := time.Unix(5000, 0)
	e.Clock = stepClock{now: t0}
	e.OnRoom(masterAt(100, false))
	e.Clock = stepClock{now: t0.Add(2 * time.Second)}
	e.OnMpvp(mpv.State{Pos: ptr(40), Pause: false}) // a resume
	// the bound expires: the follower is still far behind and NOT
	// buffering (a broken link that never recovers)
	clock := t0.Add(2 * time.Second)
	for clock.Sub(t0) < resumeBound+30*time.Second {
		clock = clock.Add(10 * time.Second)
		e.Clock = stepClock{now: clock}
		e.OnRoom(masterAt(100+clock.Sub(t0).Seconds(), false))
		e.OnMpvp(mpv.State{Pos: ptr(40), Pause: false})
	}
	if n := countType(t, po.events, "pause"); n == 0 {
		t.Fatalf("a resume that never converged never stalled: %v", evType(t, po.events))
	}
}

// W3: a failed stall-pause post must be re-armed: the post rides the same
// starving link, so the failure is the EXPECTED case there. Without the
// re-arm the follower never seeks again and never pauses the room: it
// would stay behind forever while the room plays on.
func TestFailedStallPostIsRearmed(t *testing.T) {
	pl := &fakePlayer{}
	var posted []room.Event
	fail := true
	e := &Engine{
		Master: false, Player: pl, Clock: stepClock{now: time.Unix(5000, 0)},
		Post: func(ev room.Event) error {
			posted = append(posted, ev)
			if fail {
				fail = false
				return errors.New("network is down")
			}
			return nil
		},
	}
	t0 := time.Unix(5000, 0)
	e.OnRoom(masterAt(100, false))
	e.Clock = stepClock{now: t0.Add(2 * time.Second)}
	e.OnMpvp(mpv.State{Pos: ptr(95), Pause: false}) // 7s behind
	e.OnMpvp(mpv.State{Pos: ptr(95), Pause: false}) // stall -> the post fails
	if e.autoPaused {
		t.Fatal("a failed post must not leave the stall disarmed")
	}
	e.Clock = stepClock{now: t0.Add(4 * time.Second)}
	e.OnRoom(masterAt(102, false))
	e.OnMpvp(mpv.State{Pos: ptr(95), Pause: false}) // the stall is detected again
	e.Clock = stepClock{now: t0.Add(6 * time.Second)}
	e.OnRoom(masterAt(104, false))
	e.OnMpvp(mpv.State{Pos: ptr(95), Pause: false}) // and the pause lands
	if n := countType(t, posted, "pause"); n != 2 {
		t.Fatalf("the stall pause was not retried: %v", posted)
	}
	if !e.autoPaused {
		t.Fatal("the retry did not arm the stall again")
	}
}
