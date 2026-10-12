// Package ui renders the Bubble Tea control surface: a two-line header
// (identity/connection, now-playing clock), a full-width progress bar, a
// fixed chat viewport (engine chatter renders dim and collapses while
// consecutive), a queue-preview footer and a pinned input line. Framed
// with lipgloss; 256-color safe; single render path from 80x24 up.
package ui

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"watchparty/client-go/internal/cmd"
	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
	"watchparty/client-go/internal/sync"
)

const (
	logCap    = 500
	histCap   = 100
	durProbes = 20 // duration fetch retries after a load (500ms apart)
)

type mode int

const (
	modeInput mode = iota
	modePick
)

// entryKind separates human messages from machine chatter: chat renders
// bright, engine/event lines render dim and collapse while consecutive.
type entryKind int

const (
	entChat entryKind = iota
	entEngine
	entEvent
)

type entry struct {
	kind entryKind
	nick string // chat only
	text string
}

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
	playingStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	pausedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	nickColors   = []lipgloss.Color{"205", "45", "214", "84", "117", "212"}
	pickCur      = lipgloss.NewStyle().Bold(true).Reverse(true)
)

// nudgeRe matches speed-nudge log lines ("[1.050]"): pure chatter whose
// live value is already visible as the footer drift chip.
var nudgeRe = regexp.MustCompile(`^\[\d+\.\d+\]$`)

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
	vp         viewport.Model
	log        []entry
	peers      map[string]bool
	now        string
	queue      []string // server queue state (hello/queue events)
	files      []string // last known video list (typo guard, any filesMsg)
	st         *sync.UiState
	conn       string // connecting | connected | reconnecting | lost
	connSince  time.Time
	retry      int // reconnect attempts since the last connect
	retryDelay int // current backoff in seconds
	duration   float64
	mode       mode
	pickItems  []string
	pickCursor int
	hist       []string // input history, newest last
	histIdx    int      // -1 = live editing
	draft      string   // saved while browsing history
	width      int
	height     int
}

func New(nick string, api *room.Client, player *mpv.Player, master bool) Model {
	in := textinput.New()
	in.Placeholder = "type to chat · list | pick | play | queue | skip | help"
	in.Focus()
	// sane defaults so the first frame renders before WindowSizeMsg arrives
	// (and in size-less ptys used by the test harness)
	m := Model{
		nick: nick, master: master, api: api, player: player,
		input: in, peers: map[string]bool{}, conn: "connecting",
		histIdx: -1, width: 80, height: 24,
		vp: viewport.New(76, 17),
	}
	return m.syncLayout()
}

func (m Model) Init() tea.Cmd {
	return textinput.Blink
}

type StreamClosedMsg struct{}
type MpvExitedMsg struct{}
type ReconnectingMsg struct {
	Note  string
	Delay int // current backoff seconds
	Try   int // reconnect attempt number (1-based)
}
type EngineLogMsg struct{ Line string } // engine log line (stderr writes would corrupt the frame)
type errMsg struct{ err error }
type filesMsg struct {
	files []string
	err   error
}
type loadedMsg struct{ name string } // own load finished (server never echoes it back)
type durationMsg struct {            // mpv duration for the file named below
	name string
	dur  float64
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
		return loadedMsg{name}
	}
}

func fetchFiles(api *room.Client) tea.Cmd {
	return func() tea.Msg {
		files, err := api.Files()
		return filesMsg{files, err}
	}
}

