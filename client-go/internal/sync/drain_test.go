package sync

// The quit path must not lose the last action: the post worker may still
// hold events when the program exits (the master ruling: DRAIN).

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"watchparty/client-go/internal/room"
)

// An event queued before the quit must be on the wire after the drain.
func TestDrainDeliversTheQueuedPosts(t *testing.T) {
	e, _, _ := newEngine(true)
	posted := make(chan room.Event, 4)
	e.Post = func(ev room.Event) error {
		time.Sleep(200 * time.Millisecond) // a slow room
		posted <- ev
		return nil
	}
	e.OnMpvEvent("pause") // the last action, queued just before the quit
	if !e.Drain(2 * time.Second) {
		t.Fatal("the drain timed out")
	}
	select {
	case ev := <-posted:
		if ev.Type != "pause" {
			t.Fatalf("the wire got %+v, want the pause", ev)
		}
	default:
		t.Fatal("the queued pause never reached the wire")
	}
}

// A hung network must not keep the program alive: the drain gives up —
// and says so, because a lost pause is a lost room state.
func TestDrainTimeoutIsLogged(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	e, _, _ := newEngine(true)
	logged := make(chan string, 8)
	e.Logf = func(f string, a ...any) { logged <- fmt.Sprintf(f, a...) }
	e.Post = func(ev room.Event) error {
		<-release // the post never finishes
		return nil
	}
	e.OnMpvEvent("pause")
	if e.Drain(200 * time.Millisecond) {
		t.Fatal("the drain must not wait for a hung network forever")
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case line := <-logged:
			if strings.Contains(line, "1 event(s) lost") {
				return
			}
		case <-deadline:
			t.Fatal("the lost event was not reported at all")
		}
	}
}
