package ui

// F9 client pins (ui half): key honesty, the Room flag chips, and the
// SeekBy-idle refusal. Each was RED on the v1.9.0 wiring (see
// .pi-state/f9b-worker1-raw.md).

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
	"watchparty/client-go/internal/sync"
)

func f9model(t *testing.T) (Model, *recPlayer) {
	t.Helper()
	api := room.New("http://127.0.0.1:1", "t9-1")
	pl := &recPlayer{}
	eng := &sync.Engine{
		Master: false, ID: "t9-1", Player: pl, Clock: sync.RealClock{},
		Post: func(ev room.Event) error { return nil },
	}
	m := New("t9", api, nil, false, eng) // a FOLLOWER: the chip lane under test
	m = upd(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	return m, pl
}

// PIN F9.7: an undecodable terminal sequence leaves a VISIBLE, rate
// limited hint (two rapid feeds → one line; after the window → another).
func TestF9UnknownCSIHintRateLimited(t *testing.T) {
	m, _ := f9model(t)
	seq := `?CSI[51 50 59 53 117]?`

	m = m.csiHint(seq)
	m = m.csiHint(seq) // inside the rate window: swallowed
	if got := countEntries(m, "unknown key sequence"); got != 1 {
		t.Fatalf("want exactly 1 hint for rapid feeds, got %d", got)
	}

	time.Sleep(1100 * time.Millisecond)
	m = m.csiHint(seq)
	if got := countEntries(m, "unknown key sequence"); got != 2 {
		t.Fatalf("the hint never re-armed after the window: %d", got)
	}
}

func countEntries(m Model, substr string) int {
	n := 0
	for _, e := range m.log {
		if strings.Contains(e.text, substr) {
			n++
		}
	}
	return n
}

// PIN F9.5: shift and alt+shift arrows are ±30s aliases of the
// ctrl+shift cadence (terminals that collapse or eat the chord).
func TestF9BigStepAliases(t *testing.T) {
	m, pl := f9model(t)
	p := 50.0
	m.engine.OnMpvp(mpv.State{Pos: &p, Pause: false})
	waitTells(t, m.engine)

	// waitTells between presses: the seek tells coalesce-to-latest, so a
	// queued-backlog press legitimately never executes (the F8 law) —
	// each press must drain before the next to pin every alias.
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyShiftRight})
	waitTells(t, m.engine)
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyShiftLeft})
	waitTells(t, m.engine)
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyShiftRight, Alt: true}) // alt+shift+right
	waitTells(t, m.engine)

	if len(pl.seeks) != 3 {
		t.Fatalf("the aliases did not all seek: %v", pl.seeks)
	}
	// the target always computes from the POLLED position (50, F6-A2):
	// 50+30=80, 50-30=20, 50+30=80
	if pl.seeks[0] != 80 || pl.seeks[1] != 20 || pl.seeks[2] != 80 {
		t.Fatalf("the ±30s aliases seeked wrong: %v", pl.seeks)
	}
}

// PIN F9.6: the Room flag drives the chips — no snapshot → the honest
// alone-indicator and NO fabricated drift; snapshot → the drift chip.
func TestF9RoomFlagChips(t *testing.T) {
	m, _ := f9model(t)
	p := 50.0
	m.now = "vid.mp4" // the stat row renders only with a film loaded
	m = upd(t, m, durationMsg{name: "vid.mp4", dur: 100})

	m.st = &sync.UiState{Pos: &p, Room: false}
	pane := strings.Join(m.nowPlayingLines(120), "\n")
	if strings.Contains(pane, "▸ +") {
		t.Fatalf("a lone follower shows a fabricated drift chip: %q", pane)
	}
	if !strings.Contains(pane, "alone — no room") {
		t.Fatalf("the alone-indicator is missing: %q", pane)
	}

	m.st = &sync.UiState{Pos: &p, Room: true, Drift: 0.4}
	pane = strings.Join(m.nowPlayingLines(120), "\n")
	if !strings.Contains(pane, "▸ +0.4s") {
		t.Fatalf("the drift chip vanished with a room present: %q", pane)
	}
	if strings.Contains(pane, "alone — no room") {
		t.Fatalf("the alone-indicator lies while synced: %q", pane)
	}
}

// PIN F9.7b: seeking with nothing loaded refuses VISIBLY — the ±Ns OSD
// lie is gone (engine returns false, the UI names the refusal).
func TestF9SeekByIdleRefuses(t *testing.T) {
	m, pl := f9model(t) // no sample: nothing loaded

	m = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlLeft})
	if m.lastErr == "" || !strings.Contains(m.lastErr, "nothing is playing") {
		t.Fatalf("the idle seek refused silently: %q", m.lastErr)
	}
	if len(pl.seeks) != 0 {
		t.Fatalf("an idle seek moved the player: %v", pl.seeks)
	}
}
