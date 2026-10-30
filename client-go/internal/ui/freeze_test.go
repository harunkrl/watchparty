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
	if el := time.Since(start); el > time.Second {
		t.Fatalf("the UI froze for %s with a full log channel", el)
	}
	if len(pl.pauses) != 1 {
		t.Fatalf("the room was not paused: %v", pl.pauses)
	}
}
