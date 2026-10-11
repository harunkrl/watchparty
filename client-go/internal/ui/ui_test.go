package ui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"watchparty/client-go/internal/room"
	"watchparty/client-go/internal/sync"
)

func upd(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	mm, _ := m.Update(msg)
	mod, ok := mm.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", mm)
	}
	return mod
}

func TestRenderEventTable(t *testing.T) {
	cases := []struct {
		ev   room.Event
		want string
	}{
		{room.Event{Type: "hello", Queue: []string{}}, "> room: now=- queue=[]"},
		{room.Event{Type: "hello", Now: "a.mkv", Queue: []string{"b.mkv"}}, "> room: now=a.mkv queue=[b.mkv]"},
		{room.Event{Type: "join", Nick: "bo"}, "> bo joined"},
		{room.Event{Type: "bye", Nick: "bo"}, "> bo left"},
		{room.Event{Type: "chat", Nick: "bo", Text: "hi"}, "[bo] hi"},
		{room.Event{Type: "queue", Queue: []string{"a", "b"}}, "> queue: [a, b]"},
		{room.Event{Type: "load", Name: "a.mkv"}, "> load a.mkv"},
		{room.Event{Type: "play"}, "> play"},
		{room.Event{Type: "pause"}, "> pause"},
		{room.Event{Type: "seek", T: 59.96}, "> seek @60.0s"},
		{room.Event{Type: "position"}, ""}, // heartbeats are never rendered
		{room.Event{Type: "unknown"}, ""},
	}
	for _, tc := range cases {
		if got := RenderEvent(tc.ev); got != tc.want {
			t.Errorf("RenderEvent(%+v) = %q, want %q", tc.ev, got, tc.want)
		}
	}
}

func TestUpdateDiscreteEvents(t *testing.T) {
	m := New("t", nil, nil, false)
	m = upd(t, m, room.Event{Type: "join", Nick: "a"})
	m = upd(t, m, room.Event{Type: "join", Nick: "b"})
	if len(m.peers) != 2 {
		t.Fatalf("peers = %v, want 2", m.peers)
	}
	m = upd(t, m, room.Event{Type: "bye", Nick: "a"})
	if m.peers["a"] || len(m.peers) != 1 {
		t.Fatalf("bye did not remove peer: %v", m.peers)
	}
	m = upd(t, m, room.Event{Type: "load", Name: "x.mkv"})
	if m.now != "x.mkv" {
		t.Fatalf("now = %q, want x.mkv", m.now)
	}
	m = upd(t, m, room.Event{Type: "chat", Nick: "b", Text: "yo"})
	if !contains(m.log, "[b] yo") {
		t.Fatalf("chat not logged: %v", m.log)
	}
	m = upd(t, m, room.Event{Type: "hello", Queue: []string{}})
	if m.conn != "connected" {
		t.Fatalf("conn = %q, want connected", m.conn)
	}
}

func TestStatusLineDriftAndRole(t *testing.T) {
	f := New("t", nil, nil, false)
	m := upd(t, f, sync.UiState{Drift: 0.42})
	if !strings.Contains(m.statusLine(), "drift 0.4s") {
		t.Fatalf("status missing drift: %q", m.statusLine())
	}
	mm := New("t", nil, nil, true)
	mm = upd(t, mm, room.Event{Type: "join", Nick: "a"})
	if !strings.Contains(mm.statusLine(), "master") || !strings.Contains(mm.statusLine(), "1 listener") {
		t.Fatalf("master status wrong: %q", mm.statusLine())
	}
}

func TestReconnectingAndLostStates(t *testing.T) {
	m := New("t", nil, nil, false)
	m = upd(t, m, ReconnectingMsg{Note: "> [conn] lost (x), retrying in 3s"})
	if m.conn != "reconnecting" || !contains(m.log, "> [conn] lost (x), retrying in 3s") {
		t.Fatalf("reconnect state wrong: conn=%q log=%v", m.conn, m.log)
	}
	m = upd(t, m, StreamClosedMsg{})
	if m.conn != "lost" {
		t.Fatalf("conn = %q, want lost", m.conn)
	}
}

func TestInputHistory(t *testing.T) {
	m := New("t", nil, nil, false)
	m.input.SetValue("chat hi")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // submits -> history grows
	if len(m.hist) != 1 || m.hist[0] != "chat hi" {
		t.Fatalf("hist = %v", m.hist)
	}
	m.input.SetValue("")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if m.input.Value() != "chat hi" {
		t.Fatalf("up recalled %q, want chat hi", m.input.Value())
	}
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyDown}) // back to the empty draft
	if m.input.Value() != "" {
		t.Fatalf("down restored %q, want empty draft", m.input.Value())
	}
}

func TestScrollPgUpPgDown(t *testing.T) {
	m := New("t", nil, nil, false)
	for i := 0; i < 30; i++ {
		m = m.logLine(fmt.Sprintf("line-%02d", i))
	}
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyPgUp})
	if m.scroll <= 0 {
		t.Fatal("pgup did not scroll")
	}
	if !strings.Contains(m.View(), "line-00") {
		t.Fatalf("scrolled view misses oldest line (scroll=%d)", m.scroll)
	}
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyPgDown})
	if m.scroll != 0 {
		t.Fatalf("scroll = %d, want 0 after pgdn", m.scroll)
	}
	if !strings.Contains(m.View(), "line-29") {
		t.Fatal("unscrolled view misses newest line")
	}
}

