package sync

import (
	"sync"
	"testing"
	"time"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
)

// F6-A1: the load settle must run OFF the engine lock behind a guaranteed
// signal. A wedged fake (WaitLive blocks until released) must leave OnRoom
// prompt; StartLoad (the loadfile tell) fires under the lock; the settle
// (sub attach) completes only when the signal fires.

func TestLoadSettleRunsOffTheLock(t *testing.T) {
	pl := &fakePlayer{wedgeWait: make(chan struct{})}
	e := &Engine{
		Master: true, ID: "ada-1", Player: pl,
		Clock: RealClock{},
		Post:  func(room.Event) error { return nil },
	}
	done := make(chan time.Time, 1)
	go func() {
		e.OnRoom(room.Event{Type: "load", Name: "a.mkv"})
		done <- time.Now()
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("OnRoom blocked on the load chain (the settle did not go off the lock)")
	}
	if pl.startLoads != 1 {
		t.Fatalf("the loadfile tell did not fire: %d", pl.startLoads)
	}
	if e.loaded != "a.mkv" {
		t.Fatalf("the speculative claim is missing (a hello in the settle window would reload the old file): %q", e.loaded)
	}
	if pl.attachCalls != 0 {
		t.Fatal("the settle ran before the live signal")
	}
	close(pl.wedgeWait) // the guaranteed signal fires (the fake's own WaitLive)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && pl.attachCalls != 1 {
		time.Sleep(10 * time.Millisecond)
	}
	if pl.attachCalls != 1 {
		t.Fatal("the settle never completed after the signal")
	}
}

// F6-A2: SeekBy must use the polled position field — zero Pos() round
// trips (each one is a ≤5s cmd under e.mu on a wedged mpv).
func TestSeekByUsesThePolledPosition(t *testing.T) {
	pl := &fakePlayer{}
	e := &Engine{
		Master: true, ID: "ada-1", Player: pl,
		Clock: RealClock{},
		Post:  func(room.Event) error { return nil },
	}
	pos := 10.0
	e.OnMpvp(mpv.State{Pos: &pos, At: time.Now()})
	e.SeekBy(30)
	if pl.posCalls != 0 {
		t.Fatalf("SeekBy paid %d Pos() round trips, want 0", pl.posCalls)
	}
	if len(pl.seeks) != 1 || pl.seeks[0] != 40 {
		t.Fatalf("the seek went to %v, want 40 (polled 10 + 30)", pl.seeks)
	}
}

// F6-A3: a wedged server must not grow the queue without end, and the
// stale heartbeat flood must collapse: N seeks + N heartbeats queue, the
// wire later carries every seek IN ORDER and exactly ONE position (the
// latest), and a pure discrete flood clamps at the bound.
func TestPostQueueCoalescesAndBounds(t *testing.T) {
	var mu sync.Mutex
	var delivered []room.Event
	release := make(chan struct{})
	pl := &fakePlayer{}
	e := &Engine{
		Master: true, ID: "ada-1", Player: pl,
		Clock: RealClock{},
		Post: func(ev room.Event) error {
			<-release // wedged server
			mu.Lock()
			delivered = append(delivered, ev)
			mu.Unlock()
			return nil
		},
	}
	pos := 10.0
	for i := 0; i < 70; i++ { // discrete flood past the bound (oldest pauses die)
		e.PauseToggle()
	}
	e.posts.mu.Lock()
	depth := len(e.posts.items)
	e.posts.mu.Unlock()
	if depth > postQueueMax {
		t.Fatalf("the queue grew to %d, want ≤ %d", depth, postQueueMax)
	}
	for i := 0; i < 10; i++ {
		e.OnMpvp(mpv.State{Pos: &pos, At: time.Now()}) // heartbeat position
		e.SeekBy(30)                                   // discrete seek
	}
	e.posts.mu.Lock()
	depth = len(e.posts.items)
	e.posts.mu.Unlock()
	if depth > postQueueMax {
		t.Fatalf("the queue grew to %d, want ≤ %d", depth, postQueueMax)
	}
	close(release) // the server answers: drain everything
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		e.posts.mu.Lock()
		depth = len(e.posts.items)
		e.posts.mu.Unlock()
		if depth == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if depth != 0 {
		t.Fatalf("the queue never drained: %d left", depth)
	}
	mu.Lock()
	defer mu.Unlock()
	positions, seeks := 0, 0
	for _, ev := range delivered {
		switch ev.Type {
		case "position":
			positions++
		case "seek":
			seeks++
		}
	}
	if positions != 1 {
		t.Fatalf("position posts delivered: %d, want exactly 1 (the latest)", positions)
	}
	if seeks != 10 {
		t.Fatalf("seek posts delivered: %d, want 10 (discretes keep order)", seeks)
	}
}
