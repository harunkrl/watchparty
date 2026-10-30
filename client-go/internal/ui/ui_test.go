package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/muesli/termenv"
	"watchparty/client-go/internal/cmd"
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

func logTexts(log []entry) []string {
	out := make([]string, 0, len(log))
	for _, e := range log {
		out = append(out, e.text)
	}
	return out
}

func hasChat(log []entry, nick, text string) bool {
	for _, e := range log {
		if e.kind == entChat && e.nick == nick && e.text == text {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, l := range list {
		if l == s {
			return true
		}
	}
	return false
}

func TestBareTextIsChat(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m.input.SetValue("selam, ne izliyoruz?")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !hasChat(m.log, "ada", "selam, ne izliyoruz?") {
		t.Fatalf("bare text not treated as chat: %v", logTexts(m.log))
	}
}

func TestVerbSentencesStayChat(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	for _, line := range []string{"skip it", "quit now please", "play", "list", "queue"} {
		m2 := m
		m2.input.SetValue(line)
		m2 = upd(t, m2, tea.KeyMsg{Type: tea.KeyEnter})
		if !hasChat(m2.log, "ada", line) {
			t.Fatalf("%q must be chat, log = %v", line, logTexts(m2.log))
		}
	}
}

func TestSlashCommands(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m.files = []string{"a.mkv"}
	m.input.SetValue("/play a.mkv")
	mm, c := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(Model)
	if c == nil || !contains(logTexts(m.log), "> playing a.mkv") {
		t.Fatalf("/play dispatch wrong: c=%v log=%v", c, logTexts(m.log))
	}
	m.input.SetValue("/skip")
	m = upd(t, m, room.Event{Type: "queue", Queue: []string{"b.mkv"}}) // something to skip to
	mm, c = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(Model)
	if c == nil {
		t.Fatal("/skip did not return a command")
	}
	m.input.SetValue("/help")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(m.View(), "/pick") {
		t.Fatalf("/help did not show slash usage:\n%s", m.View())
	}
	m.input.SetValue("/")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(m.View(), "plain text is chat") {
		t.Fatalf("lone / did not show usage:\n%s", m.View())
	}
}

func TestSlashEscapeAndErrors(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m.input.SetValue("// /not-a-command")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !hasChat(m.log, "ada", "/ /not-a-command") {
		t.Fatalf("double slash escape wrong: %v", logTexts(m.log))
	}
	m.input.SetValue("/play")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(m.lastErr, "needs an argument") {
		t.Fatalf("error flash missing: %q", m.lastErr)
	}
	var inputRow string
	for _, l := range strings.Split(m.View(), "\n") {
		if strings.Contains(l, "ada ›") {
			inputRow = l // the rendered input line itself, placeholder included
		}
	}
	if !strings.Contains(inputRow, "needs an argument") {
		t.Fatalf("error not on the input row: %q", inputRow)
	}
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m.lastErr != "" {
		t.Fatalf("typing must clear the error flash: %q", m.lastErr)
	}
}

func TestChatVerbStillWorks(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m.input.SetValue("chat eskisi gibi") // bare: now plain chat, same outcome
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !hasChat(m.log, "ada", "chat eskisi gibi") {
		t.Fatalf("bare chat-verb line mangled: %v", logTexts(m.log))
	}
	m.input.SetValue("/chat eskisi gibi")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !hasChat(m.log, "ada", "eskisi gibi") {
		t.Fatalf("/chat verb broken: %v", logTexts(m.log))
	}
}

func TestPaneSeparation(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m = upd(t, m, room.Event{Type: "chat", Nick: "bo", Text: "merhaba"})
	m = upd(t, m, EngineLogMsg{Line: "> pause"})
	chat, sys := strings.Join(m.renderChatLines(), "\n"), strings.Join(m.renderSysLines(), "\n")
	if !strings.Contains(chat, "merhaba") || strings.Contains(chat, "pause") {
		t.Fatalf("chat pane wrong: %q", chat)
	}
	if !strings.Contains(sys, "pause") || strings.Contains(sys, "merhaba") {
		t.Fatalf("system pane wrong: %q", sys)
	}
	if !strings.Contains(m.View(), "system") {
		t.Fatal("View missing the system divider")
	}
}

func TestBareChatTooLong(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	long := strings.Repeat("x", cmd.ChatMaxLen+1)
	m.input.SetValue(long)
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(m.View(), "chat too long") {
		t.Fatalf("length guard missing:\n%s", m.View())
	}
	if hasChat(m.log, "ada", long) {
		t.Fatal("over-long chat must not be sent")
	}
}

func TestRenderEventTable(t *testing.T) {
	big := make([]string, 25)
	for i := range big {
		big[i] = fmt.Sprintf("f%d.mkv", i)
	}
	cases := []struct {
		ev   room.Event
		self string
		want string
	}{
		{room.Event{Type: "hello", Queue: []string{}}, "ada", "> room: now=- queue=[]"},
		{room.Event{Type: "hello", Now: "a.mkv", Queue: []string{"b.mkv"}}, "ada", "> room: now=a.mkv queue=[b.mkv]"},
		{room.Event{Type: "join", Nick: "bo"}, "ada", "> bo joined"},
		{room.Event{Type: "bye", Nick: "bo"}, "ada", "> bo left"},
		{room.Event{Type: "chat", Nick: "bo", Text: "hi"}, "ada", "[bo] hi"},
		{room.Event{Type: "queue", Queue: []string{"a", "b"}}, "ada", "> queue: [a, b]"},
		// R1: a long queue must not swallow the system tail
		{room.Event{Type: "queue", Queue: big}, "ada", "> queue: [f0.mkv, f1.mkv, f2.mkv (+22)]"},
		{room.Event{Type: "load", Name: "a.mkv"}, "ada", "> load a.mkv"},
		{room.Event{Type: "play"}, "ada", "> play"},
		{room.Event{Type: "pause"}, "ada", "> pause"},
		{room.Event{Type: "seek", T: 59.96}, "ada", "> seek @60.0s"},
		{room.Event{Type: "position"}, "ada", ""}, // heartbeats are never rendered
		{room.Event{Type: "unknown"}, "ada", ""},
		// C-F: the room stamps the actor on a state change
		{room.Event{Type: "pause", Nick: "bo"}, "ada", "> pause by bo"},
		{room.Event{Type: "play", Nick: "bo"}, "ada", "> play by bo"},
		{room.Event{Type: "seek", T: 12.3, Nick: "bo"}, "ada", "> seek @12.3s by bo"},
		{room.Event{Type: "load", Name: "a.mkv", Nick: "bo"}, "ada", "> load a.mkv by bo"},
		{room.Event{Type: "queue", Queue: []string{"a"}, Nick: "bo"}, "ada", "> queue: [a] by bo"},
		{room.Event{Type: "load", Name: "a.mkv", Nick: "ada"}, "ada", "> load a.mkv"},
		{room.Event{Type: "queue", Queue: []string{"a"}, Nick: "ada"}, "ada", "> queue: [a]"},
		{room.Event{Type: "pause", Nick: "ada"}, "ada", "> pause"}, // our own action stays bare
		{room.Event{Type: "seek", T: 12.3, Nick: "ada"}, "ada", "> seek @12.3s"},
	}
	for _, tc := range cases {
		if got := RenderEvent(tc.ev, tc.self); got != tc.want {
			t.Errorf("RenderEvent(%+v) = %q, want %q", tc.ev, got, tc.want)
		}
	}
}

func TestUpdateDiscreteEvents(t *testing.T) {
	m := New("t", nil, nil, false, nil)
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
	if !hasChat(m.log, "b", "yo") {
		t.Fatalf("chat not logged: %v", logTexts(m.log))
	}
	m = upd(t, m, room.Event{Type: "hello", Queue: []string{}})
	if m.conn != "connected" {
		t.Fatalf("conn = %q, want connected", m.conn)
	}
}

func TestHelloRosterSeedsWatchers(t *testing.T) {
	m := New("late", nil, nil, false, nil)
	m = upd(t, m, room.Event{Type: "join", Nick: "early"}) // stale join from before hello
	m = upd(t, m, room.Event{Type: "hello", Queue: []string{}, Roster: []string{"ada", "late", "bo"}})
	if len(m.peers) != 2 || !m.peers["ada"] || !m.peers["bo"] || m.peers["late"] || m.peers["early"] {
		t.Fatalf("peers = %v, want exactly ada+bo from the roster", m.peers)
	}
}

func TestFooterShowsDriftAndListeners(t *testing.T) {
	p := 10.0
	f := New("t", nil, nil, false, nil)
	m := upd(t, f, sync.UiState{Drift: 0.42, Pos: &p})
	if !strings.Contains(m.footerLine(m.contentWidth()), "drift +0.4s") {
		t.Fatalf("footer missing drift: %q", m.footerLine(m.contentWidth()))
	}
	mm := New("t", nil, nil, true, nil)
	mm = upd(t, mm, room.Event{Type: "join", Nick: "a"})
	if !strings.Contains(mm.footerLine(mm.contentWidth()), "1 listening") {
		t.Fatalf("master footer wrong: %q", mm.footerLine(mm.contentWidth()))
	}
	l1, _ := mm.headerLines(mm.contentWidth())
	if !strings.Contains(l1, "master") {
		t.Fatalf("header missing role: %q", l1)
	}
}

func TestReconnectingAndLostStates(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, ReconnectingMsg{Note: "> [conn] lost (x), retrying in 3s", Try: 2, Delay: 3})
	if m.conn != "reconnecting" || m.retry != 2 || m.retryDelay != 3 ||
		!contains(logTexts(m.log), "> [conn] lost (x), retrying in 3s") {
		t.Fatalf("reconnect state wrong: conn=%q retry=%d log=%v", m.conn, m.retry, logTexts(m.log))
	}
	chip := m.connChip()
	if !strings.Contains(chip, "retry 2") || !strings.Contains(chip, "backoff 3s") {
		t.Fatalf("reconnect chip wrong: %q", chip)
	}
	m = upd(t, m, StreamClosedMsg{})
	if m.conn != "lost" {
		t.Fatalf("conn = %q, want lost", m.conn)
	}
	m = upd(t, m, room.Event{Type: "hello", Queue: []string{}})
	if !strings.Contains(m.connChip(), "connected") {
		t.Fatalf("hello did not mark connected: %q", m.connChip())
	}
	if m.connSince.IsZero() {
		t.Fatal("hello did not stamp connSince")
	}
}

func TestInputHistory(t *testing.T) {
	m := New("t", nil, nil, false, nil)
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
	m := New("t", nil, nil, false, nil)
	for i := 0; i < 30; i++ { // chat history is the scrollable reading surface
		m = upd(t, m, room.Event{Type: "chat", Nick: "t", Text: fmt.Sprintf("line-%02d", i)})
	}
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyPgUp})
	if m.vp.YOffset != 0 {
		t.Fatalf("pgup did not reach the top (YOffset=%d)", m.vp.YOffset)
	}
	if !strings.Contains(m.View(), "line-00") {
		t.Fatalf("scrolled view misses oldest line (YOffset=%d)", m.vp.YOffset)
	}
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyPgDown})
	if !m.vp.AtBottom() {
		t.Fatalf("pgdown did not return to bottom (YOffset=%d)", m.vp.YOffset)
	}
	if !strings.Contains(m.View(), "line-29") {
		t.Fatal("bottom view misses newest line")
	}
}

