package ui

import (
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
