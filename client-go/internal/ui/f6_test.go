package ui

// F6 D3: the local actions (seek, volume, mute) leave an OSD hint in the
// mpv window — the movie window is often hidden behind the TUI, so the log
// line alone is invisible feedback. The hint is LOCAL: the room never
// hears a volume or a mute (only the seek is a room action).

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
	"watchparty/client-go/internal/sync"
)

func TestF6LocalActionsShowAnOSDHint(t *testing.T) {
	pl := &recPlayer{pos: 10, hasPos: true}
	posted := make(chan room.Event, 8)
	e := &sync.Engine{
		// a FOLLOWER on purpose: a master posts its own heartbeat from the
		// seed (F6-A2 made the seed necessary) and the strict "only the
		// seek reaches the room" assert would have to tolerate it. A
		// follower posts no position -> the strict assert holds again
		// (master-2 review, F6 gate).
		Master: false, ID: "ada-1", Player: pl,
		Clock: sync.RealClock{},
		Post:  func(ev room.Event) error { posted <- ev; return nil },
	}
	pos := 10.0
	e.OnMpvp(mpv.State{Pos: &pos, At: time.Now()}) // F6-A2: the seek reads the polled position
	m := New("ada", nil, nil, true, e)
	var hints []string
	m.osdHook = func(s string) { hints = append(hints, s) }

	m = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlUp})    // volume +
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlDown})  // volume -
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlRight}) // seek +5s
	m = upd(t, m, runeKey('m', true))                 // mute

	want := []string{"vol +5%", "vol -5%", "+5s", "muted"}
	if len(hints) != len(want) {
		t.Fatalf("OSD hints: %v, want %v", hints, want)
	}
	for i := range want {
		if hints[i] != want[i] {
			t.Fatalf("OSD hints: %v, want %v", hints, want)
		}
	}
	// the volume/mute are local: the room may only have heard the seek
	seekSeen := false
	deadline := time.Now().Add(250 * time.Millisecond)
	var heard []string
	for time.Now().Before(deadline) {
		select {
		case ev := <-posted:
			heard = append(heard, ev.Type)
		case <-time.After(60 * time.Millisecond):
		}
	}
	for _, typ := range heard {
		if typ != "seek" {
			t.Fatalf("a local action reached the room: %q (heard %v)", typ, heard)
		}
		seekSeen = true
	}
	if !seekSeen {
		t.Fatalf("the seek did not reach the room: %v", heard)
	}
}