func TestFooterQueuePreview(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, room.Event{Type: "queue", Queue: []string{"a.mkv", "b.mkv", "c.mkv"}})
	f := m.footerLine(m.contentWidth())
	for _, want := range []string{"1. a.mkv", "2. b.mkv", "(+1)"} {
		if !strings.Contains(f, want) {
			t.Fatalf("footer queue preview missing %q: %q", want, f)
		}
	}
	m = upd(t, m, room.Event{Type: "queue", Queue: nil})
	if strings.Contains(m.footerLine(m.contentWidth()), "a.mkv") {
		t.Fatalf("queue preview did not clear: %q", m.footerLine(m.contentWidth()))
	}
}

func TestBareQueueLists(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m.input.SetValue("/queue")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter}) // empty queue -> hint
	if !strings.Contains(m.View(), "queue empty") {
		t.Fatalf("empty queue not reported: %q", m.View())
	}
	m = upd(t, m, room.Event{Type: "queue", Queue: []string{"a.mkv", "b.mkv"}})
	m.input.SetValue("/queue")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	v := m.View()
	if !strings.Contains(v, "1. a.mkv") || !strings.Contains(v, "2. b.mkv") {
		t.Fatalf("queue listing wrong:\n%s", v)
	}
}

func TestQueueGuards(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m.files = []string{"a.mkv", "b.mkv"} // cached list, like after `list`
	m = upd(t, m, room.Event{Type: "hello", Now: "a.mkv", Queue: []string{"b.mkv"}})
	for _, tc := range []struct{ in, want string }{
		{"/queue c.mkv", "no such file: c.mkv"},
		{"/queue a.mkv", "already playing"},
		{"/queue b.mkv", "already queued"},
		{"/play c.mkv", "no such file: c.mkv"},
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
	m := New("t", nil, nil, false, nil)
	m.mode = modePick
	m = upd(t, m, filesMsg{files: []string{"a.mkv", "b.mkv"}})
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyDown}) // cursor on b.mkv
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if m.mode != modeInput {
		t.Fatal("q did not leave pick mode")
	}
	if !strings.Contains(m.View(), "queued b.mkv") {
		t.Fatalf("no queued log:\n%s", m.View())
	}
}

