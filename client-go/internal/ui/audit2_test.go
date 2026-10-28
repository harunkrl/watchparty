package ui

// Audit round 2: the non-visual side of the TUI — reconnect handling and
// the key state machines. Helpers (upd, New) come from ui_test.go.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"watchparty/client-go/internal/room"
	"watchparty/client-go/internal/sync"
)

// A reconnect re-sends hello. The room is still on the same file, so the
// duration we already know is still true: dropping it kills the progress
// bar and the click-to-seek until the next load.
func TestHelloKeepsDurationForSameFile(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m.now = "vid.mp4"
	m.duration = 64
	m.st = &sync.UiState{Pos: f64(30)}

	m2 := upd(t, m, room.Event{Type: "hello", Now: "vid.mp4", Queue: []string{}})
	if m2.duration != 64 {
		t.Fatalf("duration lost on reconnect: %v (want 64)", m2.duration)
	}
	if bar := m2.progressBar(60); !strings.Contains(bar, "█") {
		t.Fatalf("progress bar gone after a reconnect: %q", bar)
	}
}

// A file change still resets the duration and re-probes it.
func TestHelloResetsDurationForNewFile(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m.now = "old.mkv"
	m.duration = 64
	m2 := upd(t, m, room.Event{Type: "hello", Now: "new.mkv"})
	if m2.duration != 0 {
		t.Fatalf("stale duration kept for a new file: %v", m2.duration)
	}
	if m2.now != "new.mkv" {
		t.Fatalf("now = %q, want new.mkv", m2.now)
	}
}

// ctrl+c must always quit: the picker owns the keys while it is open,
// and a user who cannot ctrl+c out of it thinks the TUI is stuck.
func TestCtrlCQuitsInPicker(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m.mode = modePick
	m.pickItems = []string{"a.mkv"}
	mm, cmd := m.update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c in the picker did not produce a quit command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("ctrl+c in the picker did not quit")
	}
	_ = mm
}

func f64(f float64) *float64 { return &f }
