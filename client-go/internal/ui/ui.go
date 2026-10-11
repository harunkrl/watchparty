// Package ui renders the Bubble Tea control surface: status bar (role,
// drift/listeners, connection state), room log, cursor picker, input line.
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

const logCap = 500

type mode int

const (
	modeInput mode = iota
	modePick
)

var (
	statusStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	pickCurStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
)

type Model struct {
	nick       string
	master     bool
	api        *room.Client
	player     *mpv.Player
	input      textinput.Model
	log        []string
	peers      map[string]bool
	now        string
	drift      *float64 // last follower drift, nil until the first heartbeat
	conn       string   // connecting | connected | reconnecting | lost
	mode       mode
	pickItems  []string
	pickCursor int
	width      int
	height     int
}

func New(nick string, api *room.Client, player *mpv.Player, master bool) Model {
	in := textinput.New()
	in.Prompt = "> "
	in.Placeholder = "chat <msg> | list | pick | help"
	in.Focus()
	// sane defaults so the first frame renders before WindowSizeMsg arrives
	// (and in size-less ptys used by the test harness)
	return Model{
		nick: nick, master: master, api: api, player: player,
		input: in, peers: map[string]bool{}, conn: "connecting",
		width: 80, height: 24,
	}
}

func (m Model) Init() tea.Cmd {
	return textinput.Blink
}

type StreamClosedMsg struct{}
type MpvExitedMsg struct{}
type ReconnectingMsg struct{ Note string }
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
	m.log = append(m.log, fmt.Sprintf(format, args...))
	if len(m.log) > logCap {
		m.log = m.log[len(m.log)-logCap:]
	}
	return m
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.Width = msg.Width - 4
	case tea.KeyMsg:
		if m.mode == modePick {
			return m.pickKeys(msg)
		}
		switch msg.String() {
		case "enter":
			return m.submit()
		case "esc":
			m.input.SetValue("")
		case "ctrl+c":
			return m, tea.Quit
		default:
			var c tea.Cmd
			m.input, c = m.input.Update(msg)
			return m, c
		}
	case room.Event:
		return m.onEvent(msg), nil
	case sync.UiState:
		d := msg.Drift
		m.drift = &d
	case filesMsg:
		if m.mode == modePick {
			m.pickItems = msg.files
			if m.pickCursor >= len(m.pickItems) {
				m.pickCursor = max(0, len(m.pickItems)-1)
			}
			if msg.err != nil {
				return m.logf("> err: %s", msg.err), nil
			}
			return m, nil
		}
		if msg.err != nil {
			return m.logf("> err: %s", msg.err), nil
		}
		for _, f := range msg.files {
			m = m.logf("  %s", f)
		}
	case errMsg:
		return m.logf("> err: %s", msg.err), nil
	case ReconnectingMsg:
		m.conn = "reconnecting"
		return m.logf("%s", msg.Note), nil
	case StreamClosedMsg:
		m.conn = "lost"
		return m.logf("> connection closed"), nil
	case MpvExitedMsg:
		return m.logf("> mpv exited (window closed?) - restart the client"), tea.Quit
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
	case "esc":
		m.mode = modeInput
	}
	return m, nil
}

// submit parses the input line and dispatches it.
func (m Model) submit() (tea.Model, tea.Cmd) {
	line := strings.TrimSpace(m.input.Value())
	m.input.SetValue("")
	if line == "" {
		return m, nil
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
		m = m.logf("[%s] %s", m.nick, c.Arg)
		return m, postEvent(m.api, room.Event{Type: "chat", Nick: m.nick, Text: c.Arg})
	case cmd.List:
		return m, fetchFiles(m.api)
	case cmd.Pick:
		m.mode = modePick
		m.pickCursor = 0
		m.pickItems = nil
		return m, fetchFiles(m.api)
	case cmd.Play:
		m = m.logf("> playing %s", c.Arg) // local; own load broadcast is echo-filtered
		return m, playFile(m.player, m.api, c.Arg)
	case cmd.Queue:
		return m, postEvent(m.api, room.Event{Type: "enqueue", Name: c.Arg})
	case cmd.Skip:
		return m, postEvent(m.api, room.Event{Type: "skip"})
	}
	return m, nil
}

func (m Model) onEvent(ev room.Event) Model {
	if line := RenderEvent(ev); line != "" {
		m = m.logf("%s", line)
	}
	switch ev.Type {
	case "hello":
		m.now = ev.Now
		m.conn = "connected"
	case "join":
		m.peers[ev.Nick] = true
	case "bye":
		delete(m.peers, ev.Nick)
	case "load":
		m.now = ev.Name
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

func (m Model) View() string {
	if m.height == 0 {
		return "connecting…\n"
	}
	body := m.height - 2
	var lines []string
	if m.mode == modePick {
		lines = m.pickView()
	} else {
		lines = m.log
	}
	if len(lines) > body {
		lines = lines[len(lines)-body:]
	}
	var b strings.Builder
	b.WriteString(statusStyle.Render(m.statusLine()))
	b.WriteByte('\n')
	for i := 0; i < body; i++ {
		if i < len(lines) {
			b.WriteString(truncate(lines[i], m.width))
		}
		if i < body-1 {
			b.WriteByte('\n')
		}
	}
	b.WriteByte('\n')
	b.WriteString(m.input.View())
	return b.String()
}

func (m Model) pickView() []string {
	out := []string{"pick a film  (up/down/j/k move - enter play - esc cancel)"}
	if len(m.pickItems) == 0 {
		out = append(out, "(no files / loading...)")
	}
	for i, f := range m.pickItems {
		if i == m.pickCursor {
			out = append(out, pickCurStyle.Render("› "+f))
		} else {
			out = append(out, "  "+f)
		}
	}
	return out
}

func (m Model) statusLine() string {
	role := "follower"
	if m.master {
		role = "master"
	}
	mid := ""
	if m.master {
		mid = fmt.Sprintf("%d listener(s)", len(m.peers))
	} else if m.drift != nil {
		mid = fmt.Sprintf("drift %.1fs", *m.drift)
	} else {
		mid = m.peerLine()
	}
	return fmt.Sprintf("watchparty - %s - now: %s - %s - %s", role, orDash(m.now), mid, m.conn)
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
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return string(r[:w-1]) + "…"
}