func TestQueueCountInStatus(t *testing.T) {
	m := New("t", nil, nil, false)
	m = upd(t, m, room.Event{Type: "queue", Queue: []string{"a", "b"}})
	if !strings.Contains(m.statusLine(), "q:2") {
		t.Fatalf("status missing queue count: %q", m.statusLine())
	}
	m = upd(t, m, room.Event{Type: "queue", Queue: nil})
	if strings.Contains(m.statusLine(), "q:") {
		t.Fatalf("queue count did not clear: %q", m.statusLine())
	}
}

func TestBareQueueLists(t *testing.T) {
	m := New("t", nil, nil, false)
	m.input.SetValue("queue")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // empty queue -> hint
	if !strings.Contains(m.View(), "queue empty") {
		t.Fatalf("empty queue not reported: %q", m.View())
	}
	m = upd(t, m, room.Event{Type: "queue", Queue: []string{"a.mkv", "b.mkv"}})
	m.input.SetValue("queue")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	v := m.View()
	if !strings.Contains(v, "1. a.mkv") || !strings.Contains(v, "2. b.mkv") {
		t.Fatalf("queue listing wrong:\n%s", v)
	}
}

func TestQueueGuards(t *testing.T) {
	m := New("t", nil, nil, false)
	m.files = []string{"a.mkv", "b.mkv"} // cached list, like after `list`
	m = upd(t, m, room.Event{Type: "hello", Now: "a.mkv", Queue: []string{"b.mkv"}})
	for _, tc := range []struct{ in, want string }{
		{"queue c.mkv", "no such file: c.mkv"},
		{"queue a.mkv", "already playing"},
		{"queue b.mkv", "already queued"},
		{"play c.mkv", "no such file: c.mkv"},
	} {
		mm := m
		mm.input.SetValue(tc.in)
		mm = upd(t, mm, tea.KeyMsg{Type: tea.KeyEnter})
		if !strings.Contains(mm.View(), tc.want) {
			t.Fatalf("%q: want %q in log, got:\n%s", tc.in, tc.want, mm.View())
		}
	}
}

func TestPickerQEnqueues(t *testing.T) {
	m := New("t", nil, nil, false)
	m.mode = modePick
	m = upd(t, m, filesMsg{files: []string{"a.mkv", "b.mkv"}})
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyDown}) // cursor on b.mkv
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if m.mode != modeInput {
		t.Fatal("q did not leave pick mode")
	}
	if !strings.Contains(m.View(), "> queued b.mkv") {
		t.Fatalf("no queued log:\n%s", m.View())
	}
}

func TestPickerQGuards(t *testing.T) {
	m := New("t", nil, nil, false)
	m.mode = modePick
	m = upd(t, m, filesMsg{files: []string{"a.mkv"}})
	m = upd(t, m, room.Event{Type: "hello", Now: "a.mkv"})
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if m.mode != modeInput {
		t.Fatal("q did not leave pick mode")
	}
	v := m.View()
	if !strings.Contains(v, "already playing") || strings.Contains(v, "> queued") {
		t.Fatalf("picker q bypassed guard:\n%s", v)
	}
}

func TestStatusShowsPosition(t *testing.T) {
	m := New("t", nil, nil, false)
	p := 65.0
	m = upd(t, m, sync.UiState{Pos: &p, Paused: true})
	s := m.statusLine()
	if !strings.Contains(s, "⏸ 1:05") {
		t.Fatalf("status missing paused clock: %q", s)
	}
	m = upd(t, m, sync.UiState{Pos: &p, Paused: false})
	if !strings.Contains(m.statusLine(), "▶ 1:05") {
		t.Fatalf("status missing playing clock: %q", m.statusLine())
	}
}

func TestPickerFlow(t *testing.T) {
	m := New("t", nil, nil, false)
	m.mode = modePick
	m = upd(t, m, filesMsg{files: []string{"a.mkv", "b.mkv", "c.mkv"}})
	if len(m.pickItems) != 3 || m.pickCursor != 0 {
		t.Fatalf("picker items = %v cursor = %d", m.pickItems, m.pickCursor)
	}
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.pickCursor != 1 {
		t.Fatalf("cursor = %d, want 1", m.pickCursor)
	}
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyUp})
	if m.pickCursor != 0 {
		t.Fatalf("cursor = %d, want 0", m.pickCursor)
	}
	esc := upd(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if esc.mode != modeInput {
		t.Fatal("esc did not leave picker")
	}
	// enter selects: back to input mode + local "> playing" log; the
	// returned playFile cmd is deliberately not executed (nil player).
	ent := upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if ent.mode != modeInput || !contains(ent.log, "> playing a.mkv") {
		t.Fatalf("picker enter wrong: mode=%v log=%v", ent.mode, ent.log)
	}
}

func contains(log []string, s string) bool {
	for _, l := range log {
		if l == s {
			return true
		}
	}
	return false
}