func TestPickerQGuards(t *testing.T) {
	m := New("t", nil, nil, false, nil)
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

func TestHeaderClockShowsPosition(t *testing.T) {
	p := 65.0
	m := New("t", nil, nil, false, nil)
	m.now = "movie.mkv"
	m = upd(t, m, sync.UiState{Pos: &p, Paused: true})
	_, l2 := m.headerLines(m.contentWidth())
	if !strings.Contains(l2, "⏸") || !strings.Contains(l2, "1:05") {
		t.Fatalf("header missing paused clock: %q", l2)
	}
	m = upd(t, m, sync.UiState{Pos: &p, Paused: false})
	_, l2 = m.headerLines(m.contentWidth())
	if !strings.Contains(l2, "▶") {
		t.Fatalf("header missing playing icon: %q", l2)
	}
	m.duration = 600
	_, l2 = m.headerLines(m.contentWidth())
	if !strings.Contains(l2, "1:05 / 10:00") {
		t.Fatalf("header missing total: %q", l2)
	}
	if _, _, showBar, _, _ := m.layout(); !showBar {
		t.Fatal("progress bar should show with pos+duration")
	}
	if !strings.Contains(m.progressBar(m.contentWidth()), "█") {
		t.Fatalf("progress bar empty: %q", m.progressBar(m.contentWidth()))
	}
}

func TestEngineRunCollapse(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	for i := 0; i < 3; i++ {
		m = m.logEngine("> play")
	}
	out := strings.Join(m.renderSysLines(), "\n")
	if strings.Count(out, "play") != 1 || !strings.Contains(out, "(2)") { // icon-prefixed now
		t.Fatalf("3 identical engine lines did not collapse: %q", out)
	}
	m = m.logEngine("> pause")
	out = strings.Join(m.renderSysLines(), "\n")
	if !strings.Contains(out, "play") || !strings.Contains(out, "pause") {
		t.Fatalf("type change must end the run: %q", out)
	}
	m = m.logEngine("> play") // alternating play/pause: every state stays visible
	m = m.logChat("ada", "hello")
	m = m.logEngine("> play")
	out = strings.Join(m.renderSysLines(), "\n")
	if strings.Count(out, "play") != 3 { // 1 collapsed + 1 before chat + 1 after: the run did not cross the chat
		t.Fatalf("chat must break an engine run: %q", out)
	}
}

func TestNudgeFilter(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	before := len(m.log)
	m = m.logEngine("[1.050]")
	m = m.logEngine("[0.970]")
	if len(m.log) != before {
		t.Fatalf("speed-nudge lines must be filtered, log = %v", logTexts(m.log))
	}
	m = m.logEngine("> pause")
	if len(m.log) != before+1 {
		t.Fatal("filter swallowed a real engine line")
	}
}

func TestWrapIndented(t *testing.T) {
	lines := wrapIndented("hello world this is a fairly long chat message", 20, 4, "")
	for i, l := range lines {
		if lipgloss.Width(l) > 20 {
			t.Fatalf("line %d overflows: %q", i, l)
		}
		if i > 0 && !strings.HasPrefix(l, "    ") {
			t.Fatalf("continuation line %d not indented: %q", i, l)
		}
	}
	cut := wrapIndented("supercalifragilisticexpialidocious", 12, 0, "")
	if len(cut) != 1 || lipgloss.Width(cut[0]) > 12 || !strings.HasSuffix(cut[0], "…") {
		t.Fatalf("overlong word not hard-cut: %q", cut)
	}
	one := wrapIndented("short", 20, 4, "")
	if len(one) != 1 || one[0] != "short" {
		t.Fatalf("plain line mangled: %q", one)
	}
}

func TestPickerFlow(t *testing.T) {
	m := New("t", nil, nil, false, nil)
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
	if ent.mode != modeInput || !contains(logTexts(ent.log), "> playing a.mkv") {
		t.Fatalf("picker enter wrong: mode=%v log=%v", ent.mode, logTexts(ent.log))
	}
}

func TestCtrlPOpensPicker(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlP})
	if m.mode != modePick {
		t.Fatal("ctrl+p did not open the picker")
	}
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != modeInput {
		t.Fatal("esc did not close the picker")
	}
}

func TestListOpensPicker(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m.input.SetValue("/list")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modePick {
		t.Fatal("/list did not open the picker (2-line system strip cannot hold a file dump)")
	}
}

func TestOwnLoadUpdatesNow(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, loadedMsg{name: "movie.mkv"})
	if m.now != "movie.mkv" {
		t.Fatalf("own load did not set now: %q", m.now)
	}
	m = upd(t, m, durationMsg{name: "movie.mkv", dur: 3600 + 200})
	if m.duration != 3800 {
		t.Fatalf("duration not stored: %v", m.duration)
	}
	m = upd(t, m, durationMsg{name: "other.mkv", dur: 99})
	if m.duration != 3800 {
		t.Fatalf("stale duration accepted: %v", m.duration)
	}
}

// --- UI overhaul (A/U batch) -----------------------------------------------

func TestTabCompletionVerb(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m.input.SetValue("/pl")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if got := m.input.Value(); got != "/play " {
		t.Fatalf("single verb match: got %q, want /play ", got)
	}
	m.input.SetValue("/p")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyTab}) // ambiguous: pick/play -> menu
	if !m.compOpen {
		t.Fatal("ambiguous matches must open the menu")
	}
	var pick, play bool
	for _, it := range m.compItems {
		if it.text == "/pick" {
			pick = true
		}
		if it.text == "/play" {
			play = true
		}
	}
	if !pick || !play {
		t.Fatalf("menu candidates: %+v", m.compItems)
	}
	// arrow to /play, enter accepts
	for i, it := range m.compItems {
		if it.text == "/play" {
			m.compCursor = i
		}
	}
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if got := m.input.Value(); got != "/play " || m.compOpen {
		t.Fatalf("menu accept: got %q open=%v", got, m.compOpen)
	}
}

func TestTabCompletionFile(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m = upd(t, m, filesMsg{files: []string{"movie one.mkv", "movie two.mkv", "song.flac"}})
	m.input.SetValue("/play movie t")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if got := m.input.Value(); got != "/play movie two.mkv " {
		t.Fatalf("file completion: got %q", got)
	}
}

func TestZenModeCollapses(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 90, Height: 30}) // stacked zone: >=100 is the dashboard
	_, vh, showBar, showSys, showFooter := m.layout()
	if !showSys || !showFooter {
		t.Fatalf("precondition: optional rows visible at h=30 (%v %v %v)", showBar, showSys, showFooter)
	}
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlZ})
	_, vhZen, showBar2, showSys2, showFooter2 := m.layout()
	if showBar2 || showSys2 || showFooter2 {
		t.Fatalf("zen hides optional rows: %v %v %v", showBar2, showSys2, showFooter2)
	}
	if vhZen <= vh {
		t.Fatalf("zen chat height grew: %d -> %d", vh, vhZen)
	}
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlZ})
	if _, _, _, showSys3, _ := m.layout(); !showSys3 {
		t.Fatal("ctrl+z toggles back")
	}
}

