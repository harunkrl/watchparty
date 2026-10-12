// Package ui renders the Bubble Tea control surface: status bar (role,
// drift/listeners, connection state, queue), room log with scroll and
// input history, cursor picker, framed with lipgloss.
package ui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"watchparty/client-go/internal/cmd"
	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
	"watchparty/client-go/internal/sync"
)

const (
	logCap  = 500
	histCap = 100
)

type mode int

const (
	modeInput mode = iota
	modePick
)

var (
	frame = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	dim   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	title = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))

	roleStyle = map[string]lipgloss.Style{
		"master":   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3")),
		"follower": lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")),
	}
	connStyle = map[string]lipgloss.Style{
		"connected":    lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		"reconnecting": lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
		"lost":         lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
		"connecting":   dim,
	}
	nickColors = []lipgloss.Color{"205", "45", "214", "84", "117", "212"}
	pickCur    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
)

func nickStyle(nick string) lipgloss.Style {
	h := 0
	for _, r := range nick {
		h += int(r)
	}
	return lipgloss.NewStyle().Bold(true).Foreground(nickColors[h%len(nickColors)])
}

type Model struct {
	nick       string
	master     bool
	api        *room.Client
	player     *mpv.Player
	input      textinput.Model
	log        []string
	peers      map[string]bool
	now        string
	queue      []string      // server queue state (hello/queue events)
	files      []string      // last known video list (typo guard, any filesMsg)
	st         *sync.UiState // live playback state for the status bar
	conn       string        // connecting | connected | reconnecting | lost
	mode       mode
	pickItems  []string
	pickCursor int
	hist       []string // input history, newest last
	histIdx    int      // -1 = live editing
	draft      string   // saved while browsing history
	scroll     int      // lines above the bottom edge (0 = follow)
	width      int
	height     int
}

func New(nick string, api *room.Client, player *mpv.Player, master bool) Model {
	in := textinput.New()
	in.Prompt = "> "
	in.Placeholder = "chat <msg> | pick | list | help"
	in.Focus()
	// sane defaults so the first frame renders before WindowSizeMsg arrives
	// (and in size-less ptys used by the test harness)
	return Model{
		nick: nick, master: master, api: api, player: player,
		input: in, peers: map[string]bool{}, conn: "connecting",
		histIdx: -1, width: 80, height: 24,
	}
}

func (m Model) Init() tea.Cmd {
	return textinput.Blink
}

type StreamClosedMsg struct{}
type MpvExitedMsg struct{}
type ReconnectingMsg struct{ Note string }
type EngineLogMsg struct{ Line string } // engine log line (stderr writes would corrupt the frame)
type errMsg struct{ err error }
type filesMsg struct {
	files []string
	err   error
}

func postEvent(api *room.Client, ev room.Event) tea.Cmd {
	return func() tea.Msg {
		if err := api.Post(ev); err != nil {
			return errMsg{err}
		}
		return nil
	}
}

// playFile mirrors client.py's play: load locally first, then tell the room.
func playFile(pl *mpv.Player, api *room.Client, name string) tea.Cmd {
	return func() tea.Msg {
		if err := pl.Load(name); err != nil {
			return errMsg{err}
		}
		if err := api.Post(room.Event{Type: "load", Name: name}); err != nil {
			return errMsg{err}
		}
		return nil
	}
}

func fetchFiles(api *room.Client) tea.Cmd {
	return func() tea.Msg {
		files, err := api.Files()
		return filesMsg{files, err}
	}
}

func (m Model) logf(format string, args ...any) Model {
	return m.logLine(fmt.Sprintf(format, args...))
}

func (m Model) logLine(line string) Model {
	m.log = append(m.log, line)
	if len(m.log) > logCap {
		m.log = m.log[len(m.log)-logCap:]
	}
	return m
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.Width = m.contentWidth() - 4
	case tea.KeyMsg:
		if m.mode == modePick {
			return m.pickKeys(msg)
		}
		return m.inputKeys(msg)
	case room.Event:
		return m.onEvent(msg), nil
	case sync.UiState:
		st := msg
		m.st = &st
	case filesMsg:
		if msg.err != nil {
			return m.logf("> err: %s", msg.err), nil
		}
		m.files = msg.files // cache even outside pick mode (typo guard)
		if m.mode == modePick {
			m.pickItems = msg.files
			if m.pickCursor >= len(m.pickItems) {
				m.pickCursor = max(0, len(m.pickItems)-1)
			}
			return m, nil
		}
		for _, f := range msg.files {
			m = m.logf("  %s", f)
		}
	case errMsg:
		return m.logf("> err: %s", msg.err), nil
	case ReconnectingMsg:
		m.conn = "reconnecting"
		return m.logLine(msg.Note), nil
	case EngineLogMsg:
		return m.logLine(msg.Line), nil
	case StreamClosedMsg:
		m.conn = "lost"
		return m.logLine("> connection closed"), nil
	case MpvExitedMsg:
		return m.logLine("> mpv exited (window closed?) - restart the client"), tea.Quit
	}
	return m, nil
}

