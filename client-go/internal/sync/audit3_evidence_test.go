//go:build audit3

package sync

// Audit round 3 (worker2) — EVIDENCE. These tests FAIL against the current
// tree on purpose: each one documents a reported bug. They are behind the
// `audit3` build tag so the normal battery stays green.
//
// Run: go test -tags audit3 ./internal/sync/

import (
	"testing"
	"time"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
)

// A3-SYNC1: the stale-heartbeat flag measures the age of the SNAPSHOT, and
// the snapshot is re-anchored by a discrete "play" event (the R5 fix for
// the resume jump). So a room that keeps sending discrete play/pause
// events keeps the snapshot "fresh" even when the master's heartbeat
// stopped long ago: the amber "master heartbeat stale" hint never
// appears. The freshness of the heartbeat must be measured on its own
// clock, not shared with the drift re-anchor.
func TestAudit3StaleIsNotMaskedByADiscretePlay(t *testing.T) {
	ui := make(chan UiState, 8)
	e, _, _ := newEngine(false)
	e.Ui = ui
	t0 := time.Unix(7000, 0)
	e.Clock = stepClock{now: t0}
	e.OnRoom(masterAt(10, false)) // the master's last heartbeat, at t0
	e.Clock = stepClock{now: t0.Add(7 * time.Second)}
	e.OnRoom(room.Event{Type: "play"}) // a discrete play event re-anchors the snapshot
	e.Clock = stepClock{now: t0.Add(8 * time.Second)}
	e.OnMpvp(mpv.State{Pos: ptr(18), Pause: false})
	st := <-ui
	if !st.Stale {
		t.Fatalf("no heartbeat for 7s, but the snapshot looks fresh: %+v", st)
	}
}

// A3-SYNC3 (PASSES — it documents the boundary of the claim): "the master
// is never flagged stale" was attacked three ways and holds:
//  1. the master holds no snapshot at all: its own heartbeats are
//     echo-filtered, so e.snap stays nil
//  2. a master that reconnects DOES hold a snapshot (the hello event),
//     but the stale flag is follower-only
//  3. after a self-demotion the engine is a follower again and is then
//     flagged exactly like any follower
func TestAudit3MasterIsNeverStale(t *testing.T) {
	ui := make(chan UiState, 8)
	e, _, _ := newEngine(true)
	e.ID = "ada-1" // the notice names us by id — that is what demotes us
	e.Ui = ui
	t0 := time.Unix(8000, 0)
	e.Clock = stepClock{now: t0}
	e.OnMpvp(mpv.State{Pos: ptr(1), Dur: 600})         // the local player knows the duration
	e.OnRoom(room.Event{Type: "hello", Pos: ptr(10)})  // a reconnect: the master holds a snapshot
	e.Clock = stepClock{now: t0.Add(30 * time.Second)} // no heartbeat for 30s
	e.OnMpvp(mpv.State{Pos: ptr(40), Pause: false})
	if st := lastUi(ui); st.Stale {
		t.Fatalf("the master was flagged stale: %+v", st)
	}
	// after a self-demotion the same engine is a follower and IS flagged
	e.OnRoom(room.Event{Type: "notice", From: e.ID, Text: "two masters: bo owns the clock"})
	e.Clock = stepClock{now: t0.Add(31 * time.Second)}
	e.OnMpvp(mpv.State{Pos: ptr(41), Pause: false})
	if st := lastUi(ui); !st.Stale {
		t.Fatalf("the demoted engine is not flagged stale: %+v", st)
	}
}

// A3-F1a: the room's position is clamped to the file we actually have.
// The local player knows the duration (600s); the room says 1:00:10 —
// seeking there would EOF the player.
func TestAudit3HelloPosIsClampedToTheLocalDuration(t *testing.T) {
	e, pl, _ := newEngine(false)
	e.OnMpvp(mpv.State{Pos: ptr(1), Dur: 600}) // the local player knows the duration
	pos := 3610.0
	e.OnRoom(room.Event{Type: "hello", Pos: &pos})
	if len(pl.seeks) != 1 {
		t.Fatalf("the clamped position was not applied: %v", pl.seeks)
	}
	if pl.seeks[0] > 599 {
		t.Fatalf("applied position %v is past the end of the file (dur 600)", pl.seeks[0])
	}
}

// A3-F1b: the same path with an UNKNOWN duration (mpv reports it a moment
// after the load, and a reconnect has no fresh sample yet). The position
// must be held back — not seeked unclamped: falling past the end of the
// file is F1 itself.
func TestAudit3HelloPosWaitsForTheUnknownDuration(t *testing.T) {
	e, pl, _ := newEngine(false)
	pos := 3610.0
	e.OnRoom(room.Event{Type: "hello", Pos: &pos})
	if len(pl.seeks) != 0 {
		t.Fatalf("seeked with an unknown duration: %v", pl.seeks)
	}
	// the first sample that knows the duration applies it, clamped
	e.OnMpvp(mpv.State{Pos: ptr(1), Dur: 600})
	if len(pl.seeks) == 0 {
		t.Fatalf("the held-back position was never applied: %v", pl.seeks)
	}
	if pl.seeks[0] > 599 {
		t.Fatalf("applied position %v is past the end of the file (dur 600)", pl.seeks[0])
	}
}

// A3-SEEK: the server rejects a negative seek target (A3, worker1), so
// the client must clamp it: a local apply and the relayed event would
// otherwise disagree (ctrl+left at 0:00 would seek to -5s locally while
// the relay carries -5 and is dropped).
func TestAudit3SeekTargetIsNeverNegative(t *testing.T) {
	e, pl, po := newEngine(false)
	pl.pos = ptr(0)
	e.SeekBy(-5) // ctrl+left at the very head
	for _, ev := range po.all(t) {
		if ev.T < 0 {
			t.Fatalf("a negative seek target was posted: %+v", ev)
		}
	}
	for _, s := range pl.seeks {
		if s < 0 {
			t.Fatalf("a negative seek was applied locally: %v", pl.seeks)
		}
	}
}

// A3-SYNC2: the poll samples mpv in its own goroutine, so a sample taken
// BEFORE a load can be handed to the engine AFTER the room already moved
// to the next file. The master then posts a heartbeat for the new file
// carrying the OLD file's position and duration; the server stores it
// (state.now is already the new file) and a joining client is clamped to
// the wrong duration.
func TestAudit3StaleSampleDoesNotPoisonTheHeartbeat(t *testing.T) {
	e, _, po := newEngine(true)
	e.OnMpvp(mpv.State{Pos: ptr(10), Dur: 120}) // file A: the heartbeat carries A's duration
	before := len(po.all(t))
	e.OnRoom(room.Event{Type: "load", Name: "b.mkv"})                  // the room advances to file B
	e.OnMpvp(mpv.State{Pos: ptr(10), Dur: 120, At: time.Unix(999, 0)}) // the poll's stale sample of file A
	for _, ev := range po.all(t)[before:] {
		if ev.Type == "position" && ev.Dur != 0 {
			t.Fatalf("a heartbeat after the load carries the previous file's duration: %+v", ev)
		}
	}
}