// fetchDuration polls mpv for the duration of the current file; loadfile is
// async so a fresh load needs a few probes before the property exists.
func fetchDuration(pl *mpv.Player, name string) tea.Cmd {
	if pl == nil {
		return nil
	}
	return func() tea.Msg {
		for i := 0; i < durProbes; i++ {
			if v, err := pl.Get("duration"); err == nil && v != nil {
				if d, ok := v.(float64); ok && d > 0 {
					return durationMsg{name, d}
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
		return nil
	}
}

func (m Model) addEntry(e entry) Model {
	m.log = append(m.log, e)
	if len(m.log) > logCap {
		m.log = m.log[len(m.log)-logCap:]
	}
	return m
}

func (m Model) logChat(nick, text string) Model {
	return m.addEntry(entry{kind: entChat, nick: nick, text: text})
}

func (m Model) logEvent(text string) Model {
	return m.addEntry(entry{kind: entEvent, text: text})
}

func (m Model) logEngine(text string) Model {
	if nudgeRe.MatchString(strings.TrimSpace(text)) {
		return m // speed-nudge chatter: the footer drift chip shows it live
	}
	return m.addEntry(entry{kind: entEngine, text: text})
}

func (m Model) logf(format string, args ...any) Model {
	return m.logEngine(fmt.Sprintf(format, args...))
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m, c := m.update(msg)
	return m.syncViewport(), c
}

func (m Model) update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m.syncLayout(), nil
	case tea.MouseMsg: // wheel scrolling in the chat viewport
		var c tea.Cmd
		m.vp, c = m.vp.Update(msg)
		return m, c
	case tea.KeyMsg:
		if m.mode == modePick {
			return m.pickKeys(msg)
		}
		return m.inputKeys(msg)
	case room.Event:
		return m.onEvent(msg)
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
		m.retry, m.retryDelay = msg.Try, msg.Delay
		return m.logEngine(msg.Note), nil
	case EngineLogMsg:
		return m.logEngine(msg.Line), nil
	case StreamClosedMsg:
		m.conn = "lost"
		m.connSince = time.Time{}
		return m.logEngine("> connection closed"), nil
	case MpvExitedMsg:
		return m.logEngine("> mpv exited (window closed?) - restart the client"), tea.Quit
	case loadedMsg: // the server never echoes our own load back to us
		if msg.name != m.now {
			m.now = msg.name
			m.duration = 0
			return m, fetchDuration(m.player, m.now)
		}
	case durationMsg:
		if msg.name == m.now && msg.dur > 0 {
			m.duration = msg.dur
		}
	}
	return m, nil
}

// inputKeys handles typing plus history browsing and viewport scrolling.
func (m Model) inputKeys(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		return m.submit()
	case "esc":
		m.input.SetValue("")
		m.vp.GotoBottom()
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
		m.vp.ScrollUp(m.vp.Height)
	case "pgdown":
		m.vp.ScrollDown(m.vp.Height)
	case "home":
		m.vp.GotoTop()
	case "end":
		m.vp.GotoBottom()
	case "ctrl+p":
		m.mode = modePick
		m.pickCursor = 0
		m.pickItems = nil
		return m, fetchFiles(m.api)
	default:
		var c tea.Cmd
		m.input, c = m.input.Update(msg)
		return m, c
	}
	return m, nil
}

