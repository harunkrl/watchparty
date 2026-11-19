package ui

// L1 lobby-first UI pins: the card, enter-resume (empty enter was a
// silent no-op before), the invite surfaces and the master auto-picker
// latch. RED on the v1.9.3 tree (no lobby concept existed); raw trail in
// .pi-state/l1-worker1-raw.md.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"watchparty/client-go/internal/room"
	"watchparty/client-go/internal/sync"
)

func ptrL(f float64) *float64 { return &f }

func lobbyEngineModel(t *testing.T, master bool) (Model, *sync.Engine) {
	t.Helper()
	api := room.New("http://127.0.0.1:1", "t-l1")
	pl := &recPlayer{}
	eng := &sync.Engine{
		Master: master, ID: "t-l1", Player: pl, Clock: sync.RealClock{},
		Post: func(ev room.Event) error { return nil },
	}
	m := New("t9", api, nil, master, eng)
	m = upd(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	return m, eng
}

// idleHello drives the engine into the lobby state through its real gate.
func lobbyHello() room.Event {
	return room.Event{
		Type: "hello", Now: "a.mkv", Roster: []string{"me"},
		Pos: ptrL(754), P: true,
	}
}

// PIN L1.9: the card renders exactly the contract line — raw file name,
// mm:ss of the lobby position, the pause glyph when paused, the resume
// hint and the invite line. Without a lobby it renders nothing.
func TestLobbyCardRenders(t *testing.T) {
	m, eng := lobbyEngineModel(t, false)
	eng.OnRoom(lobbyHello())
	if !eng.ResumeLobby() {
		t.Fatalf("the engine never held the lobby (harness broken)")
	}
	m.st = &sync.UiState{Lobby: &sync.Lobby{Now: "a.mkv", Pos: ptrL(754), Paused: true}}
	m.invite = "http://100.89.11.31:8090" // a hello happened before the card
	card := strings.Join(m.lobbyLines(120), "\n")
	for _, want := range []string{"last watched: a.mkv", "12:34", "⏸", "enter resume", "/play pick another", "invite:"} {
		if !strings.Contains(card, want) {
			t.Fatalf("the card is missing %q:\n%s", want, card)
		}
	}
	m.st = &sync.UiState{} // no lobby: no card
	if lines := m.lobbyLines(120); len(lines) != 0 {
		t.Fatalf("an empty lobby rendered a card: %v", lines)
	}
}

// PIN L1.10: enter on an EMPTY input resumes the lobby (the key was an
// unbound silent no-op before) — the engine's lobby clears and no
// refusal is logged.
func TestEmptyEnterResumesLobby(t *testing.T) {
	m, eng := lobbyEngineModel(t, false)
	eng.OnRoom(lobbyHello())
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if eng.ResumeLobby() {
		t.Fatalf("enter on empty input did not resume the lobby (it is still up)")
	}
	if got := countEntries(m, "nothing to resume"); got != 0 {
		t.Fatalf("a successful resume logged a refusal (%d times)", got)
	}
}

// PIN L1.11: enter on an empty input WITHOUT a lobby refuses visibly
// (F9 yardstick) — it must not send an empty chat either.
func TestEmptyEnterRefusesWithoutLobby(t *testing.T) {
	m, _ := lobbyEngineModel(t, false)
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if got := countEntries(m, "nothing to resume"); got != 1 {
		t.Fatalf("want exactly 1 refusal, got %d", got)
	}
	for _, e := range m.log {
		if e.kind == entChat {
			t.Fatalf("the refused enter posted a chat: %q", e.text)
		}
	}
}

// PIN L1.12: every hello prints the invite system line (both addresses
// when the server offers a second one).
func TestInviteLineOnHello(t *testing.T) {
	m, _ := lobbyEngineModel(t, false)
	m, _ = m.onEvent(room.Event{
		Type: "hello", Now: "", Queue: nil, Roster: []string{"me"},
		Invite: "http://100.89.11.31:8090", InviteAlt: "http://192.168.1.181:8090",
	})
	line := "invite: http://100.89.11.31:8090 or http://192.168.1.181:8090"
	if got := countEntries(m, line); got != 1 {
		t.Fatalf("the hello invite line is missing (%d): log=%v", got, m.log)
	}
}

// PIN L1.13: /invite re-prints the stored address; without one it
// refuses visibly.
func TestInviteCommand(t *testing.T) {
	m, _ := lobbyEngineModel(t, false)
	m.invite = "http://100.89.11.31:8090"
	m, _ = m.dispatch("/invite")
	if got := countEntries(m, "invite: http://100.89.11.31:8090"); got != 1 {
		t.Fatalf("/invite did not re-print the stored address")
	}
	m.invite = ""
	m, _ = m.dispatch("/invite")
	if got := countEntries(m, "no invite address yet"); got != 1 {
		t.Fatalf("/invite without an address did not refuse visibly")
	}
}

// PIN L1.14 (§7): the MASTER's first hello into an empty room opens the
// picker exactly once (latch); a follower never auto-opens.
func TestMasterAutoPickerLatch(t *testing.T) {
	m, _ := lobbyEngineModel(t, true)
	hello := room.Event{Type: "hello", Now: "", Roster: []string{"me"}}
	m, _ = m.onEvent(hello)
	if m.mode != modePick {
		t.Fatalf("the master's first empty hello did not open the picker (mode=%v)", m.mode)
	}
	m.mode = modeInput // the human closed it
	m, _ = m.onEvent(hello)
	if m.mode == modePick {
		t.Fatalf("the latch re-opened the picker on a later hello")
	}

	fm, _ := lobbyEngineModel(t, false) // a follower never auto-opens
	fm, _ = fm.onEvent(hello)
	if fm.mode == modePick {
		t.Fatalf("a follower auto-opened the picker")
	}
}

// PIN L1.15: the resume handoff is visible in the F1 sheet and the
// usage line — the keys must be discoverable (§B.4).
func TestLobbyKeysDocumented(t *testing.T) {
	m, _ := lobbyEngineModel(t, false)
	sheet := strings.Join(m.helpLines(), "\n")
	if !strings.Contains(sheet, "resume") || !strings.Contains(sheet, "/invite") {
		t.Fatalf("the F1 sheet does not document the lobby keys / /invite")
	}
	if !strings.Contains(usageTUI, "/invite") {
		t.Fatalf("usageTUI does not mention /invite")
	}
}

// PIN L1.16 (worker2's pty blocker): the DASHBOARD path renders the
// card too — a real terminal (≥100 cols) shows the dashboard, and the
// card was invisible there (the very case L1 exists for). The chat
// viewport pays the card's rows: the box must not overflow the frame.
func TestLobbyCardOnDashboard(t *testing.T) {
	m, _ := lobbyEngineModel(t, false)
	m = upd(t, m, tea.WindowSizeMsg{Width: 200, Height: 50})
	if !m.dashOn() {
		t.Fatalf("harness: 200x50 must take the dashboard path")
	}
	base := m.chatBoxH() // the chat height without a card
	m.st = &sync.UiState{Lobby: &sync.Lobby{Now: "a.mkv", Pos: ptrL(42), Paused: false}}
	dash := m.dashView()
	if !strings.Contains(dash, "last watched: a.mkv") || !strings.Contains(dash, "0:42") {
		t.Fatalf("the dashboard never renders the lobby card")
	}
	if got := m.chatBoxH(); got != base-1 {
		t.Fatalf("the card rows came from nowhere: chat %d, want %d (base-1)", got, base-1)
	}
}