// inputKeys handles typing plus history browsing and log scrolling.
func (m Model) inputKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		return m.submit()
	case "esc":
		m.input.SetValue("")
		m.scroll = 0
	case "ctrl+c":
		return m, tea.Quit
	case "up":
		if len(m.hist) > 0 {
			if m.histIdx == -1 {
				m.draft, m.histIdx = m.input.Value(), len(m.hist)-1
			} else if m.histIdx > 0 {
				m.histIdx--
			}
			m.input.SetValue(m.hist[m.histIdx])
		}
	case "down":
		if m.histIdx != -1 {
			if m.histIdx < len(m.hist)-1 {
				m.histIdx++
				m.input.SetValue(m.hist[m.histIdx])
			} else {
				m.histIdx = -1
				m.input.SetValue(m.draft)
			}
		}
	case "pgup":
		m.scroll = min(m.scroll+10, max(0, len(m.log)-1))
	case "pgdown":
		m.scroll = max(0, m.scroll-10)
	default:
		var c tea.Cmd
		m.input, c = m.input.Update(msg)
		return m, c
	}
	return m, nil
}

// pickKeys handles the cursor list: arrows/j/k move, enter plays, esc backs.
func (m Model) pickKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		m.pickCursor = max(0, m.pickCursor-1)
	case "down", "j":
		if m.pickCursor < len(m.pickItems)-1 {
			m.pickCursor++
		}
	case "enter":
		if m.pickCursor < len(m.pickItems) {
			name := m.pickItems[m.pickCursor]
			m.mode = modeInput
			m = m.logf("> playing %s", name)
			return m, playFile(m.player, m.api, name)
		}
	case "q":
		if m.pickCursor < len(m.pickItems) {
			m.mode = modeInput
			return m.enqueue(m.pickItems[m.pickCursor])
		}
	case "esc":
		m.mode = modeInput
	}
	return m, nil
}

// enqueue mirrors the server's enqueue conditions for instant feedback;
// on success its queue broadcast confirms (it reaches us too).
func (m Model) enqueue(name string) (Model, tea.Cmd) {
	if name == m.now {
		return m.logf("> %s is already playing", name), nil
	}
	for _, q := range m.queue {
		if q == name {
			return m.logf("> %s is already queued", name), nil
		}
	}
	if !m.knownFile(name) {
		return m.logf("> no such file: %s (list to refresh)", name), nil
	}
	m = m.logf("> queued %s", name)
	return m, postEvent(m.api, room.Event{Type: "enqueue", Name: name})
}

// submit parses the input line and dispatches it.
func (m Model) submit() (tea.Model, tea.Cmd) {
	line := strings.TrimSpace(m.input.Value())
	m.input.SetValue("")
	m.histIdx, m.draft = -1, ""
	if line == "" {
		return m, nil
	}
	m.hist = append(m.hist, line)
	if len(m.hist) > histCap {
		m.hist = m.hist[len(m.hist)-histCap:]
	}
	c, err := cmd.Parse(line)
	if err != nil {
		return m.logf("> %s", err), nil
	}
	switch c.Kind {
	case cmd.Quit:
		return m, tea.Quit
	case cmd.Help:
		return m.logf("> %s", cmd.Usage), nil
	case cmd.Chat:
		// local echo: the server never reflects our own chat back
		m = m.logLine(fmt.Sprintf("[%s] %s", nickStyle(m.nick).Render(m.nick), c.Arg))
		return m, postEvent(m.api, room.Event{Type: "chat", Nick: m.nick, Text: c.Arg})
	case cmd.List:
		return m, fetchFiles(m.api)
	case cmd.Pick:
		m.mode = modePick
		m.pickCursor = 0
		m.pickItems = nil
		return m, fetchFiles(m.api)
	case cmd.Play:
		if !m.knownFile(c.Arg) {
			return m.logf("> no such file: %s (list to refresh)", c.Arg), nil
		}
		m = m.logf("> playing %s", c.Arg) // local; own load broadcast is echo-filtered
		return m, playFile(m.player, m.api, c.Arg)
	case cmd.Queue:
		if c.Arg == "" { // bare queue: show the room queue
			if len(m.queue) == 0 {
				return m.logLine("> queue empty (queue <file> to add)"), nil
			}
			for i, n := range m.queue {
				m = m.logf("> %d. %s", i+1, n)
			}
			return m, nil
		}
		return m.enqueue(c.Arg)
	case cmd.Skip:
		return m, postEvent(m.api, room.Event{Type: "skip"})
	}
	return m, nil
}

func (m Model) onEvent(ev room.Event) Model {
	if ev.Type == "chat" {
		// colored nick, shared shape with RenderEvent (headless parity)
		m = m.logLine(fmt.Sprintf("[%s] %s", nickStyle(ev.Nick).Render(ev.Nick), ev.Text))
	} else if line := RenderEvent(ev); line != "" {
		m = m.logLine(line)
	}
	switch ev.Type {
	case "hello":
		m.now = ev.Now
		m.conn = "connected"
		m.queue = ev.Queue
	case "join":
		m.peers[ev.Nick] = true
	case "bye":
		delete(m.peers, ev.Nick)
	case "load":
		m.now = ev.Name
	case "queue":
		m.queue = ev.Queue
	}
	// position heartbeats are deliberately not logged (client.py parity)
	return m
}