func TestSysToggle(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlG})
	if _, _, _, showSys, _ := m.layout(); showSys {
		t.Fatal("ctrl+g hides the system pane")
	}
}

func TestChatTimestampsGroupSameNick(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m = m.logChat("ada", "one")
	m = m.logChat("ada", "two")
	m = m.logChat("bo", "three")
	lines := m.renderChatLines()
	if len(lines) != 3 {
		t.Fatalf("lines: %q", lines)
	}
	if strings.Contains(lines[1], "ada") {
		t.Fatalf("grouped continuation must hide the nick: %q", lines[1])
	}
	if !strings.Contains(lines[0], "ada") || !strings.Contains(lines[2], "bo") {
		t.Fatalf("timestamps/nicks missing: %q", lines)
	}
	if !strings.Contains(lines[0], ":") || !strings.Contains(lines[2], ":") {
		t.Fatalf("first lines lack the HH:MM gutter: %q", lines)
	}
	if !strings.Contains(lines[1], "▏") { // grouped line: speaker anchor, no repeated time
		t.Fatalf("grouped continuation lacks the anchor: %q", lines[1])
	}
}

func TestSystemGutter(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m = m.logEvent("> bo joined")
	m = m.logEngine("> seek @1.0s")
	for _, l := range m.renderSysLines() {
		if !strings.HasPrefix(l, gutStyle.Render("▏")) {
			t.Fatalf("system line lacks gutter: %q", l)
		}
	}
}

func TestJoinLeftColored(t *testing.T) {
	// tests run without a TTY: force ANSI so lipgloss actually emits codes
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(orig)
	m := New("ada", nil, nil, false, nil)
	if got := m.renderEntry(entry{kind: entEvent, text: "> bo joined"}, 60, false, false)[0]; got == dim.Render("· bo joined") {
		t.Fatalf("join line still renders dim: %q", got)
	}
	if got := m.renderEntry(entry{kind: entEvent, text: "> bo left"}, 60, false, false)[0]; got == dim.Render("· bo left") {
		t.Fatalf("left line still renders dim: %q", got)
	}
}

func TestInviteHint(t *testing.T) {
	api := room.New("http://100.101.102.103:8090", "ada-1")
	m := New("ada", api, nil, false, nil)
	m = m.syncViewport()
	if !strings.Contains(m.vp.View(), "100.101.102.103:8090") {
		t.Fatalf("invite hint missing: %q", m.vp.View())
	}
	m = m.logChat("ada", "hi")
	m = m.syncViewport()
	if strings.Contains(m.vp.View(), "invite") {
		t.Fatalf("hint should vanish once chat exists: %q", m.vp.View())
	}
}

func TestClickBarSeeks(t *testing.T) {
	pl := &recPlayer{}
	var posted []room.Event
	e := &sync.Engine{
		Master: true, ID: "ada-1", Player: pl, Clock: sync.RealClock{},
		Post: func(ev room.Event) error { posted = append(posted, ev); return nil },
	}
	m := New("ada", nil, nil, false, e)
	m = upd(t, m, tea.WindowSizeMsg{Width: 90, Height: 30}) // stacked layout: the dashboard owns >=100
	pos := 10.0
	m = upd(t, m, sync.UiState{Pos: &pos, Paused: false, Drift: 0})
	m.now = "f"
	m = upd(t, m, durationMsg{name: "f", dur: 100})
	_, _, showBar, _, _ := m.layout()
	if !showBar {
		t.Fatal("precondition: bar visible")
	}
	click := tea.MouseMsg(tea.MouseEvent{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 50, Y: 3})
	m.Update(click)
	// G1: the click moves the clicker's own player AND tells the room
	if len(pl.seeks) != 1 {
		t.Fatalf("bar click produced no local seek: %v", pl.seeks)
	}
	if len(posted) != 1 || posted[0].Type != "seek" {
		t.Fatalf("bar click did not tell the room: %v", posted)
	}
	miss := tea.MouseMsg(tea.MouseEvent{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 50, Y: 8})
	m.Update(miss)
	if len(pl.seeks) != 1 || len(posted) != 1 {
		t.Fatal("click off the bar row must not seek")
	}
}

func TestDashboardRenders(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	m = m.logChat("bo", "selam")
	m = m.syncViewport()
	v := m.View()
	for _, want := range []string{"NOW PLAYING", "WATCHERS", "QUEUE", "CHAT", "MESSAGE", "ctrl+space", "ada (you)"} {
		if !strings.Contains(v, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	if strings.Contains(v, "invite them") {
		t.Fatal("invite hint must not render once chat exists")
	}
}

func TestDashboardFallbackBelow100Cols(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 99, Height: 30})
	if strings.Contains(m.View(), "NOW PLAYING") {
		t.Fatal("dashboard must not render below 100 columns")
	}
}

func TestWatchersOfflineTag(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m = upd(t, m, room.Event{Type: "join", Nick: "bo"})
	m = upd(t, m, room.Event{Type: "bye", Nick: "bo"})
	lines := strings.Join(m.watcherLines(), "\n")
	if !strings.Contains(lines, "bo (offline)") {
		t.Fatalf("offline tag missing: %q", lines)
	}
	m = upd(t, m, room.Event{Type: "join", Nick: "bo"})
	if strings.Contains(strings.Join(m.watcherLines(), "\n"), "offline") {
		t.Fatal("returning nick keeps its offline tag")
	}
}

// --- Gemini round (C-B..C-G) -----------------------------------------------

// C-B: the equalizer must fill its box. The idle flatline (0.04) rounded
// down to zero blocks, so the whole panel rendered empty.
func TestVibePanelNeverRendersEmpty(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = m.stepEq() // idle: no state at all
	lines := m.vibePanel(40)
	if len(lines) != 8 {
		t.Fatalf("vibe panel rows = %d, want 8", len(lines))
	}
	blocks := false
	for _, l := range lines {
		if lipgloss.Width(l) > 40 {
			t.Fatalf("panel row too wide (%d cols): %q", lipgloss.Width(l), l)
		}
		if strings.Contains(l, "██") {
			blocks = true
		}
	}
	if !blocks {
		t.Fatalf("the idle equalizer renders no blocks at all: %q", lines)
	}
}

