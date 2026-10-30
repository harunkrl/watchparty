package ui

// F4: the human feedback round.
//   (1) bare /queue and bare /play open the picker in the matching mode:
//       the enqueue action must not live behind a hidden key.
//   (2) the ctrl-shortcut matrix: every advertised shortcut × every mode,
//       asserted by EFFECT on the real Model (headless — no terminal, no
//       tmux). The matrix itself is in
//       .pi-state/analysis5/worker2-f4-raw.md.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"watchparty/client-go/internal/room"
	"watchparty/client-go/internal/sync"
)

// --- (1) the picker's mode -----------------------------------------------

// Bare /queue opens the picker in queue mode: enter enqueues. Before
// F4 the only way to enqueue from the picker was the hidden q key.
func TestBareQueueOpensThePickerInQueueMode(t *testing.T) {
	posted := make(chan room.Event, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev room.Event
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			t.Errorf("bad /cmd body: %v", err)
			return
		}
		posted <- ev
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	api := room.New(srv.URL, "ada-1")
	m := New("ada", api, nil, false, nil)
	m.files = []string{"a.mkv"}
	m.input.SetValue("/queue")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modePick {
		t.Fatalf("bare /queue did not open the picker (mode=%v)", m.mode)
	}
	if m.pickAct != pickActQueue {
		t.Fatalf("bare /queue opened the picker in the wrong mode: %v", m.pickAct)
	}
	m = upd(t, m, filesMsg{files: []string{"a.mkv"}})
	mm, c := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(Model)
	if c != nil {
		c() // the post happens in the command
	}
	select {
	case ev := <-posted:
		if ev.Type != "enqueue" || ev.Name != "a.mkv" {
			t.Fatalf("enter did not enqueue: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the queue event was not posted")
	}
}

// Bare /play opens the picker in play mode: enter plays.
func TestBarePlayOpensThePickerInPlayMode(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m.files = []string{"a.mkv"}
	m.input.SetValue("/play")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modePick {
		t.Fatalf("bare /play did not open the picker (mode=%v)", m.mode)
	}
	if m.pickAct != pickActPlay {
		t.Fatalf("bare /play opened the picker in the wrong mode: %v", m.pickAct)
	}
	m = upd(t, m, filesMsg{files: []string{"a.mkv"}})
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !contains(logTexts(m.log), "> playing a.mkv") {
		t.Fatalf("enter did not play: %v", logTexts(m.log))
	}
}

// With an argument both verbs keep their direct behaviour.
func TestPlayAndQueueWithArgumentStayDirect(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m.files = []string{"a.mkv"}
	m.input.SetValue("/play a.mkv")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode == modePick {
		t.Fatal("/play <file> opened the picker instead of playing")
	}
	if !contains(logTexts(m.log), "> playing a.mkv") {
		t.Fatalf("/play <file> did not play: %v", logTexts(m.log))
	}
	m.input.SetValue("/queue a.mkv")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode == modePick {
		t.Fatal("/queue <file> opened the picker instead of enqueueing")
	}
	if !contains(logTexts(m.log), "> queued a.mkv") {
		t.Fatalf("/queue <file> did not enqueue: %v", logTexts(m.log))
	}
}

// The picker footer must say what the keys do IN THE CURRENT MODE: a
// footer that lies about enter is worse than no footer.
func TestPickerFooterStatesTheCurrentMode(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlP}) // ctrl+p: the picker, play mode
	m = upd(t, m, filesMsg{files: []string{"a.mkv"}})
	view := m.View()
	if !strings.Contains(view, "enter play") {
		t.Fatalf("the play-mode footer does not say what enter does:\n%s", view)
	}
	if strings.Contains(view, "enter enqueue") {
		t.Fatalf("the play-mode footer promises the queue action:\n%s", view)
	}
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEscape}) // back
	m.input.SetValue("/queue")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = upd(t, m, filesMsg{files: []string{"a.mkv"}})
	view = m.View()
	if !strings.Contains(view, "enter enqueue") {
		t.Fatalf("the queue-mode footer does not say what enter does:\n%s", view)
	}
	if !strings.Contains(view, "q queue") {
		t.Fatalf("the queue-mode footer hides the q shortcut:\n%s", view)
	}
}

// --- (3) the UI must never wait for the room -----------------------------

// The same with a server that accepts the connection and never answers:
// the UI must stay responsive (the post waits in the background).
func TestCtrlSpaceDoesNotFreezeOnAHungServer(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // the POST never gets an answer
	}))
	defer func() { close(release); srv.Close() }()

	api := room.New(srv.URL, "ada-1")
	pl := &recPlayer{}
	e := &sync.Engine{
		Master: true, ID: "ada-1", Player: pl, Clock: sync.RealClock{},
		Post: func(ev room.Event) error { return api.Post(ev) },
	}
	m := New("ada", api, nil, true, e)
	start := time.Now()
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlAt}) // ctrl+space
	if el := time.Since(start); el > time.Second {
		t.Fatalf("the UI froze for %s on a hung server", el)
	}
	if len(pl.pauses) != 1 {
		t.Fatalf("the room was not paused: %v", pl.pauses)
	}
}

