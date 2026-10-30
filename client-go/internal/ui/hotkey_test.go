package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"watchparty/client-go/internal/room"
	"watchparty/client-go/internal/sync"
)

// recPlayer records what the engine asked mpv to do.
type recPlayer struct {
	pauses []bool
	seeks  []float64
}

func (p *recPlayer) Load(string) error          { return nil }
func (p *recPlayer) SetPause(v bool) error      { p.pauses = append(p.pauses, v); return nil }
func (p *recPlayer) Seek(t float64) error       { p.seeks = append(p.seeks, t); return nil }
func (p *recPlayer) Speed(float64) error        { return nil }
func (p *recPlayer) ShowText(string, int) error { return nil }
func (p *recPlayer) Pos() (float64, bool)       { return 0, false }

// W1: on Windows bubbletea delivers ctrl+space as a NUL rune (KeyRunes),
// not as "ctrl+@" — the room play/pause hotkey must work for both, and
// the NUL must not land in the chat input.
func TestNulRuneTogglesPause(t *testing.T) {
	pl := &recPlayer{}
	e := &sync.Engine{
		Master: true, ID: "ada-1", Player: pl,
		Clock: sync.RealClock{},
		Post:  func(room.Event) error { return nil },
	}
	m := New("ada", nil, nil, true, e)
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{0}})
	if len(pl.pauses) != 1 || !pl.pauses[0] {
		t.Fatalf("ctrl+space (NUL) did not toggle the room: pauses=%v", pl.pauses)
	}
	if v := m.input.Value(); v != "" {
		t.Fatalf("the NUL leaked into the chat input: %q", v)
	}
}

// G1: the clickable bar must move the CLICKER's own player too — the
// server never echoes a poster's own event back, so a POST-only click
// does nothing for the master and snaps back for a follower.
func TestBarClickSeeksTheLocalPlayer(t *testing.T) {
	pl := &recPlayer{}
	var posted []room.Event
	e := &sync.Engine{
		Master: true, ID: "ada-1", Player: pl,
		Clock: sync.RealClock{},
		Post:  func(ev room.Event) error { posted = append(posted, ev); return nil },
	}
	m := New("ada", nil, nil, true, e)
	pos := 10.0
	m.duration = 100
	m.st = &sync.UiState{Pos: &pos}
	// the stacked layout draws the bar on row 3, from column 3 to 3+cw
	m.barClick(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 41, Y: 3})
	if len(pl.seeks) != 1 {
		t.Fatalf("the click did not seek the local player: %v", pl.seeks)
	}
	if pl.seeks[0] < 49 || pl.seeks[0] > 51 {
		t.Fatalf("the click seeked to %v, want ~50 (half of 100s)", pl.seeks[0])
	}
	if len(posted) != 1 || posted[0].Type != "seek" {
		t.Fatalf("the click did not tell the room: %v", posted)
	}
}