// RenderEvent formats one room event as a log line shared by the TUI and
// the headless adapter (client.py log parity). Empty = not rendered.
func RenderEvent(ev room.Event) string {
	switch ev.Type {
	case "hello":
		return fmt.Sprintf("> room: now=%s queue=%s", orDash(ev.Now), queueStr(ev.Queue))
	case "join":
		return fmt.Sprintf("> %s joined", ev.Nick)
	case "bye":
		return fmt.Sprintf("> %s left", ev.Nick)
	case "chat":
		return fmt.Sprintf("[%s] %s", ev.Nick, ev.Text)
	case "queue":
		return fmt.Sprintf("> queue: %s", queueStr(ev.Queue))
	case "load":
		return fmt.Sprintf("> load %s", ev.Name)
	case "play", "pause":
		return "> " + ev.Type
	case "seek":
		return fmt.Sprintf("> seek @%.1fs", ev.T)
	}
	return ""
}

// contentWidth is the usable width inside the frame.
func (m Model) contentWidth() int {
	w := m.width - frame.GetHorizontalFrameSize()
	if w < 20 {
		w = 20
	}
	return w
}

func (m Model) View() string {
	if m.height == 0 {
		return "connecting…\n"
	}
	inner := m.height - frame.GetVerticalFrameSize()
	body := inner - 2 // status + input lines
	cw := m.contentWidth()

	var lines []string
	if m.mode == modePick {
		lines = m.pickView()
	} else {
		lines = m.log
	}
	// scroll: 0 pins to the newest line; >0 looks back into history
	lo, hi := 0, len(lines)
	if hi > body {
		lo = hi - body
	}
	if m.scroll > 0 {
		lo = max(0, lo-m.scroll)
		hi = min(len(lines), lo+body)
	}

	var b strings.Builder
	b.WriteString(m.statusLine())
	b.WriteByte('\n')
	for i := lo; i < lo+body; i++ {
		if i < hi {
			b.WriteString(truncate(lines[i], cw))
		}
		if i < lo+body-1 {
			b.WriteByte('\n')
		}
	}
	b.WriteByte('\n')
	b.WriteString(m.input.View())
	if m.scroll > 0 {
		b.WriteString(dim.Render(fmt.Sprintf("  (scrolled %d — pgdn/end)", m.scroll)))
	}
	return frame.Render(b.String())
}

func (m Model) pickView() []string {
	out := []string{title.Render("pick a film") + dim.Render("  (up/down/j/k move - enter play / q queue - esc cancel)")}
	if len(m.pickItems) == 0 {
		out = append(out, "(no files / loading...)")
	}
	for i, f := range m.pickItems {
		if i == m.pickCursor {
			out = append(out, pickCur.Render("› "+f))
		} else {
			out = append(out, "  "+f)
		}
	}
	return out
}

// knownFile guards typos when we have a cached list; with no cache yet
// the server stays the judge (it silently no-ops unknown enqueues).
func (m Model) knownFile(name string) bool {
	if len(m.files) == 0 {
		return true
	}
	for _, f := range m.files {
		if f == name {
			return true
		}
	}
	return false
}

func (m Model) statusLine() string {
	role := "follower"
	if m.master {
		role = "master"
	}
	mid := ""
	if m.master {
		mid = fmt.Sprintf("%d listener(s)", len(m.peers))
	} else if m.st != nil {
		mid = fmt.Sprintf("drift %.1fs", m.st.Drift)
	} else {
		mid = m.peerLine()
	}
	if m.st != nil && m.st.Pos != nil { // live play state + clock
		icon := "▶"
		if m.st.Paused {
			icon = "⏸"
		}
		mid = icon + " " + mmss(*m.st.Pos) + " · " + mid
	}
	queue := ""
	if len(m.queue) > 0 {
		queue = fmt.Sprintf(" · q:%d", len(m.queue))
	}
	return fmt.Sprintf("%s · %s · %s · %s",
		roleStyle[role].Render("watchparty "+role),
		"now: "+orDash(m.now),
		mid+queue,
		connStyle[m.conn].Render(m.conn))
}

func (m Model) peerLine() string {
	if len(m.peers) == 0 {
		return "just you"
	}
	names := make([]string, 0, len(m.peers))
	for n := range m.peers {
		names = append(names, n)
	}
	sort.Strings(names) // stable render, map order would flicker
	return strings.Join(names, ", ")
}

func mmss(sec float64) string {
	m, s := int(sec)/60, int(sec)%60
	return fmt.Sprintf("%d:%02d", m, s)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func queueStr(q []string) string {
	return "[" + strings.Join(q, ", ") + "]"
}

func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	// shortcut: styled (ANSI) lines are rare and short-lived; plain cut is
	// good enough and never crashes on escape codes
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return string(r[:w-1]) + "…"
}