// --- (2) the ctrl-shortcut matrix ----------------------------------------

// matrixPlayer records what the room asked the player to do.
type matrixPlayer struct {
	pauses []bool
	seeks  []float64
}

func (p *matrixPlayer) Load(string) error          { return nil }
func (p *matrixPlayer) SetPause(v bool) error      { p.pauses = append(p.pauses, v); return nil }
func (p *matrixPlayer) Seek(t float64) error       { p.seeks = append(p.seeks, t); return nil }
func (p *matrixPlayer) Speed(float64) error        { return nil }
func (p *matrixPlayer) ShowText(string, int) error { return nil }
func (p *matrixPlayer) Pos() (float64, bool)       { return 30, true } // the seek needs a position

// matrixMode is one context of the matrix: the state the user is in.
type matrixMode struct {
	name string
	prep func(t *testing.T, m Model) Model
}

// matrixModes are the states a shortcut can meet.
func matrixModes() []matrixMode {
	return []matrixMode{
		{"input empty", func(t *testing.T, m Model) Model { return m }},
		{"input filled", func(t *testing.T, m Model) Model {
			m.input.SetValue("merhaba")
			return m
		}},
		{"zen", func(t *testing.T, m Model) Model {
			m.zen = true
			return m
		}},
		{"picker open", func(t *testing.T, m Model) Model {
			m.mode = modePick
			m.pickItems = []string{"a.mkv"}
			return m
		}},
		{"tab menu open", func(t *testing.T, m Model) Model {
			return upd(t, m, tea.KeyMsg{Type: tea.KeyTab})
		}},
		{"dashboard", func(t *testing.T, m Model) Model {
			m.width, m.height = 120, 40
			return m
		}},
	}
}

// matrixKey is one advertised shortcut: the key spellings a terminal may
// deliver and what the shortcut must do.
type matrixKey struct {
	name string
	keys []tea.KeyMsg
	// effect reports what the shortcut did ("" = nothing happened)
	effect func(before, after Model, pl *matrixPlayer) string
}

// The advertised shortcuts (usageTUI + the hotbar).
func matrixKeys() []matrixKey {
	return []matrixKey{
		{"ctrl+space", []tea.KeyMsg{
			{Type: tea.KeyCtrlAt},                  // the ANSI path: 0x00 -> "ctrl+@"
			{Type: tea.KeyRunes, Runes: []rune{0}}, // the windows console path: a NUL rune
		}, func(before, after Model, pl *matrixPlayer) string {
			if len(pl.pauses) == 0 {
				return ""
			}
			return "pause"
		}},
		{"ctrl+left", []tea.KeyMsg{{Type: tea.KeyCtrlLeft}}, func(before, after Model, pl *matrixPlayer) string {
			if len(pl.seeks) == 0 {
				return ""
			}
			return fmt.Sprintf("seek %.0f", pl.seeks[0])
		}},
		{"ctrl+right", []tea.KeyMsg{{Type: tea.KeyCtrlRight}}, func(before, after Model, pl *matrixPlayer) string {
			if len(pl.seeks) == 0 {
				return ""
			}
			return fmt.Sprintf("seek %.0f", pl.seeks[0])
		}},
		{"ctrl+z", []tea.KeyMsg{{Type: tea.KeyCtrlZ}}, func(before, after Model, pl *matrixPlayer) string {
			if before.zen == after.zen {
				return ""
			}
			return "zen"
		}},
		{"ctrl+g", []tea.KeyMsg{{Type: tea.KeyCtrlG}}, func(before, after Model, pl *matrixPlayer) string {
			if before.sysOff == after.sysOff {
				return ""
			}
			return "sys"
		}},
		{"ctrl+p", []tea.KeyMsg{{Type: tea.KeyCtrlP}}, func(before, after Model, pl *matrixPlayer) string {
			if before.mode == modePick || after.mode != modePick {
				return ""
			}
			return "pick"
		}},
		{"tab", []tea.KeyMsg{{Type: tea.KeyTab}}, func(before, after Model, pl *matrixPlayer) string {
			if !after.compOpen {
				return ""
			}
			return "menu"
		}},
	}
}

