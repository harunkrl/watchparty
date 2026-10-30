package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"watchparty/client-go/internal/room"
	"watchparty/client-go/internal/sync"
)

// F5 "keys for a whole movie": the new hotkeys reach the engine (second
// seek cadence, conhost-proof aliases, local volume) and F1 toggles a
// PASSIVE help overlay — while it is up every other key keeps behaving,
// and only F1/esc close it (plain "?" is banned: the chat input would eat
// it — "?merhaba" must stay typeable).

func f5Model(t *testing.T) (Model, *recPlayer) {
	t.Helper()
	pl := &recPlayer{pos: 10, hasPos: true}
	e := &sync.Engine{
		Master: true, ID: "ada-1", Player: pl,
		Clock: sync.RealClock{},
		Post:  func(room.Event) error { return nil },
	}
	return New("ada", nil, nil, true, e), pl
}

func key(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

func runeKey(r rune, alt bool) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: alt}
}

func TestF5SeekCadenceAndAliases(t *testing.T) {
	m, pl := f5Model(t)
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlShiftRight}) // +30
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlShiftLeft})  // 10-30 -> clamp 0
	if len(pl.seeks) != 2 || pl.seeks[0] != 40 || pl.seeks[1] != 0 {
		t.Fatalf("ctrl+shift+arrows seek: %v, want [40 0]", pl.seeks)
	}
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyLeft, Alt: true}) // -5 (conhost-proof alias)
	if len(pl.seeks) != 3 || pl.seeks[2] != 5 {
		t.Fatalf("alt+left seek: %v, want [... 5]", pl.seeks)
	}
	if v := m.input.Value(); v != "" {
		t.Fatalf("an alias leaked into the chat input: %q", v)
	}
}

func TestF5PauseAlias(t *testing.T) {
	m, pl := f5Model(t)
	m = upd(t, m, runeKey('p', true)) // alt+p
	if len(pl.pauses) != 1 || !pl.pauses[0] {
		t.Fatalf("alt+p did not toggle the room: %v", pl.pauses)
	}
	if v := m.input.Value(); v != "" {
		t.Fatalf("alt+p leaked into the chat input: %q", v)
	}
}

func TestF5LocalVolumeKeys(t *testing.T) {
	m, pl := f5Model(t)
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlUp})   // +5
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlDown}) // -5
	m = upd(t, m, runeKey('m', true))                // alt+m mute
	if len(pl.vols) != 2 || pl.vols[0] != 5 || pl.vols[1] != -5 {
		t.Fatalf("volume keys: %v, want [5 -5]", pl.vols)
	}
	if pl.mutes != 1 {
		t.Fatalf("alt+m did not toggle mute: %d", pl.mutes)
	}
	if v := m.input.Value(); v != "" {
		t.Fatalf("a volume key leaked into the chat input: %q", v)
	}
}

func TestF1PassiveOverlay(t *testing.T) {
	m, _ := f5Model(t)
	m.width, m.height = 120, 40
	if m.helpOpen {
		t.Fatal("the overlay starts open")
	}
	m = upd(t, m, key(tea.KeyF1))
	if !m.helpOpen {
		t.Fatal("F1 did not open the overlay")
	}
	view := m.View()
	if !strings.Contains(view, "[F1") || !strings.Contains(view, "ctrl+shift") {
		t.Fatalf("the overlay/hotbar does not document the keys")
	}
	m = upd(t, m, runeKey('a', false)) // passive: typing keeps working, overlay stays
	if !m.helpOpen {
		t.Fatal("a normal key closed the overlay")
	}
	if v := m.input.Value(); v != "a" {
		t.Fatalf("the key did not reach the chat input while the overlay was up: %q", v)
	}
	m = upd(t, m, key(tea.KeyEsc))
	if m.helpOpen {
		t.Fatal("esc did not close the overlay")
	}
	if v := m.input.Value(); v != "a" {
		t.Fatalf("the closing esc leaked into the input handling: got %q, want 'a' kept", v)
	}
}

// F5-P3: passivity must hold for the WIRE keys too — playback keys stay
// live while the overlay is up (typing alone would be a weak pin).
func TestF1OverlayPassivityWire(t *testing.T) {
	m, pl := f5Model(t)
	m.width, m.height = 120, 40
	m = upd(t, m, key(tea.KeyF1)) // overlay up
	if !m.helpOpen {
		t.Fatal("F1 did not open the overlay")
	}
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{0}}) // ctrl+space (NUL)
	if len(pl.pauses) != 1 || !pl.pauses[0] {
		t.Fatalf("ctrl+space is dead while the overlay is up: %v", pl.pauses)
	}
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlShiftRight}) // +30 over pos 10
	if len(pl.seeks) != 1 || pl.seeks[0] != 40 {
		t.Fatalf("the seek cadence is dead while the overlay is up: %v", pl.seeks)
	}
	if !m.helpOpen {
		t.Fatal("a playback key closed the overlay")
	}
}

// F5-P3: in the picker, esc with the overlay up closes ONLY the overlay —
// the picker must survive (a silent regression would throw the user out
// of the file list when they only wanted the sheet gone).
func TestF1OverlayInPickerEscStopsAtTheOverlay(t *testing.T) {
	m, _ := f5Model(t)
	m.width, m.height = 120, 40
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlP}) // picker opens (play mode)
	if m.mode != modePick {
		t.Fatal("ctrl+p did not open the picker")
	}
	m = upd(t, m, key(tea.KeyF1)) // overlay on top of the picker
	if !m.helpOpen || m.mode != modePick {
		t.Fatal("F1 did not open the overlay over the picker")
	}
	m = upd(t, m, key(tea.KeyEsc)) // closes ONLY the overlay
	if m.helpOpen {
		t.Fatal("esc did not close the overlay")
	}
	if m.mode != modePick {
		t.Fatal("esc closed the picker too — overlay esc must stop at the overlay")
	}
}