// C-B: the equalizer box must use its full inner width (the panel used
// to be rendered two columns narrower, leaving a gap on the right).
func TestEqualizerFillsItsBoxWidth(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	_, rightW := m.dashGeom()
	inner := rightW - 4 // the box's content width
	lines := m.vibePanel(inner)
	widest := 0
	for _, l := range lines {
		w := lipgloss.Width(l)
		if w > inner {
			t.Fatalf("panel row too wide (%d > %d): %q", w, inner, l)
		}
		if w > widest {
			widest = w
		}
	}
	if widest < inner-2 {
		t.Fatalf("the equalizer leaves %d empty columns in its box: %q", inner-widest, lines)
	}
}

// C-C: the hotbar points at the file picker; the tab key keeps working.
func TestHotbarShowsCtrlPFiles(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	v := m.View()
	if !strings.Contains(v, "[ctrl+p]") || !strings.Contains(v, "files") {
		t.Fatalf("hotbar missing the picker hint:\n%s", v)
	}
	if strings.Contains(v, "[tab] commands") {
		t.Fatalf("the hotbar still advertises the tab menu:\n%s", v)
	}
	m2 := New("t", nil, nil, false, nil)
	m2.input.SetValue("/pl")
	m2 = upd(t, m2, tea.KeyMsg{Type: tea.KeyTab})
	if m2.input.Value() != "/play " {
		t.Fatalf("tab completion broken: %q", m2.input.Value())
	}
}

// C-D: /skip on an empty queue says so instead of silently doing nothing.
func TestSkipOnEmptyQueueReports(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m.input.SetValue("/skip")
	mm, c := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(Model)
	if c != nil {
		t.Fatal("skip on an empty queue must not post")
	}
	if !strings.Contains(m.lastErr, "queue is empty") {
		t.Fatalf("no feedback for an empty queue: %q", m.lastErr)
	}
	if !strings.Contains(m.View(), "queue is empty") {
		t.Fatalf("the feedback must be visible: %q", m.View())
	}
	// a queued file makes the skip real again
	m = upd(t, m, room.Event{Type: "queue", Queue: []string{"a.mkv"}})
	m.input.SetValue("/skip")
	mm, c = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if c == nil {
		t.Fatal("skip with a queued file must post")
	}
}

// C-E: a short conversation sits at the bottom of the viewport, next to
// the input row — and follow-bottom still wins when the chat is long.
func TestSparseChatSitsAtTheBottom(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 90, Height: 30})
	m = upd(t, m, room.Event{Type: "chat", Nick: "bo", Text: "selam"})
	lines := strings.Split(m.vp.View(), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, "selam") {
		t.Fatalf("the message must be the last viewport row: %q", last)
	}
	for i := 0; i < 40; i++ {
		m = upd(t, m, room.Event{Type: "chat", Nick: "bo", Text: fmt.Sprintf("line-%02d", i)})
	}
	lines = strings.Split(m.vp.View(), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, "line-39") {
		t.Fatalf("follow-bottom broken: %q", last)
	}
}

// B1: the completion menu borrows rows from the chat viewport. The
// padding that keeps a sparse chat at the bottom must follow the
// SHRUNK viewport height — otherwise the message is pushed out of the
// visible window and the chat looks empty while the menu is open.
func TestMenuOpenKeepsChatVisible(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 90, Height: 30})
	m = upd(t, m, room.Event{Type: "chat", Nick: "bo", Text: "selam"})
	m.input.SetValue("/p")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyTab}) // ambiguous: the menu opens
	if !m.compOpen {
		t.Fatal("precondition: the completion menu must be open")
	}
	_, vh, _, _, _ := m.layout()
	if m.vp.Height != vh {
		t.Fatalf("the chat viewport height is stale: model=%d layout=%d", m.vp.Height, vh)
	}
	lines := strings.Split(m.vp.View(), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, "selam") {
		t.Fatalf("the message must be the last viewport row: %q", last)
	}
	if !strings.Contains(m.View(), "selam") {
		t.Fatalf("the message must be visible with the menu open:\n%s", m.View())
	}
	// a long chat still follows the bottom while the menu is open
	for i := 0; i < 40; i++ {
		m = upd(t, m, room.Event{Type: "chat", Nick: "bo", Text: fmt.Sprintf("line-%02d", i)})
	}
	lines = strings.Split(m.vp.View(), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, "line-39") {
		t.Fatalf("follow-bottom broken with the menu open: %q", last)
	}
}

// B4: the menu budget must come from the REAL layout. The stacked
// layout has no equalizer, so its chat is far taller than the
// dashboard formula assumed — the budget collapsed to one row and the
// candidates were never visible.
func TestStackedMenuShowsCandidates(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 90, Height: 30})
	m.input.SetValue("/p")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if !m.compOpen {
		t.Fatal("precondition: the completion menu must be open")
	}
	lines := m.menuLines(m.contentWidth())
	if len(lines) < 2 {
		t.Fatalf("the menu shows %d rows, the candidates are hidden: %q", len(lines), lines)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"/pick", "/play"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the menu misses %q: %q", want, joined)
		}
	}
}

// B4 regression guard: the dashboard keeps today's budget — there the
// chat really is 5 rows tall and the 4-row floor wins.
func TestDashboardMenuBudgetUnchanged(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	m.input.SetValue("/p")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if !m.compOpen {
		t.Fatal("precondition: the completion menu must be open")
	}
	if lines := m.menuLines(m.contentWidth()); len(lines) != 1 {
		t.Fatalf("the dashboard menu budget changed: %d rows", len(lines))
	}
}

// C-F: a remote state change names its actor, and the icon/collapse keys
// still work (the first token is unchanged).
func TestSystemLineNamesTheActor(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m = upd(t, m, room.Event{Type: "pause", Nick: "bo"})
	sys := strings.Join(m.renderSysLines(), "\n")
	if !strings.Contains(sys, "pause by bo") {
		t.Fatalf("the actor is missing: %q", sys)
	}
	if !strings.Contains(sys, "⏸") {
		t.Fatalf("the pause icon is gone: %q", sys)
	}
	m = upd(t, m, room.Event{Type: "pause", Nick: "ada"}) // our own action
	sys = strings.Join(m.renderSysLines(), "\n")
	if strings.Count(sys, "pause") != 2 {
		t.Fatalf("two different actors collapsed into one line: %q", sys)
	}
}

// C-G: the server's hello carries the address to invite people to.
func TestInviteFromHello(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m = upd(t, m, room.Event{Type: "hello", Invite: "http://100.64.1.2:8090"})
	m = m.syncViewport()
	if !strings.Contains(m.vp.View(), "100.64.1.2:8090") {
		t.Fatalf("the invite address is missing: %q", m.vp.View())
	}
}

// --- v2 polish round (T1..T6, K1, K2) ---------------------------------------