// The matrix: shortcut × mode. want "" means the shortcut must stay DEAD
// in that mode (the picker owns the keys), anything else is the effect it
// must have.
var matrix = []struct {
	shortcut string
	mode     string
	want     string
}{
	// ctrl+space: play/pause for the room
	{"ctrl+space", "input empty", "pause"},
	{"ctrl+space", "input filled", "pause"},
	{"ctrl+space", "zen", "pause"},
	{"ctrl+space", "tab menu open", "pause"},
	{"ctrl+space", "dashboard", "pause"},
	{"ctrl+space", "picker open", ""}, // the picker owns the keys
	// ctrl+arrows: the seek nudge
	{"ctrl+left", "input empty", "seek 25"},
	{"ctrl+left", "input filled", "seek 25"},
	{"ctrl+left", "zen", "seek 25"},
	{"ctrl+left", "tab menu open", "seek 25"},
	{"ctrl+left", "dashboard", "seek 25"},
	{"ctrl+left", "picker open", ""},
	{"ctrl+right", "input empty", "seek 35"},
	{"ctrl+right", "input filled", "seek 35"},
	{"ctrl+right", "zen", "seek 35"},
	{"ctrl+right", "tab menu open", "seek 35"},
	{"ctrl+right", "dashboard", "seek 35"},
	{"ctrl+right", "picker open", ""},
	// ctrl+z: movie-night mode
	{"ctrl+z", "input empty", "zen"},
	{"ctrl+z", "input filled", "zen"},
	{"ctrl+z", "zen", "zen"},
	{"ctrl+z", "tab menu open", "zen"},
	{"ctrl+z", "dashboard", "zen"},
	{"ctrl+z", "picker open", ""},
	// ctrl+g: the system pane
	{"ctrl+g", "input empty", "sys"},
	{"ctrl+g", "input filled", "sys"},
	{"ctrl+g", "zen", "sys"},
	{"ctrl+g", "tab menu open", "sys"},
	{"ctrl+g", "dashboard", "sys"},
	{"ctrl+g", "picker open", ""},
	// ctrl+p: the file picker
	{"ctrl+p", "input empty", "pick"},
	{"ctrl+p", "input filled", "pick"},
	{"ctrl+p", "zen", "pick"},
	{"ctrl+p", "tab menu open", "pick"},
	{"ctrl+p", "dashboard", "pick"},
	{"ctrl+p", "picker open", ""}, // the picker is already open
	// tab: the completion menu
	{"tab", "input empty", "menu"},
	{"tab", "input filled", ""}, // one candidate fills inline, no menu
	{"tab", "zen", "menu"},
	{"tab", "dashboard", "menu"},
	{"tab", "picker open", ""}, // the picker owns the keys
}

// master-2 P5 (F4): the picker open => the dashboard must not advertise
// the dead ctrl shortcuts. Its row must show the picker's own controls.
// The band hint too: from the picker the bar cannot be clicked and the
// ctrl nudge is dead.
func TestDashboardTellsTheTruthInThePicker(t *testing.T) {
	m := New("ada", nil, nil, true, nil)
	m.width, m.height = 120, 40 // the dashboard layout
	pos := 10.0
	m.duration = 100
	m.st = &sync.UiState{Pos: &pos} // a playing film: the band shows its hint
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlP})
	m = upd(t, m, filesMsg{files: []string{"a.mkv"}})
	view := m.View()
	for _, dead := range []string{"ctrl+space", "ctrl+z", "ctrl+g", "ctrl+p", "[tab]", "ctrl+←", "ctrl+→"} {
		if strings.Contains(view, dead) {
			t.Errorf("the dashboard still advertises %s (dead in the picker):\n%s", dead, view)
		}
	}
	if !strings.Contains(view, "enter play") {
		t.Errorf("the dashboard does not show the picker's controls:\n%s", view)
	}
}

// TestCtrlShortcutMatrix drives the real Model headless and asserts the
// EFFECT of every advertised shortcut in every mode.
func TestCtrlShortcutMatrix(t *testing.T) {
	keys := map[string]matrixKey{}
	for _, k := range matrixKeys() {
		keys[k.name] = k
	}
	modes := map[string]matrixMode{}
	for _, mo := range matrixModes() {
		modes[mo.name] = mo
	}
	for _, row := range matrix {
		k, ok := keys[row.shortcut]
		if !ok {
			t.Fatalf("the matrix names an unknown shortcut: %s", row.shortcut)
		}
		mo, ok := modes[row.mode]
		if !ok {
			t.Fatalf("the matrix names an unknown mode: %s", row.mode)
		}
		for _, key := range k.keys {
			pl := &matrixPlayer{}
			e := &sync.Engine{
				Master: true, ID: "ada-1", Player: pl, Clock: sync.RealClock{},
				Post: func(room.Event) error { return nil },
			}
			m := New("ada", nil, nil, true, e)
			m = mo.prep(t, m)
			before := m
			beforePl := *pl
			m = upd(t, m, key)
			got := k.effect(before, m, pl)
			_ = beforePl
			if got != row.want {
				t.Errorf("%s in %s: effect %q, want %q", row.shortcut, row.mode, got, row.want)
			}
			// a shortcut must never land in the chat input
			if row.shortcut != "tab" && m.input.Value() != before.input.Value() {
				t.Errorf("%s in %s: the key leaked into the input (%q)", row.shortcut, row.mode, m.input.Value())
			}
		}
	}
}

// waitPost waits for the engine's asynchronous post: the engine hands
// the events to its worker, so the caller never waits for the network.
func waitPost(t *testing.T, ch chan room.Event) room.Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("the engine posted nothing")
		return room.Event{}
	}
}
