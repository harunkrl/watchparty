package ui

// FREEZE-2: the TUI froze when a log was sent to the program from inside
// the event loop (bubbletea's message channel is unbuffered: p.Send from
// within Update waits for a loop that waits for the Send). The engine's
// log path must never block the UI.

import (
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"watchparty/client-go/internal/room"
	"watchparty/client-go/internal/sync"
)

// With a full log channel the ctrl+space must still return: the log is
// dropped, the UI must not wait for it.
func TestCtrlSpaceWithAFullLogChannelReturns(t *testing.T) {
	ch := make(chan string, 1)
	ch <- "full" // the log channel is full
	pl := &recPlayer{}
	e := &sync.Engine{
		Master: true, ID: "ada-1", Player: pl, Clock: sync.RealClock{},
		Post: func(room.Event) error { return nil },
	}
	e.Logf = func(f string, a ...any) {
		select {
		case ch <- fmt.Sprintf(f, a...):
		default: // the log is dropped: the UI must stay alive
		}
	}
	m := New("ada", nil, nil, true, e)
	start := time.Now()
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlAt}) // ctrl+space
	// 3s, not 2s: a loaded CI runner can pause mid-assert (the v1.6.1
	// widening already moved 1s→2s); the pin guards the forever-deadlock
	// and the 5s wedged-player class, both far above 3s (F7 widening)
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("the UI froze for %s with a full log channel", el)
	}
	waitTells(t, e)
	if len(pl.pauses) != 1 {
		t.Fatalf("the room was not paused: %v", pl.pauses)
	}
}