// T1/FB1: the queue box says how many files it holds and how to skip —
// the hint rides in the TITLE, so it costs no row.
func TestQueueBoxShowsCountAndSkipHint(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	if !strings.Contains(m.View(), "QUEUE (0)") {
		t.Fatalf("the empty queue must show its count:\n%s", m.View())
	}
	if strings.Contains(m.View(), "/skip next") {
		t.Fatalf("an empty queue must not advertise a skip:\n%s", m.View())
	}
	m = upd(t, m, room.Event{Type: "queue", Queue: []string{"a.mkv", "b.mkv"}})
	v := m.View()
	if !strings.Contains(v, "QUEUE (2) · /skip next") {
		t.Fatalf("the queue count or the skip hint missing:\n%s", v)
	}
}

// FB1: a non-empty queue ALWAYS shows at least one file name — even at
// the 4-row floor, where the overflow hint takes the second row.
func TestQueueAlwaysShowsAFileName(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	q := make([]string, 25)
	for i := range q {
		q[i] = fmt.Sprintf("video-%02d.mkv", i)
	}
	m = upd(t, m, room.Event{Type: "queue", Queue: q})
	v := m.View()
	if !strings.Contains(v, "1. video-00.mkv") {
		t.Fatalf("the queue box shows no file name:\n%s", v)
	}
	if !strings.Contains(v, "+24 more") {
		t.Fatalf("the overflow hint missing:\n%s", v)
	}
}

// T3: alone in the room, the watcher panel says so.
func TestWaitingForFollowerHint(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	if !strings.Contains(m.View(), "waiting for follower") {
		t.Fatalf("the waiting hint missing:\n%s", m.View())
	}
	m = upd(t, m, room.Event{Type: "join", Nick: "bo"})
	if strings.Contains(m.View(), "waiting for follower") {
		t.Fatal("the waiting hint must vanish when someone is here")
	}
}

// H8: the hotbar verb is the ACTION the key does now: pause while the
// room plays, play while it is paused.
func TestHotbarVerbIsContextual(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	if !strings.Contains(m.View(), "[ctrl+space] pause") {
		t.Fatalf("the hotbar must offer pause by default:\n%s", m.View())
	}
	pos := 10.0
	m = upd(t, m, sync.UiState{Pos: &pos, Paused: true})
	v := m.View()
	if !strings.Contains(v, "[ctrl+space] play") {
		t.Fatalf("the paused hotbar must offer play:\n%s", v)
	}
}

// H3: the band names the seek keys in its right segment (and says
// PAUSED while the room is paused).
func TestBandTitleNamesSeekKeys(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	v := m.View()
	if !strings.Contains(v, "NOW PLAYING") {
		t.Fatalf("the band title missing:\n%s", v)
	}
	if strings.Contains(v, "click bar") {
		t.Fatalf("no bar, no click hint:\n%s", v)
	}
	pos := 10.0
	m = upd(t, m, sync.UiState{Pos: &pos})
	m.now = "vid.mp4"
	m = upd(t, m, durationMsg{name: "vid.mp4", dur: 100})
	if !strings.Contains(m.View(), "click bar or ctrl+←/→ to seek") {
		t.Fatalf("the band misses the seek hint:\n%s", m.View())
	}
	m = upd(t, m, sync.UiState{Pos: &pos, Paused: true})
	if !strings.Contains(m.View(), "NOW PLAYING · PAUSED") {
		t.Fatalf("the paused band must say so:\n%s", m.View())
	}
}

// H2: the equalizer names the real state only — and the layout is only
// named when mpv told us.
func TestEqualizerLabel(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	pos := 10.0
	m = upd(t, m, sync.UiState{Pos: &pos, Volume: 80})
	if !strings.Contains(m.View(), "♪ vol 80%") {
		t.Fatalf("the volume label missing:\n%s", m.View())
	}
	if strings.Contains(m.View(), "stereo") {
		t.Fatal("an unknown layout must not be claimed")
	}
	m = upd(t, m, sync.UiState{Pos: &pos, Volume: 80, Layout: "stereo"})
	if !strings.Contains(m.View(), "♪ vol 80% · stereo") {
		t.Fatalf("the layout label missing:\n%s", m.View())
	}
	m = upd(t, m, sync.UiState{Pos: &pos, Paused: true})
	if !strings.Contains(m.View(), "⏸ paused") {
		t.Fatalf("the paused label missing:\n%s", m.View())
	}
	// idle: no state, no claim
	m2 := New("t", nil, nil, false, nil)
	m2 = upd(t, m2, tea.WindowSizeMsg{Width: 120, Height: 30})
	if strings.Contains(m2.View(), "vol") || strings.Contains(m2.View(), "⏸") {
		t.Fatalf("the idle equalizer must not claim a state:\n%s", m2.View())
	}
}

// K1: zen fades the old messages; the normal layouts never do.
func TestZenDimsOldMessages(t *testing.T) {
	// tests run without a TTY: force a color profile so lipgloss emits
	// codes (truecolor keeps the 241 dim apart from the 243 timestamp)
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(orig)

	mark := dim.Render("X")
	code := mark[:strings.Index(mark, "X")]
	if code == "" {
		t.Fatal("the color profile must emit codes")
	}

	log := []entry{
		{kind: entChat, nick: "bo", text: "eski", at: time.Now().Add(-30 * time.Minute)},
		{kind: entChat, nick: "bo", text: "yeni", at: time.Now()},
	}
	m := New("t", nil, nil, false, nil)
	m.zen = true
	m.log = log
	lines := m.renderChatLines()
	if len(lines) != 2 {
		t.Fatalf("chat lines: %q", lines)
	}
	if !strings.Contains(lines[0], code) {
		t.Fatalf("the old message must be dim in zen: %q", lines[0])
	}
	if strings.Contains(lines[1], code) {
		t.Fatalf("the fresh message must stay bright in zen: %q", lines[1])
	}
	// the normal layout never dims
	m2 := New("t", nil, nil, false, nil)
	m2.log = log
	for _, l := range m2.renderChatLines() {
		if strings.Contains(l, code) {
			t.Fatalf("the normal layout must not dim: %q", l)
		}
	}
}

// K2: two invite addresses are listed and labeled by their network; a
// single address stays one unlabeled line.
func TestDualInviteAddresses(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, room.Event{Type: "hello",
		Invite: "http://100.84.22.15:8090", InviteAlt: "http://192.168.1.181:8090"})
	m = m.syncViewport()
	v := m.vp.View()
	for _, want := range []string{"100.84.22.15:8090", "192.168.1.181:8090", "(tailscale)", "(local lan)"} {
		if !strings.Contains(v, want) {
			t.Fatalf("the placeholder misses %q: %q", want, v)
		}
	}
	m2 := New("t", nil, nil, false, nil)
	m2 = upd(t, m2, room.Event{Type: "hello", Invite: "http://100.84.22.15:8090"})
	m2 = m2.syncViewport()
	if v2 := m2.vp.View(); !strings.Contains(v2, "100.84.22.15:8090") || strings.Contains(v2, "(tailscale)") {
		t.Fatalf("a single address must stay one unlabeled line: %q", v2)
	}
}

