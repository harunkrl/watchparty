package ui

import (
	"fmt"
	"strings"
	"testing"

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
	for _, l := range lines {
		if !strings.Contains(l, ":") { // HH:MM gutter on every line
			t.Fatalf("no timestamp gutter: %q", l)
		}
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
	if got := m.renderEntry(entry{kind: entEvent, text: "> bo joined"}, 60, false)[0]; got == dim.Render("· bo joined") {
		t.Fatalf("join line still renders dim: %q", got)
	}
	if got := m.renderEntry(entry{kind: entEvent, text: "> bo left"}, 60, false)[0]; got == dim.Render("· bo left") {
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
	m := New("ada", nil, nil, false, nil)
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
	mm, cmd := m.Update(click)
	if cmd == nil {
		t.Fatal("bar click produced no seek command")
	}
	_ = mm
	miss := tea.MouseMsg(tea.MouseEvent{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 50, Y: 8})
	if _, cmd2 := m.Update(miss); cmd2 != nil {
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