// pickKeys handles the cursor list: arrows/j/k move, enter plays, esc backs.
func (m Model) pickKeys(msg tea.KeyMsg) (Model, tea.Cmd) {
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

// isCommand reports whether the line starts with a reserved verb; those
// keep their command meaning. Everything else typed at the prompt is chat —
// talking is the primary input, no prefix needed. The explicit "chat"
// verb keeps working (user habit, headless harness parity).
func isCommand(line string) bool {
	verb, _, _ := strings.Cut(line, " ")
	switch verb {
	case cmd.List, cmd.Pick, cmd.Play, cmd.Queue, cmd.Skip, cmd.Chat, cmd.Quit, cmd.Help:
		return true
	}
	return false
}

// submit parses the input line and dispatches it. Bare text goes to the
// room as chat; reserved verbs dispatch as commands with their existing
// parse errors (a typo'd "play" without argument errors instead of
// accidentally chatting the word "play").
func (m Model) submit() (Model, tea.Cmd) {
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
	if !isCommand(line) {
		if utf8.RuneCountInString(line) > cmd.ChatMaxLen {
			return m.logf("> chat too long (max %d chars)", cmd.ChatMaxLen), nil
		}
		// local echo: the server never reflects our own chat back
		m = m.logChat(m.nick, line)
		return m, postEvent(m.api, room.Event{Type: "chat", Nick: m.nick, Text: line})
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
		m = m.logChat(m.nick, c.Arg)
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
				return m.logEngine("> queue empty (queue <file> to add)"), nil
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

func (m Model) onEvent(ev room.Event) (Model, tea.Cmd) {
	var c tea.Cmd
	if ev.Type == "chat" {
		m = m.logChat(ev.Nick, ev.Text)
	} else if line := RenderEvent(ev); line != "" {
		m = m.logEvent(line)
	}
	switch ev.Type {
	case "hello":
		m.conn = "connected"
		m.retry, m.retryDelay = 0, 0
		m.connSince = time.Now()
		m.duration = 0
		if ev.Now != m.now { // joining mid-film or reconnecting: probe the file mpv is loading
			m.now = ev.Now
			c = fetchDuration(m.player, m.now)
		}
		m.queue = ev.Queue
	case "join":
		m.peers[ev.Nick] = true
	case "bye":
		delete(m.peers, ev.Nick)
	case "load": // auto-advance and other people's plays (own loads arrive as loadedMsg)
		if ev.Name != m.now {
			m.now = ev.Name
			m.duration = 0
			c = fetchDuration(m.player, m.now)
		}
	case "queue":
		m.queue = ev.Queue
	}
	// position heartbeats are deliberately not logged (client.py parity)
	return m, c
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

// syncLayout applies the derived geometry (viewport size, input width) to
// the widgets; layout() below is the single source of the math.
func (m Model) syncLayout() Model {
	cw, vh, _, _ := m.layout()
	m.vp.Width, m.vp.Height = cw, vh
	m.input.Prompt = m.prompt()
	m.input.Width = max(10, cw-lipgloss.Width(m.prompt())-1)
	return m
}

// syncViewport rebuilds the viewport content (chat or picker) while keeping
// follow-bottom: fresh lines stick only when the user is at the bottom.
func (m Model) syncViewport() Model {
	if m.mode == modePick {
		m.vp.SetContent(strings.Join(m.pickLines(), "\n"))
		if m.pickCursor < m.vp.YOffset {
			m.vp.SetYOffset(m.pickCursor)
		} else if m.pickCursor >= m.vp.YOffset+m.vp.Height {
			m.vp.SetYOffset(m.pickCursor - m.vp.Height + 1)
		}
		return m
	}
	atBottom := m.vp.AtBottom()
	m.vp.SetContent(strings.Join(m.renderLines(), "\n"))
	if atBottom {
		m.vp.GotoBottom()
	}
	return m
}

// layout derives the content geometry from the terminal size: inner width,
// viewport height and whether the optional rows (progress bar, footer) fit.
// Below ~20 rows the optional rows degrade first; the layout never splits.
func (m Model) layout() (cw, vh int, showBar, showFooter bool) {
	cw = m.contentWidth()
	inner := m.height - frame.GetVerticalFrameSize()
	if inner < 3 {
		inner = 3
	}
	fixed := 3 // two header lines + input
	showBar = m.duration > 0 && m.st != nil && m.st.Pos != nil && inner-fixed >= 4
	if showBar {
		fixed++
	}
	showFooter = inner-fixed >= 3
	if showFooter {
		fixed++
	}
	vh = max(1, inner-fixed)
	return cw, vh, showBar, showFooter
}

// contentWidth is the usable width inside the frame.
func (m Model) contentWidth() int {
	w := m.width - frame.GetHorizontalFrameSize()
	if w < 20 {
		w = 20
	}
	return w
}

func (m Model) prompt() string {
	return nickStyle(m.nick).Render(m.nick) + " › "
}

func (m Model) View() string {
	if m.height == 0 {
		return "connecting…\n"
	}
	cw, vh, showBar, showFooter := m.layout()
	vp := m.vp // sized copy: View must not depend on Update having run first
	vp.Width, vp.Height = cw, vh

	var b strings.Builder
	l1, l2 := m.headerLines(cw)
	b.WriteString(l1 + "\n" + l2 + "\n")
	if showBar {
		b.WriteString(m.progressBar(cw) + "\n")
	}
	b.WriteString(vp.View() + "\n")
	if showFooter {
		b.WriteString(m.footerLine(cw) + "\n")
	}
	hint := ""
	if !vp.AtBottom() {
		hint = dim.Render("↑ more")
	}
	b.WriteString(joinLR(m.input.View(), hint, cw))
	return frame.Render(b.String())
}

// renderLines converts the log into display lines: chat bright, engine and
// event lines dim; consecutive same-type engine lines collapse to their last
// line plus a hidden count, so sync chatter never buries the conversation.
func (m Model) renderLines() []string {
	cw := m.contentWidth()
	var out []string
	for i := 0; i < len(m.log); i++ {
		e := m.log[i]
		if e.kind != entEngine {
			out = append(out, m.renderEntry(e, cw)...)
			continue
		}
		j := i
		for j+1 < len(m.log) && m.log[j+1].kind == entEngine &&
			engineKind(m.log[j+1].text) == engineKind(e.text) {
			j++
		}
		lines := m.renderEntry(m.log[j], cw)
		if j > i {
			lines[len(lines)-1] += dim.Render(fmt.Sprintf("  (%d)", j-i))
		}
		out = append(out, lines...)
		i = j
	}
	return out
}

func (m Model) renderEntry(e entry, cw int) []string {
	switch e.kind {
	case entChat:
		prefix := nickStyle(e.nick).Render(e.nick)
		return wrapIndented(e.text, cw-lipgloss.Width(prefix)-1, lipgloss.Width(prefix)+1, prefix+" ")
	case entEngine:
		return wrapIndented(e.text, cw, 0, "")
	default: // entEvent: dim, "·"-prefixed — visibly not a human speaking
		lines := wrapIndented(strings.TrimPrefix(e.text, "> "), cw, 2, "")
		lines[0] = "· " + lines[0]
		for i, l := range lines {
			lines[i] = dim.Render(l)
		}
		return lines
	}
}

// engineKind is the collapse key: the first token after the ">" prompt, so
// "> pause" and "> play" never merge (the last real state must stay visible)
// while repeats like "> seek @1.0s", "> seek @2.0s" collapse to the newest.
func engineKind(text string) string {
	t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), ">"))
	if i := strings.IndexAny(t, " \t"); i >= 0 {
		t = t[:i]
	}
	return t
}

// wrapIndented word-wraps plain text to width; continuation lines get
// indent spaces. A single word wider than the line is hard-cut with an
// ellipsis (display-width aware, never overflows the frame).
func wrapIndented(text string, width, indent int, first string) []string {
	if width < 12 {
		width = 12
	}
	empty := lipgloss.Width(first)
	line, lw := first, empty
	var out []string
	flush := func() {
		out = append(out, strings.TrimRight(line, " "))
		line, lw, empty = strings.Repeat(" ", indent), indent, indent
	}
	for _, word := range strings.Fields(text) {
		for {
			need := lipgloss.Width(word)
			if lw > empty {
				need++ // separating space
			}
			if lw+need <= width || lw == empty {
				break // fits, or the line is fresh and must take the word
			}
			flush()
		}
		if lw == empty && lw+lipgloss.Width(word) > width { // wider than a whole line
			fit := width - lw - 1
			cut := word
			for lipgloss.Width(cut) > fit && len(cut) > 0 {
				_, sz := utf8.DecodeLastRuneInString(cut)
				cut = cut[:len(cut)-sz]
			}
			out = append(out, strings.TrimRight(line+cut, " ")+"…")
			line, lw, empty = strings.Repeat(" ", indent), indent, indent // fresh line, nothing to flush
			continue
		}
		sep := ""
		if lw > empty {
			sep = " "
		}
		line += sep + word
		lw += len(sep) + lipgloss.Width(word)
	}
	if line != "" || len(out) == 0 {
		out = append(out, strings.TrimRight(line, " "))
	}
	return out
}

func (m Model) pickLines() []string {
	out := []string{title.Render("pick a film") + dim.Render("  up/down move · enter play · q queue · esc cancel")}
	if len(m.pickItems) == 0 {
		out = append(out, dim.Render("(no files / loading…)"))
	}
	for i, f := range m.pickItems {
		if i == m.pickCursor {
			out = append(out, pickCur.Render(" ▸ "+f+" "))
		} else {
			out = append(out, "   "+f)
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

// headerLines renders the two header rows: identity + connection, then
// now-playing + clock.
func (m Model) headerLines(cw int) (string, string) {
	role := "follower"
	if m.master {
		role = "master"
	}
	l1 := joinLR(roleStyle[role].Render("watchparty "+role), m.connChip(), cw)
	l2 := joinLR(m.nowChip(), m.clockChip(), cw)
	return l1, l2
}

func (m Model) connChip() string {
	switch m.conn {
	case "connected":
		up := ""
		if !m.connSince.IsZero() {
			up = " · up " + humanDur(time.Since(m.connSince))
		}
		return connStyle["connected"].Render("● connected" + up)
	case "reconnecting":
		return connStyle["reconnecting"].Render(fmt.Sprintf("○ retry %d · backoff %ds", m.retry, m.retryDelay))
	case "lost":
		return connStyle["lost"].Render("● lost")
	default:
		return dim.Render("○ connecting")
	}
}

func (m Model) nowChip() string {
	if m.now == "" {
		if m.st != nil && m.st.Pos != nil {
			return dim.Render("—") // playing, but the title is not known yet
		}
		return dim.Render("— nothing playing")
	}
	icon, style := "▶", playingStyle
	if m.st != nil && m.st.Paused {
		icon, style = "⏸", pausedStyle
	}
	return style.Render(icon) + " " + m.now
}

func (m Model) clockChip() string {
	if m.st == nil || m.st.Pos == nil {
		return ""
	}
	clock := clockStr(*m.st.Pos)
	if m.duration > 0 {
		clock += " / " + clockStr(m.duration)
	}
	return dim.Render(clock)
}

// progressBar renders the full-width elapsed bar; green while playing,
// amber while paused so the state reads even in the corner of an eye.
func (m Model) progressBar(cw int) string {
	if m.duration <= 0 || m.st == nil || m.st.Pos == nil {
		return ""
	}
	frac := *m.st.Pos / m.duration
	frac = min(max(frac, 0), 1)
	fill := int(frac * float64(cw))
	col := lipgloss.Color("2")
	if m.st.Paused {
		col = lipgloss.Color("3")
	}
	return lipgloss.NewStyle().Foreground(col).Render(strings.Repeat("█", fill)) +
		lipgloss.NewStyle().Foreground(lipgloss.Color("238")).Render(strings.Repeat("░", cw-fill))
}

// footerLine: queue preview on the left, the live sync gauge on the right.
func (m Model) footerLine(cw int) string {
	right := ""
	if m.master {
		right = dim.Render(fmt.Sprintf("%d listening", len(m.peers)))
	} else if m.st != nil && m.st.Pos != nil {
		d := m.st.Drift
		col := "2" // inside the smooth-sync band
		if d > 1 || d < -1 {
			col = "1" // hard re-seek territory
		} else if d > 0.15 || d < -0.15 {
			col = "3" // tempo-nudge territory
		}
		right = lipgloss.NewStyle().Foreground(lipgloss.Color(col)).
			Render(fmt.Sprintf("drift %+.1fs", d))
	}
	return joinLR(m.queuePreview(), right, cw)
}

func (m Model) queuePreview() string {
	if len(m.queue) == 0 {
		return dim.Render("queue empty")
	}
	parts := make([]string, 0, min(2, len(m.queue)))
	for i, q := range m.queue {
		if i == 2 {
			break
		}
		parts = append(parts, fmt.Sprintf("%d. %s", i+1, q))
	}
	s := dim.Render("next ") + strings.Join(parts, " · ")
	if len(m.queue) > 2 {
		s += dim.Render(fmt.Sprintf(" (+%d)", len(m.queue)-2))
	}
	return s
}

// joinLR puts left and right on one line, right-aligned; drops the right
// side when they would collide (never cuts styled strings mid-escape).
func joinLR(l, r string, w int) string {
	if r == "" {
		return l
	}
	gap := w - lipgloss.Width(l) - lipgloss.Width(r)
	if gap < 1 {
		return l
	}
	return l + strings.Repeat(" ", gap) + r
}

// clockStr formats seconds as m:ss below an hour, h:mm:ss above.
func clockStr(sec float64) string {
	s := int(sec)
	if s < 0 {
		s = 0
	}
	if s < 3600 {
		return fmt.Sprintf("%d:%02d", s/60, s%60)
	}
	return fmt.Sprintf("%d:%02d:%02d", s/3600, s%3600/60, s%60)
}

// humanDur renders "42s", "12m" or "1h 3m" for the connection age.
func humanDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
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