// F2: a continuation inherits the group's head state — a bright head
// with an older tail must not fade (the tail can be older than the
// head, that quirk is pre-existing and stays).
func TestZenContinuationInheritsHeadState(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(orig)

	mark := dim.Render("X")
	code := mark[:strings.Index(mark, "X")]

	// head: 14 minutes old (bright), tail: 16 minutes old (dim on its own)
	log := []entry{
		{kind: entChat, nick: "bo", text: "head", at: time.Now().Add(-14 * time.Minute)},
		{kind: entChat, nick: "bo", text: "tail", at: time.Now().Add(-16 * time.Minute)},
	}
	m := New("t", nil, nil, false, nil)
	m.zen = true
	m.log = log
	lines := m.renderChatLines()
	if len(lines) != 2 {
		t.Fatalf("chat lines: %q", lines)
	}
	if strings.Contains(lines[0], code) {
		t.Fatalf("the 14-minute head must stay bright: %q", lines[0])
	}
	if strings.Contains(lines[1], code) {
		t.Fatalf("the continuation must inherit the bright head: %q", lines[1])
	}

	// the other way round: a dim head drags its tail down, even when the
	// tail would be bright on its own (head -19.5min, tail -14.6min:
	// same group, the tail inside the 15-minute window alone)
	log2 := []entry{
		{kind: entChat, nick: "bo", text: "head", at: time.Now().Add(-19500 * time.Second)},
		{kind: entChat, nick: "bo", text: "tail", at: time.Now().Add(-14600 * time.Second)},
	}
	m.log = log2
	lines = m.renderChatLines()
	if !strings.Contains(lines[0], code) || !strings.Contains(lines[1], code) {
		t.Fatalf("the continuation must inherit the dim head: %q", lines)
	}
}

// --- v2b round (H1..H8) ---------------------------------------------------

// H3: the dashboard bar is clickable — a click in the middle of it seeks
// the room to the middle of the film.
func TestDashboardBarClickSeeks(t *testing.T) {
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

	api := room.New(srv.URL, "sen-1")
	pl := &recPlayer{}
	eng := &sync.Engine{
		Master: true, ID: "sen-1", Player: pl, Clock: sync.RealClock{},
		Post: func(ev room.Event) error { return api.Post(ev) },
	}
	m := New("sen", api, nil, true, eng)
	m = upd(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	pos := 0.0
	m = upd(t, m, sync.UiState{Pos: &pos})
	m.now = "vid.mp4"
	m = upd(t, m, durationMsg{name: "vid.mp4", dur: 100})

	// the bar row of the band: header, gap, border, name, blank, bar
	click := tea.MouseMsg(tea.MouseEvent{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 59, Y: 5})
	m.Update(click)
	// G1: the clicker's own player moves too (the server never echoes a
	// poster's own event back)
	if len(pl.seeks) != 1 {
		t.Fatalf("the dashboard bar click did not seek the local player: %v", pl.seeks)
	}
	select {
	case ev := <-posted:
		if ev.Type != "seek" {
			t.Fatalf("posted %+v, want a seek", ev)
		}
		if ev.T < 40 || ev.T > 60 {
			t.Fatalf("seek target %.1fs, want about the middle of 100s", ev.T)
		}
	default:
		t.Fatal("no seek was posted")
	}
	// a click outside the bar row must not seek
	m.Update(tea.MouseMsg(tea.MouseEvent{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 59, Y: 4}))
	if len(pl.seeks) != 1 {
		t.Fatal("a click off the bar row must not seek")
	}
	select {
	case ev := <-posted:
		t.Fatalf("a click off the bar row posted %+v", ev)
	default:
	}
}

// H4: the stat row names who paused the room — and stays bare when the
// pause is ours.
func TestStatRowNamesThePauseActor(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	pos := 10.0
	m = upd(t, m, sync.UiState{Pos: &pos, Paused: true, PausedBy: "bo"})
	row := strings.Join(m.nowPlayingLines(100), "\n")
	if !strings.Contains(row, "⏸ paused by bo") {
		t.Fatalf("the actor chip missing: %q", row)
	}
	m = upd(t, m, sync.UiState{Pos: &pos, Paused: true})
	row = strings.Join(m.nowPlayingLines(100), "\n")
	if !strings.Contains(row, "⏸ paused") || strings.Contains(row, "by ") {
		t.Fatalf("our own pause must stay bare: %q", row)
	}
}

// H5: the subtitle chip is PERSONAL — it shows the local track and is
// never synced or posted.
func TestSubtitleChipIsPersonal(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	pos := 10.0
	m = upd(t, m, sync.UiState{Pos: &pos, Sub: "tr"})
	row := strings.Join(m.nowPlayingLines(100), "\n")
	if !strings.Contains(row, "⊞ you: sub tr [j in mpv]") {
		t.Fatalf("the subtitle chip missing: %q", row)
	}
	chip := m.zenSyncChip()
	if !strings.Contains(chip, "⊞ tr") {
		t.Fatalf("the zen chip misses the subtitle: %q", chip)
	}
	// no subtitle, no chip anywhere
	m2 := New("t", nil, nil, false, nil)
	m2 = upd(t, m2, sync.UiState{Pos: &pos})
	if strings.Contains(strings.Join(m2.nowPlayingLines(100), "\n"), "sub") {
		t.Fatal("the subtitle chip must not appear without a subtitle")
	}
	if strings.Contains(m2.zenSyncChip(), "⊞") {
		t.Fatal("the zen chip must not appear without a subtitle")
	}
}

// H6: the system tail grows to 30 lines on a tall terminal.
func TestSystemTailHasThirtyLines(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 120, Height: 50})
	for i := 0; i < 40; i++ {
		m = upd(t, m, room.Event{Type: "chat", Nick: "bo", Text: fmt.Sprintf("line-%02d", i)})
		m = m.logEngine(fmt.Sprintf("> tick %02d", i))
	}
	v := m.View()
	if !strings.Contains(v, "30-line tail") {
		t.Fatalf("the system tail must show 30 lines:\n%s", v)
	}
	if n := len(strings.Split(v, "\n")); n > 50 {
		t.Fatalf("the dashboard overflows: %d rows", n)
	}
}

// H8: all six hotbar items fit in the narrowest dashboard (100 columns).
func TestHotbarFitsNarrowDashboard(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 100, Height: 20})
	v := m.View()
	for _, want := range []string{"[tab]", "[ctrl+g]", "[ctrl+space]", "[ctrl+p]"} {
		if !strings.Contains(v, want) {
			t.Fatalf("the hotbar misses %q:\n%s", want, v)
		}
	}
	for i, l := range strings.Split(v, "\n") {
		if w := lipgloss.Width(l); w > 100 {
			t.Fatalf("line %d is %d cols (> 100): %q", i, w, l)
		}
	}
}

// H8: an empty input + Tab opens the command menu (the verbs must be
// discoverable without the slash).
func TestEmptyInputTabOpensCommandMenu(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 90, Height: 30})
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if !m.compOpen {
		t.Fatal("Tab on an empty input must open the menu")
	}
	joined := strings.Join(m.menuLines(m.contentWidth()), "\n")
	if !strings.Contains(joined, "/play") {
		t.Fatalf("the menu misses the verbs: %q", joined)
	}
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.HasPrefix(m.input.Value(), "/") {
		t.Fatalf("enter must fill the input: %q", m.input.Value())
	}
}

// H2b: while paused the equalizer FREEZES (no decay) and wears the
// pause color; while playing it is green.
func TestPausedEqualizerFreezes(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(orig)

	m := New("t", nil, nil, false, nil)
	pos := 10.0
	m = upd(t, m, sync.UiState{Pos: &pos, Paused: true})
	m = m.stepEq()
	before := append([]float64{}, m.eq...)
	m = m.stepEq()
	for i := range before {
		if m.eq[i] != before[i] {
			t.Fatalf("the paused bars must freeze: %v -> %v", before, m.eq)
		}
	}
	panel := strings.Join(m.vibePanel(40), "\n")
	if !strings.Contains(panel, "\x1b[33m") { // the amber pause color
		t.Fatalf("the paused bars must wear the pause color: %q", panel)
	}
	// playing: green
	m2 := New("t", nil, nil, false, nil)
	m2 = upd(t, m2, sync.UiState{Pos: &pos})
	m2 = m2.stepEq()
	panel = strings.Join(m2.vibePanel(40), "\n")
	if !strings.Contains(panel, "\x1b[32m") {
		t.Fatalf("the playing bars must be green: %q", panel)
	}
}

// FB5: a right label that does not fit is dropped — the border must
// never grow past the box.
func TestBoxRightLabelNeverOverflows(t *testing.T) {
	lines := box(20, "T", "a very long right label", nil, lipgloss.NewStyle())
	if w := lipgloss.Width(lines[0]); w != 20 {
		t.Fatalf("the top border is %d cols, want 20: %q", w, lines[0])
	}
	// a label that fits is kept
	lines = box(40, "T", "right", nil, lipgloss.NewStyle())
	if !strings.Contains(lines[0], "right") {
		t.Fatalf("the right label was dropped although it fits: %q", lines[0])
	}
	if w := lipgloss.Width(lines[0]); w != 40 {
		t.Fatalf("the top border is %d cols, want 40: %q", w, lines[0])
	}
}

// H9: the equalizer border follows the state, exactly like the band's —
// the paused amber is the same one the bars wear.
func TestEqualizerBorderFollowsTheState(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(orig)

	m := New("t", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	pos := 10.0

	// idle: nothing loaded → the muted idle grey
	if line := eqBorderLine(t, m); !strings.Contains(line, "38;5;241m") {
		t.Fatalf("the idle equalizer border must be muted: %q", line)
	}
	// playing → the vibe green
	m = upd(t, m, sync.UiState{Pos: &pos})
	if line := eqBorderLine(t, m); !strings.Contains(line, "38;5;84m") {
		t.Fatalf("the playing equalizer border must be the vibe color: %q", line)
	}
	// paused → the pause amber (the bars wear the same one)
	m = upd(t, m, sync.UiState{Pos: &pos, Paused: true})
	if line := eqBorderLine(t, m); !strings.Contains(line, "\x1b[33m") {
		t.Fatalf("the paused equalizer border must be the pause color: %q", line)
	}
}

// eqBorderLine returns the top border of the equalizer box.
func eqBorderLine(t *testing.T, m Model) string {
	t.Helper()
	for _, l := range strings.Split(m.View(), "\n") {
		if strings.Contains(l, "EQUALIZER") {
			return l
		}
	}
	t.Fatal("the equalizer box is missing from the view")
	return ""
}

// H10: the stat row always names the state — "▶ playing" while the room
// plays, "⏸ paused" (with the actor) while it does not.
func TestStatRowShowsThePlayingChip(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	pos := 10.0
	m = upd(t, m, sync.UiState{Pos: &pos})
	row := strings.Join(m.nowPlayingLines(100), "\n")
	if !strings.Contains(row, "▶ playing") {
		t.Fatalf("the playing chip missing: %q", row)
	}
	if strings.Contains(row, "paused") {
		t.Fatalf("a playing room must not show a paused chip: %q", row)
	}
	m = upd(t, m, sync.UiState{Pos: &pos, Paused: true})
	row = strings.Join(m.nowPlayingLines(100), "\n")
	if !strings.Contains(row, "⏸ paused") {
		t.Fatalf("the paused chip missing: %q", row)
	}
	if strings.Contains(row, "▶ playing") {
		t.Fatalf("a paused room must not show a playing chip: %q", row)
	}
}

// --- H12: the menu ceiling -----------------------------------------------

// H12: an empty input + Tab shows the WHOLE command menu — with a
// ceiling of 16 rows the eight verbs fit without a "+N more".
func TestEmptyInputTabShowsEveryCommand(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if !m.compOpen {
		t.Fatal("the empty input + Tab must open the command menu")
	}
	v := m.View()
	for _, c := range verbs {
		if !strings.Contains(v, "/"+c) {
			t.Fatalf("/%s missing from the menu:\n%s", c, v)
		}
	}
}

// H12: the file menu grows to 16 rows and says how many candidates are
// hidden below.
func TestFileMenuGrowsToSixteenRows(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 120, Height: 60})
	files := make([]string, 20)
	for i := range files {
		files[i] = fmt.Sprintf("movie-%02d.mkv", i)
	}
	m = upd(t, m, filesMsg{files: files})
	m.input.SetValue("/play ")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if !m.compOpen {
		t.Fatal("the file menu must open")
	}
	rows := m.menuLines(100)
	if len(rows) != 16 {
		t.Fatalf("the menu has %d rows, want 16", len(rows))
	}
	if !strings.Contains(rows[len(rows)-1], "more") {
		t.Fatalf("the menu must say how many candidates are hidden: %q", rows[len(rows)-1])
	}
}

// H12 INVARIANT: an open menu never pushes the dashboard past the
// terminal — the chat gives up its rows and the menu shrinks into the
// floor.
func TestOpenMenuKeepsTheDashboardInsideTheTerminal(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	files := make([]string, 20)
	for i := range files {
		files[i] = fmt.Sprintf("movie-%02d.mkv", i)
	}
	m = upd(t, m, filesMsg{files: files})
	m.input.SetValue("/play ")
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if !m.compOpen {
		t.Fatal("the file menu must open")
	}
	if n := len(strings.Split(m.View(), "\n")); n > 40 {
		t.Fatalf("the dashboard is %d lines tall, want at most 40", n)
	}
}
