// Package ui renders the Bubble Tea control surface: a two-line header
// (identity/connection, now-playing clock), a full-width progress bar, a
// fixed chat viewport (engine chatter renders dim and collapses while
// consecutive), a queue-preview footer and a pinned input line. Framed
// with lipgloss; 256-color safe; single render path from 80x24 up.
package ui

import (
	"fmt"
	"math/rand"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/spinner"
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
	logCap      = 500
	histCap     = 100
	durProbes   = 20  // duration fetch retries after a load (500ms apart)
	dashMin     = 100 // two-column dashboard needs this many columns
	dashMinH    = 20  // ... and this many rows; narrower falls back to the stacked layout
	seekStep    = 5.0 // ctrl+arrow nudge, seconds
	sysRows     = 30  // system tail lines in the dashboard
	eqBars      = 40  // vibe equalizer column budget (widest terminal)
	eqPeriod    = 150 * time.Millisecond
	zenDimAfter = 15 * time.Minute // zen only: messages older than this fade out
)

type mode int

const (
	modeInput mode = iota
	modePick
)

// pickAction is what the picker's enter key does: play the file or add
// it to the room queue. Bare /play opens the picker in play mode, bare
// /queue in queue mode (F4): the queue action must be reachable without
// knowing the hidden q shortcut.
type pickAction int

const (
	pickActPlay pickAction = iota
	pickActQueue
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
	at   time.Time // chat only: timestamp gutter + same-nick grouping
}

var (
	frame   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	dim     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	statLbl = lipgloss.NewStyle().Foreground(lipgloss.Color("245")) // stat labels: readable, never competes with values
	title   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))

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
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	joinStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	leftStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	noticeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	seekStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	loadStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	nickColors   = []lipgloss.Color{"205", "45", "214", "84", "117", "212"}
	pickCur      = lipgloss.NewStyle().Bold(true).Reverse(true)
	timeStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	gutStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	keyStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	// per-box border colors: each panel reads at a glance
	bordPlay      = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	bordPause     = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	bordIdle      = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	bordWatch     = lipgloss.NewStyle().Foreground(lipgloss.Color("141"))
	bordQueue     = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
	bordQueueIdle = lipgloss.NewStyle().Foreground(lipgloss.Color("168")) // muted orchid: same family, quieter
	bordSys       = lipgloss.NewStyle().Foreground(lipgloss.Color("208"))
	bordChat      = lipgloss.NewStyle().Foreground(lipgloss.Color("75"))
	bordMsg       = lipgloss.NewStyle().Foreground(lipgloss.Color("179"))
	bordVibe      = lipgloss.NewStyle().Foreground(lipgloss.Color("84"))
)

// nudgeRe matches speed-nudge log lines ("[1.050]"): pure chatter whose
// live value is already visible as the footer drift chip.
var nudgeRe = regexp.MustCompile(`^\[\d+\.\d+\]$`)

// usageTUI is the slash-style command reference shown in the TUI; the
// bare-verb syntax stays in cmd.Usage for the headless test harness.
const usageTUI = "commands: /list · /pick · /play [file] · /queue [file] · /skip · /chat <msg> · /quit · /help — plain text is chat · tab completes · ctrl+space play/pause · ctrl+←/→ ±5s · ctrl+z movie mode · ctrl+g system pane"

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
	engine     *sync.Engine
	input      textinput.Model
	spin       spinner.Model  // connecting screen (pre-WindowSizeMsg)
	vp         viewport.Model // chat pane
	sysVp      viewport.Model // system tail: events + engine lines
	lastErr    string         // input-row feedback until the next keystroke
	log        []entry
	peers      map[string]bool
	now        string
	invite     string   // address to invite people to (server hello "invite")
	inviteAlt  string   // second invite address (server hello "inviteAlt")
	queue      []string // server queue state (hello/queue events)
	files      []string // last known video list (typo guard, any filesMsg)
	st         *sync.UiState
	conn       string // connecting | connected | reconnecting | lost
	connSince  time.Time
	retry      int // reconnect attempts since the last connect
	retryDelay int // current backoff in seconds
	duration   float64
	durCache   map[string]float64 // known file durations: a reload paints the bar instantly
	lostAt     time.Time          // when the connection dropped (reconnect delta)
	mode       mode
	pickItems  []string
	pickCursor int
	pickAct    pickAction // what enter does in the picker: play or enqueue
	hist       []string   // input history, newest last
	histIdx    int        // -1 = live editing
	draft      string     // saved while browsing history
	zen        bool       // movie-night mode: one status line + input only
	sysOff     bool       // ctrl+g: system pane hidden
	gone       []string   // departed nicks, shown offline (cap 5)
	eq         []float64  // vibe equalizer bar heights 0..1
	compOpen   bool       // Tab menu of command/file completions
	compItems  []compItem
	compCursor int
	compFile   bool // completing a file argument (vs a verb)
	compVerb   string
	compWord   string
	width      int
	height     int
}

// compItem is one row of the Tab completion menu.
type compItem struct {
	text string
	desc string
}

func New(nick string, api *room.Client, player *mpv.Player, master bool, engine *sync.Engine) Model {
	in := textinput.New()
	in.Placeholder = "type to chat · /pick /list /help"
	in.Focus()
	// sane defaults so the first frame renders before WindowSizeMsg arrives
	// (and in size-less ptys used by the test harness)
	m := Model{
		nick: nick, master: master, api: api, player: player, engine: engine,
		input: in, peers: map[string]bool{}, conn: "connecting",
		durCache: map[string]float64{},
		spin:     spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(dim)),
		histIdx:  -1, width: 80, height: 24,
		vp: viewport.New(76, 14), sysVp: viewport.New(76, 2),
	}
	return m.syncLayout()
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, m.spin.Tick, tea.Tick(eqPeriod, eqTick))
}

// eqTick drives the vibe equalizer animation.
func eqTick(time.Time) tea.Msg { return eqTickMsg{} }

type eqTickMsg struct{}

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

// playFile mirrors client.py's play: load locally first, then tell the
// room. The engine is told too (OwnLoad) — our own load never comes back
// as a room event, and without it a later hello would reload the very
// file mpv is already playing (F3, master half).
func playFile(pl *mpv.Player, api *room.Client, engine *sync.Engine, name string) tea.Cmd {
	return func() tea.Msg {
		if err := pl.Load(name); err != nil {
			return errMsg{err}
		}
		engine.OwnLoad(name)
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
func (m Model) fetchDuration(name string) tea.Cmd {
	pl := m.player
	if pl == nil {
		return func() tea.Msg { return nil } // no player (tests): nothing to probe
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

// setFile points the UI at a file: a duration we already measured paints
// the bar instantly (no mpv re-probe), an unknown one is probed in the
// background.
func (m Model) setFile(name string) (Model, tea.Cmd) {
	m.now = name
	if d, ok := m.durCache[name]; ok {
		// known duration: the bar renders instantly, but the file behind
		// the name may have been replaced (a new cut of the same film) —
		// one background probe corrects it
		m.duration = d
		return m, m.fetchDuration(name)
	}
	m.duration = 0
	return m, m.fetchDuration(name)
}

func (m Model) addEntry(e entry) Model {
	m.log = append(m.log, e)
	if len(m.log) > logCap {
		m.log = m.log[len(m.log)-logCap:]
	}
	return m
}

func (m Model) logChat(nick, text string) Model {
	return m.addEntry(entry{kind: entChat, nick: nick, text: text, at: time.Now()})
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
	case tea.MouseMsg: // wheel scrolls chat; a click on the bar seeks
		if c := m.barClick(msg); c != nil {
			return m, c
		}
		var c tea.Cmd
		m.vp, c = m.vp.Update(msg)
		return m, c
	case tea.KeyMsg:
		if m.mode == modePick {
			return m.pickKeys(msg)
		}
		return m.inputKeys(msg)
	case spinner.TickMsg:
		var sc tea.Cmd
		m.spin, sc = m.spin.Update(msg)
		return m, sc
	case eqTickMsg:
		return m.stepEq(), tea.Tick(eqPeriod, eqTick)
	case room.Event:
		return m.onEvent(msg)
	case sync.UiState:
		st := msg
		m.st = &st
	case filesMsg:
		if msg.err != nil {
			m.lastErr = msg.err.Error()
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
		m.lastErr = msg.err.Error()
		return m.logf("> err: %s", msg.err), nil
	case ReconnectingMsg:
		m.conn = "reconnecting"
		m.retry, m.retryDelay = msg.Try, msg.Delay
		if m.lostAt.IsZero() {
			m.lostAt = time.Now() // the outage starts now (C4)
		}
		return m.logEngine(msg.Note), nil
	case EngineLogMsg:
		return m.logEngine(msg.Line), nil
	case StreamClosedMsg:
		m.conn = "lost"
		m.connSince = time.Time{}
		if m.lostAt.IsZero() {
			m.lostAt = time.Now()
		}
		return m.logEngine("> connection closed"), nil
	case MpvExitedMsg:
		return m.logEngine("> mpv exited or stopped talking - restart the client"), tea.Quit
	case loadedMsg: // the server never echoes our own load back to us
		if msg.name != m.now {
			var c tea.Cmd
			m, c = m.setFile(msg.name)
			return m, c
		}
	case durationMsg:
		if msg.name == m.now && msg.dur > 0 {
			m.duration = msg.dur
		}
		if msg.dur > 0 { // remember it: the next load of this file is instant
			if m.durCache == nil {
				m.durCache = map[string]float64{}
			}
			m.durCache[msg.name] = msg.dur
		}
	}
	return m, nil
}

// inputKeys handles typing plus history browsing and viewport scrolling.
func (m Model) inputKeys(msg tea.KeyMsg) (Model, tea.Cmd) {
	if m.compOpen { // the completion menu owns the keys while open
		switch msg.String() {
		case "enter":
			m.acceptMenu()
			return m.syncLayout(), nil
		case "up":
			m.compCursor = max(0, m.compCursor-1)
			return m, nil
		case "down", "tab":
			if m.compCursor < len(m.compItems)-1 {
				m.compCursor++
			}
			return m, nil
		case "esc":
			m.compOpen = false
			return m.syncLayout(), nil
		}
	}
	switch msg.String() {
	case "enter":
		m.compOpen = false
		m, c := m.submit()
		return m.syncLayout(), c
	case "esc":
		m.input.SetValue("")
		m.vp.GotoBottom()
	case "ctrl+c":
		return m, tea.Quit
	case "tab":
		m, c := m.complete()
		return m.syncLayout(), c
	case "ctrl+@", "\x00": // ctrl+space sends NUL: play/pause without leaving the chat
		// (on Windows bubbletea delivers ctrl+space as a NUL rune, not as
		// "ctrl+@" — both spellings must toggle)
		if m.engine != nil {
			m.engine.PauseToggle()
		}
		return m, nil
	case "ctrl+left", "ctrl+right":
		if m.engine != nil {
			dt := -seekStep
			if msg.String() == "ctrl+right" {
				dt = seekStep
			}
			m.engine.SeekBy(dt)
		}
		return m, nil
	case "ctrl+z": // movie-night mode: one status line + chat + input.
		// ctrl+m is unreachable: terminals send CR for it (== enter)
		m.zen = !m.zen
		return m.syncLayout(), nil
	case "ctrl+g": // system pane toggle: hide the machine chatter
		m.sysOff = !m.sysOff
		return m.syncLayout(), nil
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
		return m.openPicker(pickActPlay)
	default:
		m.lastErr = "" // fresh keystroke: the old feedback is obsolete
		var c tea.Cmd
		m.input, c = m.input.Update(msg)
		if m.compOpen { // live refilter while the menu is up
			m = m.openMenu()
		}
		return m.syncLayout(), c
	}
	return m, nil
}

// openPicker opens the file browser: enter plays the file in play mode
// and enqueues it in queue mode (F4).
func (m Model) openPicker(act pickAction) (Model, tea.Cmd) {
	m.mode = modePick
	m.pickAct = act
	m.pickCursor = 0
	m.pickItems = nil
	return m, fetchFiles(m.api)
}

// pickKeys handles the cursor list: arrows/j/k move, enter does what the
// picker was opened for (play or queue), esc backs.
func (m Model) pickKeys(msg tea.KeyMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c": // the picker owns the keys, but ctrl+c must always quit
		return m, tea.Quit
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
			if m.pickAct == pickActQueue {
				return m.enqueue(name)
			}
			m = m.logf("> playing %s", name)
			return m, playFile(m.player, m.api, m.engine, name)
		}
	case "q": // the queue shortcut: it enqueues in both modes
		if m.pickCursor < len(m.pickItems) {
			m.mode = modeInput
			return m.enqueue(m.pickItems[m.pickCursor])
		}
	case "esc":
		m.mode = modeInput
	}
	return m, nil
}

// verbs is the tab-completion candidate set (cmd.Parse accepts these).
var verbs = []string{"pick", "play", "queue", "skip", "chat", "list", "help", "quit"}

// verbDescs feeds the Tab menu.
var verbDescs = map[string]string{
	"pick":  "browse and play/queue",
	"play":  "start a file for everyone",
	"queue": "append to the shared queue",
	"skip":  "jump to the next file",
	"chat":  "send a message",
	"list":  "list videos on the server",
	"help":  "show commands",
	"quit":  "exit",
}

// candidatesFor returns the completion matches for the current input:
// slash verbs while typing the first word, file names for /play and
// /queue arguments.
func (m Model) candidatesFor(v string) (items []compItem, isFile bool, verb, word string) {
	if v == "" {
		// an empty input + Tab shows the whole command menu: the verbs
		// must be discoverable without knowing the slash
		for _, c := range verbs {
			items = append(items, compItem{text: "/" + c, desc: verbDescs[c]})
		}
		return items, false, "", ""
	}
	if !strings.HasPrefix(v, "/") {
		return nil, false, "", ""
	}
	verbPart, rest, hasArg := strings.Cut(strings.TrimPrefix(v, "/"), " ")
	word = strings.TrimLeft(rest, " ")
	if !hasArg {
		for _, c := range verbs {
			if strings.HasPrefix(c, verbPart) {
				items = append(items, compItem{text: "/" + c, desc: verbDescs[c]})
			}
		}
		return items, false, "", verbPart
	}
	if verbPart == "play" || verbPart == "queue" {
		for _, f := range m.files {
			if strings.HasPrefix(f, word) {
				items = append(items, compItem{text: f})
			}
		}
		return items, true, verbPart, word
	}
	return nil, false, verbPart, word
}

// openMenu shows the completion menu (multiple matches); typing refilters,
// enter accepts, esc closes. On terminals too short to spare a single row
// it stays closed (Tab still inline-completes unique matches).
func (m Model) openMenu() Model {
	items, isFile, verb, word := m.candidatesFor(m.input.Value())
	if len(items) == 0 {
		m.compOpen = false
		return m
	}
	m.compOpen, m.compItems, m.compCursor = true, items, 0
	m.compFile, m.compVerb, m.compWord = isFile, verb, word
	if m.menuBudget() <= 0 {
		m.compOpen = false
	}
	return m
}

// acceptMenu fills the input from the selected row.
func (m *Model) acceptMenu() {
	if !m.compOpen || m.compCursor >= len(m.compItems) {
		return
	}
	it := m.compItems[m.compCursor]
	if m.compFile {
		m.input.SetValue("/" + m.compVerb + " " + it.text + " ")
	} else {
		m.input.SetValue(it.text + " ")
	}
	m.input.CursorEnd()
	m.compOpen = false
}

// complete implements Tab: a single match fills inline; several open the
// arrow-navigable menu.
func (m Model) complete() (Model, tea.Cmd) {
	items, _, _, _ := m.candidatesFor(m.input.Value())
	if len(items) == 1 {
		it := items[0]
		if it.desc != "" { // verb
			m.input.SetValue(it.text + " ")
		} else { // file
			v, _, _ := strings.Cut(strings.TrimPrefix(m.input.Value(), "/"), " ")
			m.input.SetValue("/" + v + " " + it.text + " ")
		}
		m.input.CursorEnd()
		return m, nil
	}
	return m.openMenu(), nil
}

// barClick turns a left click on the progress bar into a room seek.
// The stacked layout draws the bar on screen row 3 (border + two header
// lines) and starts at column frame-left + 1; the dashboard draws it
// inside the now-playing band (row 5). The click's column maps
// linearly to time.
func (m Model) barClick(msg tea.MouseMsg) tea.Cmd {
	if msg.Button != tea.MouseButtonLeft || msg.Action != tea.MouseActionPress ||
		m.duration <= 0 || m.st == nil || m.st.Pos == nil {
		return nil
	}
	if m.dashOn() && !m.zen {
		return m.dashBarClick(msg)
	}
	_, _, showBar, _, _ := m.layout()
	if !showBar || m.zen || m.mode == modePick {
		return nil
	}
	if msg.Y != frame.GetVerticalFrameSize()/2+2 { // top border (1) + two header rows = row 3
		return nil
	}
	cw := m.contentWidth()
	left := frame.GetHorizontalFrameSize() - 1 // border + padding, bar starts after
	if msg.X < left || msg.X > left+cw {
		return nil
	}
	frac := float64(msg.X-left) / float64(cw)
	t := frac * m.duration
	return m.seekRoom(t)
}

// dashBarClick maps a click on the dashboard band's progress bar to a
// room seek. The band spans the whole screen: its content starts after
// the box border and padding, the bar itself after the elapsed label.
func (m Model) dashBarClick(msg tea.MouseMsg) tea.Cmd {
	if m.mode == modePick {
		return nil
	}
	if msg.Y != 5 { // header, gap, band border, name, blank — then the bar
		return nil
	}
	cw := m.width - 4 // the band's content width
	elapsed, total := clockStr(*m.st.Pos), clockStr(m.duration)
	// the bar starts after the border, the padding, the elapsed label AND
	// the space that follows it (progressBar lays it out that way — the
	// comment claimed that before the arithmetic did, A5 W7)
	left := 3 + lipgloss.Width(elapsed) + 1
	barW := cw - lipgloss.Width(elapsed) - lipgloss.Width(total) - 4
	if barW < 1 || msg.X < left || msg.X > left+barW {
		return nil
	}
	frac := min(max(float64(msg.X-left)/float64(barW), 0), 1)
	return m.seekRoom(frac * m.duration)
}

// seekRoom moves our own player AND tells the room: the server never
// echoes a poster's own event back, so a POST-only seek does nothing for
// the clicker (the master would not move at all, a follower would snap
// back on the next heartbeat).
func (m Model) seekRoom(t float64) tea.Cmd {
	if m.engine != nil {
		m.engine.SeekTo(t)
	}
	return nil
}

// enqueue mirrors the server's enqueue conditions for instant feedback;
// on success its queue broadcast confirms (it reaches us too).
func (m Model) enqueue(name string) (Model, tea.Cmd) {
	if name == m.now {
		return m.failf("%s is already playing", name)
	}
	for _, q := range m.queue {
		if q == name {
			return m.failf("%s is already queued", name)
		}
	}
	if !m.knownFile(name) {
		return m.failf("no such file: %s (try /list)", name)
	}
	m = m.logf("> queued %s", name)
	return m, postEvent(m.api, room.Event{Type: "enqueue", Name: name})
}

// failf reports a rejected input: red on the input row (where the user
// is looking) plus the full line in the system log.
func (m Model) failf(format string, args ...any) (Model, tea.Cmd) {
	m.lastErr = fmt.Sprintf(format, args...)
	return m.logf("> %s", m.lastErr), nil
}

// submit stores history and splits the line: "/..." dispatches a command,
// everything else goes to the room as chat. Chat intent is the default —
// no bare word can ever trigger a command.
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
	if !strings.HasPrefix(line, "/") {
		if utf8.RuneCountInString(line) > cmd.ChatMaxLen {
			return m.failf("chat too long (max %d chars)", cmd.ChatMaxLen)
		}
		// local echo: the server never reflects our own chat back
		m = m.logChat(m.nick, line)
		return m, postEvent(m.api, room.Event{Type: "chat", Nick: m.nick, Text: line})
	}
	return m.dispatch(line)
}

// dispatch runs one slash command. "//text" escapes to chat with a literal
// slash; a lone "/" shows the command reference.
func (m Model) dispatch(line string) (Model, tea.Cmd) {
	body := strings.TrimPrefix(line, "/")
	if body == "" {
		return m.logf("> %s", usageTUI), nil
	}
	if strings.HasPrefix(body, "/") {
		m = m.logChat(m.nick, body)
		return m, postEvent(m.api, room.Event{Type: "chat", Nick: m.nick, Text: body})
	}
	// Bare /play and bare /queue open the picker in the matching mode
	// (F4): the queue action must be discoverable without knowing the
	// hidden q shortcut. With an argument they keep their direct
	// behaviour (play it / enqueue it).
	verb, arg, _ := strings.Cut(body, " ")
	switch verb {
	case cmd.Play:
		if strings.TrimSpace(arg) == "" {
			return m.openPicker(pickActPlay)
		}
	case cmd.Queue:
		if strings.TrimSpace(arg) == "" {
			return m.openPicker(pickActQueue)
		}
	}
	c, err := cmd.Parse(body)
	if err != nil {
		return m.failf("%s", firstLine(err))
	}
	switch c.Kind {
	case cmd.Quit:
		return m, tea.Quit
	case cmd.Help:
		return m.logf("> %s", usageTUI), nil
	case cmd.Chat:
		// local echo: the server never reflects our own chat back
		m = m.logChat(m.nick, c.Arg)
		return m, postEvent(m.api, room.Event{Type: "chat", Nick: m.nick, Text: c.Arg})
	case cmd.List, cmd.Pick: // /list opens the picker too: a 2-line system strip is no place for a file dump
		return m.openPicker(pickActPlay)
	case cmd.Play:
		if !m.knownFile(c.Arg) {
			return m.failf("no such file: %s (try /list)", c.Arg)
		}
		m = m.logf("> playing %s", c.Arg) // local; own load broadcast is echo-filtered
		return m, playFile(m.player, m.api, m.engine, c.Arg)
	case cmd.Queue:
		return m.enqueue(c.Arg)
	case cmd.Skip:
		if len(m.queue) == 0 { // nothing to skip to: say so instead of no-op
			return m.failf("queue is empty — nothing to skip to (use /pick)")
		}
		return m, postEvent(m.api, room.Event{Type: "skip"})
	}
	return m, nil
}

// firstLine strips the usage trailer cmd.Parse appends to argument errors —
// the full reference is one /help away, the input row has one line only.
func firstLine(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	return line
}

func (m Model) onEvent(ev room.Event) (Model, tea.Cmd) {
	var c tea.Cmd
	if ev.Type == "chat" {
		m = m.logChat(ev.Nick, ev.Text)
	} else if line := RenderEvent(ev, m.nick); line != "" {
		m = m.logEvent(line)
	}
	switch ev.Type {
	case "hello":
		m.conn = "connected"
		m.retry, m.retryDelay = 0, 0
		m.connSince = time.Now()
		if !m.lostAt.IsZero() { // the outage is over: say how long it lasted
			m = m.logEvent(fmt.Sprintf("> reconnected after %s", humanDur(time.Since(m.lostAt))))
			m.lostAt = time.Time{}
		}
		if ev.Now != m.now { // joining mid-film or reconnecting: probe the file mpv is loading
			m, c = m.setFile(ev.Now)
		}
		m.queue = ev.Queue
		m.invite = ev.Invite       // the server knows its own reachable address
		m.inviteAlt = ev.InviteAlt // ...and a second one (tailnet + LAN)
		// roster replaces guesswork: a late joiner sees everyone already in
		// the room, not only the joins that happened after them
		m.peers = map[string]bool{}
		m.gone = nil // the roster is current truth: offline tags reset
		for _, n := range ev.Roster {
			if n != "" && n != m.nick {
				m.peers[n] = true
			}
		}
	case "join":
		m.peers[ev.Nick] = true
		for i, g := range m.gone { // a returning nick sheds its offline tag
			if g == ev.Nick {
				m.gone = append(m.gone[:i], m.gone[i+1:]...)
				break
			}
		}
	case "bye":
		delete(m.peers, ev.Nick)
		m.gone = append(m.gone, ev.Nick)
		if len(m.gone) > 5 {
			m.gone = m.gone[len(m.gone)-5:]
		}
	case "load": // auto-advance and other people's plays (own loads arrive as loadedMsg)
		if ev.Name != m.now {
			m, c = m.setFile(ev.Name)
		}
	case "queue":
		m.queue = ev.Queue
	case "notice":
		// the engine demoted ITSELF when the room named us as the second
		// master: the UI must follow, or it keeps showing the master clock
		// and hides the drift of a client that now follows the other one
		if ev.From != "" && m.master && m.engine != nil && ev.From == m.engine.ID {
			m.master = false
		}
	}
	// position heartbeats are deliberately not logged (client.py parity)
	return m, c
}

// RenderEvent formats one room event as a log line shared by the TUI and
// the headless adapter (client.py log parity). Empty = not rendered.
// self is our own nick: an event the room stamped with someone else's
// nick names its actor, our own (or an unstamped) event keeps the bare
// format.
func RenderEvent(ev room.Event, self string) string {
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
		return fmt.Sprintf("> queue: %s", queueStr(ev.Queue)) + actor(ev.Nick, self)
	case "load":
		return fmt.Sprintf("> load %s", ev.Name) + actor(ev.Nick, self)
	case "play", "pause":
		if ev.Mechanical {
			// a stall on the other side's link: nobody pressed anything, so
			// the log must not blame a human either (m2-S2, A5 W4)
			return "> " + ev.Type + " " + stallMark
		}
		return "> " + ev.Type + actor(ev.Nick, self)
	case "notice":
		return "> " + ev.Text
	case "seek":
		return fmt.Sprintf("> seek @%.1fs%s", ev.T, actor(ev.Nick, self))
	}
	return ""
}

// stallMark marks a mechanical pause in the log: the room paused itself
// (a stall on somebody's link), nobody pressed anything.
const stallMark = "(stall)"

// actor names who changed the state (" by bo") when the room stamped
// someone else's nick on the event.
func actor(nick, self string) string {
	if nick == "" || nick == self {
		return ""
	}
	return " by " + nick
}

// syncLayout applies the derived geometry (viewport size, input width) to
// the widgets; layout() below is the single source of the math.
func (m Model) syncLayout() Model {
	cw, vh, _, _, _ := m.layout()
	m.vp.Width, m.vp.Height = cw, vh
	m.sysVp.Width, m.sysVp.Height = cw, 2
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
	} else {
		atBottom := m.vp.AtBottom()
		content := ""
		if !m.hasChat() {
			content = m.inviteLines() // empty room: show what to send the other side
		} else {
			content = m.padChatBottom(strings.Join(m.renderChatLines(), "\n"))
		}
		m.vp.SetContent(content)
		if atBottom {
			m.vp.GotoBottom()
		}
	}
	// the system pane is a glanceable tail, never a reading surface: always
	// pinned to the newest line
	m.sysVp.SetContent(strings.Join(m.renderSysLines(), "\n"))
	m.sysVp.GotoBottom()
	return m
}

// layout derives the content geometry from the terminal size: inner width,
// chat viewport height and which optional rows fit. Below ~20 rows the
// optional rows degrade first (bar, system strip, footer); chat survives
// longest — the layout never splits horizontally.
// layoutCore derives the geometry WITHOUT the completion menu: the chat
// height the frame can afford before the menu borrows rows from it.
func (m Model) layoutCore() (cw, vh int, showBar, showSys, showFooter bool) {
	if !m.zen && m.dashOn() { // dashboard: the chat box owns the right column
		_, r := m.dashGeom()
		return r - 2, m.chatBase(), false, false, false
	}
	cw = m.contentWidth()
	inner := m.height - frame.GetVerticalFrameSize()
	if inner < 3 {
		inner = 3
	}
	if m.zen { // movie-night mode: one header row + chat + input, nothing else
		return cw, max(1, inner-2), false, false, false
	}
	fixed := 3 // two header lines + input
	showBar = m.duration > 0 && m.st != nil && m.st.Pos != nil && inner-fixed >= 6
	if showBar {
		fixed++
	}
	showSys = inner-fixed >= 6 && !m.sysOff // ctrl+g hides the system strip
	if showSys {
		fixed += 3
	}
	showFooter = inner-fixed >= 3
	if showFooter {
		fixed++
	}
	return cw, max(1, inner-fixed), showBar, showSys, showFooter
}

// layout is layoutCore minus the rows the open completion menu borrows:
// the menu floats above the input, its rows come out of the chat, or
// the frame grows past the terminal and the header scrolls off
// (A3-UI5 follow-up).
func (m Model) layout() (cw, vh int, showBar, showSys, showFooter bool) {
	if !m.zen && m.dashOn() { // dashboard: the chat box lives in the column
		_, r := m.dashGeom()
		return r - 2, m.chatBoxH(), false, false, false
	}
	cw, vh, showBar, showSys, showFooter = m.layoutCore()
	return cw, max(1, vh-m.menuRows()), showBar, showSys, showFooter
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
	nick := m.nick
	// the prompt must leave room for the input (10 cols minimum): a long
	// nickname would otherwise grow the row past the frame
	if budget := m.contentWidth() - 12; budget > 0 && lipgloss.Width(nick) > budget {
		nick = fitWidth(nick, budget)
	}
	return nickStyle(nick).Render(nick) + " › "
}

// fitWidth cuts a string to at most w visible columns, with an ellipsis.
func fitWidth(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r)+"…") > w {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

func (m Model) hasChat() bool {
	for _, e := range m.log {
		if e.kind == entChat {
			return true
		}
	}
	return false
}

// padChatBottom pads a short conversation with empty lines ABOVE the
// content: sparse chat used to sit at the top of the viewport, far from
// the input row. The viewport still follows the bottom.
func (m Model) padChatBottom(content string) string {
	lines := strings.Split(content, "\n")
	if len(lines) >= m.vp.Height {
		return content
	}
	return strings.Repeat("\n", m.vp.Height-len(lines)) + content
}

// inviteLines is the empty-chat placeholder: the one thing a first-time
// host needs is the address to send the other side. The server's hello
// carries its own reachable address (and optionally a second one); our
// API base is the fallback. Two addresses are labeled by their network.
func (m Model) inviteLines() string {
	var addrs []string
	if m.invite != "" {
		addrs = append(addrs, m.invite)
	}
	if m.inviteAlt != "" {
		addrs = append(addrs, m.inviteAlt)
	}
	if len(addrs) == 0 {
		if m.api != nil && m.api.Base != "" {
			addrs = append(addrs, m.api.Base)
		} else {
			addrs = append(addrs, "http://<server>")
		}
	}
	out := dim.Render("no messages yet — invite them:")
	for _, a := range addrs {
		line := title.Render("  " + a)
		if len(addrs) > 1 {
			if label := inviteLabel(a); label != "" {
				line += " " + dim.Render(label)
			}
		}
		out += "\n" + line
	}
	return out + "\n" +
		dim.Render("(everything you type here goes to the room as chat)")
}

// inviteLabel names an invite address by its network: the tailnet
// (100.64/10) or a real home LAN address. Anything else (a public IP, a
// hostname, the docker range) stays unlabelled — "local lan" would be a
// lie there (m2-S5).
func inviteLabel(addr string) string {
	ip := net.ParseIP(inviteHost(addr))
	if ip == nil {
		return "" // a hostname: no network claim
	}
	if isTailnetIP(ip) {
		return "(tailscale)"
	}
	if isLocalLANIP(ip) {
		return "(local lan)"
	}
	return "" // a public address: no claim
}

// isLocalLANIP is a real home LAN address: 10/8, 192.168/16 or
// loopback. 172.16/12 is deliberately NOT here: it is the docker range
// (net.IP.IsPrivate would claim it as a private LAN).
func isLocalLANIP(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return ip.IsLoopback() // ::1
	}
	switch {
	case v4[0] == 10:
		return true
	case v4[0] == 192 && v4[1] == 168:
		return true
	case v4[0] == 127:
		return true
	}
	return false
}

// inviteHost is the host part of an invite address (no scheme, no port).
func inviteHost(addr string) string {
	host := addr
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	if i := strings.Index(host, ":"); i >= 0 {
		host = host[:i]
	}
	return host
}

// isTailnetIP is the Tailscale CGNAT range 100.64.0.0/10.
func isTailnetIP(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
}

// dashOn reports whether the two-column dashboard fits.
func (m Model) dashOn() bool { return m.width >= dashMin && m.height >= dashMinH }

// dashGeom returns the two column widths (left: fixed-ish panels, right:
// chat + input) with a single space between them.
func (m Model) dashGeom() (leftW, rightW int) {
	leftW = min(40, m.width/3)
	return leftW, m.width - leftW - 1
}

// chatWidth is where conversation wraps: the right column's inner width
// in dashboard mode, the frame width otherwise.
func (m Model) chatWidth() int {
	if !m.zen && m.dashOn() {
		_, r := m.dashGeom()
		return r - 2
	}
	return m.contentWidth()
}

// sysWidth is where system lines wrap: the left column's box in dashboard
// mode, the frame width otherwise.
func (m Model) sysWidth() int {
	if !m.zen && m.dashOn() {
		l, _ := m.dashGeom()
		return l - 4
	}
	return m.contentWidth()
}

// padCut pads a (possibly styled) string to exactly w visible columns,
// hard-truncating from the right when over-wide (wrapped lines fit, so
// the truncation path only hits pathological unbreakables).
func padCut(s string, w int) string {
	for lipgloss.Width(s) > w {
		_, sz := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-sz]
	}
	return s + strings.Repeat(" ", max(0, w-lipgloss.Width(s)))
}

// box renders a mock-style titled panel with a colored border:
// `╭─ TITLE ─╮` top, `│` sides. An optional right label is aligned to
// the right of the top border: `╭─ TITLE ──── right ─╮`.
func box(w int, title, right string, lines []string, border lipgloss.Style) []string {
	t := "╭─ " + title + " "
	fill := w - lipgloss.Width(t) - 1
	top := t + strings.Repeat("─", max(1, fill))
	if right != "" {
		r := " " + right + " ─"
		if fill-lipgloss.Width(r) >= 1 {
			top = t + strings.Repeat("─", fill-lipgloss.Width(r)) + r
		}
		// no room for the label: drop it — a border wider than the box
		// would break every layout that uses it
	}
	out := []string{border.Render(top + "╮")}
	for _, l := range lines {
		out = append(out, border.Render("│")+" "+padCut(l, w-4)+" "+border.Render("│"))
	}
	return append(out, border.Render("╰"+strings.Repeat("─", max(1, w-2))+"╯"))
}

// stepEq advances the vibe equalizer: a lively walk while the room plays,
// frozen while paused (a decaying spectrum would claim the room is still
// playing), flat when nothing is loaded.
func (m Model) stepEq() Model {
	if len(m.eq) == 0 {
		m.eq = make([]float64, eqBars)
		for i := range m.eq {
			m.eq[i] = 0.1
		}
	}
	live := m.st != nil && m.st.Pos != nil && !m.st.Paused
	for i := range m.eq {
		switch {
		case live:
			m.eq[i] = min(1, max(0.08, m.eq[i]+(rand.Float64()-0.5)*0.55))
		case m.st != nil && m.st.Pos != nil: // paused: the bars freeze
		default: // nothing playing: flatline
			m.eq[i] = 0.04
		}
	}
	return m
}

// vibeLabel labels the equalizer with real state only: the local volume
// and the channel layout while playing, the frozen state while paused,
// nothing when idle (no state = no claim). The layout is only named when
// mpv told us ("stereo"); an unknown layout stays unsaid.
func (m Model) vibeLabel() string {
	if m.st == nil || m.st.Pos == nil {
		return ""
	}
	if m.st.Paused {
		return "⏸ paused"
	}
	if m.st.Volume <= 0 {
		return ""
	}
	label := fmt.Sprintf("♪ vol %.0f%%", m.st.Volume)
	if m.st.Layout != "" {
		label += " · " + m.st.Layout
	}
	return label
}

// vibePanel renders the spectrum-style equalizer: paired-width columns,
// eight rows tall, one rainbow hue per column (decorative — never a state
// signal). Live while playing (stepEq walk).
func (m Model) vibePanel(w int) []string {
	if len(m.eq) == 0 {
		m = m.stepEq()
	}
	const rows = 8
	cols := max(4, (w+1)/3)                              // "██ " per column
	pad := strings.Repeat(" ", max(0, (w-(cols*3-1))/2)) // center: no dead gap on the right
	heights := make([]int, cols)
	for i := range heights {
		v := 0.04
		if i < len(m.eq) {
			v = m.eq[i]
		}
		heights[i] = max(1, int(v*float64(rows)*0.99)) // never an empty column: a flatline must still be visible
	}
	paused := m.st != nil && m.st.Pos != nil && m.st.Paused
	lines := make([]string, rows) // top row first
	for lvl := rows; lvl >= 1; lvl-- {
		var b strings.Builder
		for _, h := range heights {
			col := "2"  // the progress bar's green: one playback color, not two
			tip := "10" // bright tips on the top two rows
			if paused {
				col, tip = "3", "11" // amber: the pause color of the project
			}
			if lvl >= 7 {
				col = tip
			}
			if h >= lvl {
				b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(col)).Render("██"))
			} else {
				b.WriteString("  ")
			}
			b.WriteString(" ")
		}
		lines[rows-lvl] = pad + strings.TrimRight(b.String(), " ")
	}
	return lines
}

func (m Model) nowPlayingLines(w int) []string {
	name := m.now
	if name == "" {
		name = "— nothing playing"
	}
	icon, style := "▶", playingStyle
	if m.roomPaused() {
		icon, style = "⏸", pausedStyle
	}
	bar := m.progressBar(w)
	if bar == "" {
		bar = dim.Render("no position yet")
	}
	// stat row: one hue per fact, icons ride the segment color. Drift stays
	// state-colored (green/amber/red) — the one genuinely semantic signal.
	var stats []string
	if m.st != nil && m.st.Pos != nil {
		if m.master {
			clk := lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
			stats = append(stats, clk.Render("◷ clock: master (reference)"))
		} else {
			ds := lipgloss.NewStyle().Foreground(lipgloss.Color(driftColor(m.st.Drift)))
			stats = append(stats, ds.Render(fmt.Sprintf("≈ drift %+.1fs", m.st.Drift)))
		}
		if m.st.Cache > 0 {
			bs := lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
			if m.st.Cache < 2 {
				bs = pausedStyle // starved: warn amber
			}
			stats = append(stats, bs.Render(fmt.Sprintf("≡ buf %.1fs", m.st.Cache)))
		}
		if m.st.Volume > 0 {
			vs := lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
			stats = append(stats, vs.Render(fmt.Sprintf("♪ you: vol %.0f%%", m.st.Volume)))
		}
		if m.st.Muted { // mpv reports the mute flag: the volume number alone would lie
			stats = append(stats, pausedStyle.Render("♪ muted"))
		}
		if m.roomPaused() {
			chip := "⏸ paused"
			switch {
			case m.st.Mechanical:
				// a stall on the other side's link: nobody pressed
				// anything, so the chip must not blame a human (m2-S2)
				chip += " " + stallMark
			case m.st.PausedBy != "":
				chip += " by " + m.st.PausedBy // the room named the actor
			}
			stats = append(stats, pausedStyle.Render(chip))
		} else {
			stats = append(stats, playingStyle.Render("▶ playing"))
		}
		if m.st.Sub != "" {
			// PERSONAL: the subtitle track is local (never posted, never
			// synced) — it only tells the viewer what is on screen
			ss := lipgloss.NewStyle().Foreground(lipgloss.Color("45"))
			stats = append(stats, ss.Render(fmt.Sprintf("⊞ you: sub %s [j in mpv]", m.st.Sub)))
		}
		if m.st.Stale { // no heartbeat from the room: say so instead of pretending
			// (neutral wording: our own link may be the broken one — A5 W9)
			stats = append(stats, pausedStyle.Render("⚠ no room heartbeat"))
		}
	}
	// a blank row above and below the bar: the position reads calmer
	return []string{style.Render(icon) + " " + name, "", bar, "", strings.Join(stats, statLbl.Render(" · "))}
}

// roomPaused: the ROOM is paused — by a human, or by a stall we detected
// (the mechanical flag). Our own player can still be running for a tick
// (a stall pause reaches us a moment later), so the state chips follow
// the ROOM, not our local player (A5 W10: the UI claimed "playing" for
// two seconds while the room was already paused).
func (m Model) roomPaused() bool {
	return m.st != nil && (m.st.Paused || m.st.Mechanical)
}

func (m Model) watcherLines() []string {
	role := "you"
	if m.master {
		role = "you · master"
	}
	out := []string{"• " + nickStyle(m.nick).Render(m.nick) + dim.Render(" ("+role+")")}
	if len(m.peers) == 0 {
		out = append(out, dim.Render("○ waiting for follower…"))
	}
	for _, p := range m.sortedPeers() {
		out = append(out, "• "+nickStyle(p).Render(p))
	}
	for _, g := range m.gone {
		out = append(out, dim.Render("• "+g+" (offline)"))
	}
	return out
}

func (m Model) sortedPeers() []string {
	out := make([]string, 0, len(m.peers))
	for p := range m.peers {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// queueLines renders the queue box contents, at most n rows: a long queue
// is truncated and the last row says how many files are hidden (the box
// must never grow past its row budget — the whole dashboard would scroll
// the header off the terminal). At least one file name always survives:
// the overflow hint is not worth an empty box.
func (m Model) queueLines(n int) []string {
	if n <= 0 {
		return nil
	}
	if len(m.queue) == 0 {
		return []string{dim.Render("empty — ") + keyStyle.Render("/queue <file>") + dim.Render(" to add")}
	}
	shown := len(m.queue)
	if shown > n {
		shown = max(1, n-1) // the last row carries the overflow hint
	}
	out := make([]string, 0, n)
	for i := 0; i < shown; i++ {
		out = append(out, fmt.Sprintf("%d. %s", i+1, m.queue[i]))
	}
	if shown < len(m.queue) {
		out = append(out, dim.Render(fmt.Sprintf("… +%d more", len(m.queue)-shown)))
	}
	return out
}

// padLines grows a slice with empty rows so its box has a stable height.
func padLines(lines []string, n int) []string {
	for len(lines) < n {
		lines = append(lines, "")
	}
	return lines
}

// dashView assembles the two-column dashboard: a full-width NOW PLAYING
// band under the header, then WATCHERS/QUEUE/SYSTEM on the left and the
// conversation with the input box on the right. A hotkey bar closes.
// chatBase is the dashboard chat box height without the menu: the rows
// the left column leaves to the conversation.
func (m Model) chatBase() int {
	return max(4, m.height-3-7-m.vibeH()-3-2)
}

// chatBoxH is the dashboard chat box height: the base minus the rows
// the open menu borrows (the chat never drops below 4 rows).
func (m Model) chatBoxH() int {
	return max(4, m.chatBase()-m.menuBudget())
}

func (m Model) dashView() string {
	leftW, rightW := m.dashGeom()
	avail := m.height - 3 // header + gap + hotbar
	chatH := m.chatBoxH()
	vp := m.vp
	vp.Width, vp.Height = rightW-2, chatH

	budget := avail - 7 // left-column rows under the band
	watH := 2 + len(m.watcherLines())
	// the budget gives the SYSTEM tail its lines first (it is the panel
	// that reads), but the boxes are drawn top down: WATCHERS, QUEUE,
	// SYSTEM. The QUEUE keeps a 4-row floor.
	queueFloor := 4
	sysH := 0
	if !m.sysOff {
		if n := min(sysRows, budget-watH-2-queueFloor); n > 0 {
			sysH = n + 2
		}
	}
	if budget-watH-sysH < queueFloor {
		watH = 2 // cramped: the watchers shrink, the queue floor wins
	}
	queueH := max(queueFloor, budget-watH-sysH) // stretches to fill the left column

	// the band's border breathes with playback: green, amber, idle grey
	bord := bordIdle
	if m.st != nil && m.st.Pos != nil {
		if m.roomPaused() {
			bord = bordPause
		} else {
			bord = bordPlay
		}
	}
	bandTitle := "NOW PLAYING"
	if m.st != nil && m.st.Pos != nil && m.roomPaused() {
		bandTitle += " · PAUSED"
	}
	bandHint := ""
	if m.duration > 0 && m.st != nil && m.st.Pos != nil && m.mode != modePick {
		// the picker owns the keys: the bar cannot be clicked and the
		// ctrl nudge is dead there, so the hint must not promise them (F4)
		bandHint = "click bar or ctrl+←/→ to seek" // there is a bar to click
	}
	band := box(m.width, bandTitle, bandHint, m.nowPlayingLines(m.width-4), bord)
	var left []string
	if watH > 2 {
		left = append(left, box(leftW, fmt.Sprintf("WATCHERS (%d)", len(m.peers)+1), "", m.watcherLines(), bordWatch)...)
	}
	qBord := bordQueue // vivid pink: content
	if len(m.queue) == 0 {
		qBord = bordQueueIdle // muted orchid: same family, quieter than grey
	}
	queueTitle := fmt.Sprintf("QUEUE (%d)", len(m.queue))
	if len(m.queue) > 0 {
		queueTitle += " · /skip next" // the hint lives in the title, not in the rows
	}
	left = append(left, box(leftW, queueTitle, "", padLines(m.queueLines(queueH-2), queueH-2), qBord)...)
	if sysH > 0 {
		sys := m.renderSysLinesAt(leftW - 4)
		shown := len(sys)
		if shown > sysH-2 {
			sys = sys[shown-(sysH-2):] // a glanceable tail, never a reading surface
			shown = sysH - 2
		}
		for len(sys) < sysH-2 {
			sys = append(sys, "")
		}
		left = append(left, box(leftW, "SYSTEM", fmt.Sprintf("%d-line tail", shown), sys, bordSys)...)
	}

	in := strings.TrimRight(m.input.View(), " ")
	hint := ""
	if m.lastErr != "" {
		hint = errStyle.Render(m.lastErr)
	}
	msg := []string{joinLR(in, hint, rightW-4)}
	msg = append(msg, m.menuLines(rightW-4)...) // Tab completion menu, when open
	right := box(rightW, "CHAT", "", strings.Split(vp.View(), "\n"), bordChat)
	if m.vibeH() > 0 {
		// the equalizer border follows the state, like the band's: the
		// paused amber is the same one the bars wear
		vBord := bordIdle
		if m.st != nil && m.st.Pos != nil {
			if m.st.Paused {
				vBord = bordPause
			} else {
				vBord = bordVibe
			}
		}
		right = append(box(rightW, "EQUALIZER", m.vibeLabel(), m.vibePanel(rightW-4), vBord), right...)
	}
	right = append(right, box(rightW, "MESSAGE", "", msg, bordMsg)...)

	role := "follower"
	if m.master {
		role = "master"
	}
	header := joinLR(title.Render("▷ WATCHPARTY ")+roleStyle[role].Render(strings.ToUpper(role)), m.connChip(), m.width)
	// the hotbar echoes the panel hues: each shortcut wears the color of
	// what it controls (green=playback, cyan=seek, purple=menu, amber=movie,
	// orange=system pane)
	hue := func(c string) lipgloss.Style { return lipgloss.NewStyle().Foreground(lipgloss.Color(c)) }
	verb := "pause" // contextual: the key does what the room needs now
	if m.st != nil && m.st.Paused {
		verb = "play"
	}
	hotbar :=
		keyStyle.Render("[ctrl+space]") + hue("2").Render(" "+verb+" · ") +
			keyStyle.Render("[ctrl+←/→]") + hue("6").Render(fmt.Sprintf(" ±%.0fs · ", seekStep)) +
			keyStyle.Render("[ctrl+p]") + hue("141").Render(" files · ") +
			keyStyle.Render("[tab]") + hue("141").Render(" menu · ") +
			keyStyle.Render("[ctrl+z]") + hue("3").Render(" movie · ") +
			keyStyle.Render("[ctrl+g]") + hue("208").Render(" system")
	if m.mode == modePick {
		// the picker owns the keys: the hotbar's shortcuts are dead
		// there, so the row must tell the truth instead (F4)
		hotbar = dim.Render(m.pickControls())
	}

	var b strings.Builder
	b.WriteString(header + "\n\n") // gap row: the header reads calmer off the band
	for _, l := range band {       // the now-playing band spans both columns
		b.WriteString(l + "\n")
	}
	rows := max(len(left), len(right))
	for i := 0; i < rows; i++ {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		} else if i < len(right) {
			// left column spent: keep the right column in its lane
			l = strings.Repeat(" ", leftW)
		}
		if i < len(right) {
			r = right[i]
		}
		b.WriteString(l + " " + padCut(r, rightW) + "\n")
	}
	_ = avail
	b.WriteString(hotbar) // no trailing \n: it would render one row past the viewport and scroll the header off
	return b.String()
}

// menuLines renders the open completion menu: selected row reversed,
// descriptions dim, windowed to menuMax rows around the cursor so the
// box never grows past the terminal. Empty when closed.
const menuMax = 16

func (m Model) menuLines(w int) []string {
	budget := m.menuBudget()
	if !m.compOpen || len(m.compItems) == 0 || budget <= 0 {
		return nil
	}
	lo := max(0, min(m.compCursor-(budget-1)/2, len(m.compItems)-budget))
	hi := min(len(m.compItems), lo+budget)
	out := make([]string, 0, budget)
	for i := lo; i < hi; i++ {
		it := m.compItems[i]
		row := "   " + it.text
		if i == m.compCursor {
			row = pickCur.Render(" ▸ " + it.text + " ")
		}
		if it.desc != "" {
			row += dim.Render("  " + it.desc)
		}
		out = append(out, padCut(row, w))
	}
	if hi < len(m.compItems) && len(out) > 0 { // indicator stays inside the budget
		out[len(out)-1] = dim.Render(padCut(fmt.Sprintf("   … +%d more (↓)", len(m.compItems)-hi+1), w))
	}
	return out
}

// vibeH: the equalizer drops out on short screens — chat survives first.
func (m Model) vibeH() int {
	if m.height < 29 || m.zen {
		return 0
	}
	return 10
}

// menuRows is the raw height the open menu wants (the more-indicator
// replaces the last window row when truncated, so it needs no extra row).
func (m Model) menuRows() int {
	if !m.compOpen {
		return 0
	}
	return min(len(m.compItems), menuMax)
}

// menuBudget caps what the menu may borrow: the chat never drops below
// 4 rows. The budget comes from the CURRENT layout — the stacked and zen
// layouts have no equalizer, so their chat is far taller than the
// dashboard formula assumed (the candidates used to be squeezed into a
// single row and never shown).
func (m Model) menuBudget() int {
	_, vh, _, _, _ := m.layoutCore()
	return min(m.menuRows(), max(0, vh-4))
}

func (m Model) View() string {
	if m.height == 0 {
		return m.spin.View() + " connecting…\n"
	}
	if !m.zen && m.dashOn() {
		return m.dashView()
	}
	cw, vh, showBar, showSys, showFooter := m.layout()
	vp := m.vp // sized copies: View must not depend on Update having run first
	vp.Width, vp.Height = cw, vh
	sysVp := m.sysVp
	sysVp.Width, sysVp.Height = cw, 2

	var b strings.Builder
	l1, l2 := m.headerLines(cw)
	b.WriteString(l1 + "\n")
	if !m.zen {
		b.WriteString(l2 + "\n")
		if showBar {
			b.WriteString(m.progressBar(cw) + "\n")
		}
	}
	b.WriteString(vp.View() + "\n")
	for _, l := range m.menuLines(cw) { // stacked layout: the menu floats above the input
		b.WriteString(l + "\n")
	}
	if !m.zen {
		if showSys {
			b.WriteString(m.sysDivider(cw) + "\n")
			b.WriteString(sysVp.View() + "\n")
		}
		if showFooter {
			b.WriteString(m.footerLine(cw) + "\n")
		}
	}
	// textinput pads its view to the full input width; the padding would
	// eat the whole gap in joinLR, so measure and render without it
	in := strings.TrimRight(m.input.View(), " ")
	b.WriteString(joinLR(in, m.inputSideHint(vp), cw))
	return frame.Render(b.String())
}

// sysDivider separates the chat pane from the system tail.
func (m Model) sysDivider(cw int) string {
	label := dim.Render("─ system ")
	return label + dim.Render(strings.Repeat("─", max(0, cw-lipgloss.Width(label))))
}

// inputSideHint picks the right-hand input row hint: the last error wins
// (red, truncated to fit), then the scrollback marker. The used width is
// computed from the actual content: textinput.View() pads to its full
// Width, which would leave no room for the hint.
func (m Model) inputSideHint(vp viewport.Model) string {
	if m.lastErr != "" {
		used := lipgloss.Width(m.prompt()) + lipgloss.Width(m.input.Value())
		if m.input.Value() == "" {
			used += lipgloss.Width(m.input.Placeholder)
		}
		gap := m.contentWidth() - used - 2
		if gap < 8 {
			return "" // no room: the error stays in the system log
		}
		s, cut := m.lastErr, false
		for lipgloss.Width(s) > gap {
			_, sz := utf8.DecodeLastRuneInString(s)
			s = s[:len(s)-sz]
			cut = true
		}
		if cut {
			s += "…"
		}
		return errStyle.Render(s)
	}
	if !vp.AtBottom() {
		return dim.Render("↑ more")
	}
	return ""
}

// renderChatLines renders the human conversation: bright, word-wrapped,
// never mixed with machine output. Consecutive messages from the same
// nick within five minutes group under one timestamp + nickname.
func (m Model) renderChatLines() []string { return m.renderChatLinesAt(m.chatWidth()) }

func (m Model) renderChatLinesAt(cw int) []string {
	var out []string
	prevIdx := -1
	faded := false // the group's head decides: a continuation inherits it
	for i, e := range m.log {
		if e.kind != entChat {
			continue
		}
		cont := prevIdx >= 0 && m.log[prevIdx].nick == e.nick &&
			e.at.Sub(m.log[prevIdx].at) < 5*time.Minute
		if !cont {
			faded = m.zen && time.Since(e.at) > zenDimAfter
		}
		out = append(out, m.renderEntry(e, cw, cont, faded)...)
		prevIdx = i
	}
	return out
}

// renderSysLines renders events and engine lines dim; consecutive same-type
// engine lines collapse to their last line plus a hidden count, so sync
// chatter stays a compact tail instead of burying the conversation.
func (m Model) renderSysLines() []string { return m.renderSysLinesAt(m.sysWidth()) }

func (m Model) renderSysLinesAt(cw int) []string {
	var out []string
	for i := 0; i < len(m.log); i++ {
		e := m.log[i]
		if e.kind == entChat {
			continue // not system noise, but still breaks an engine run
		}
		if e.kind != entEngine {
			out = append(out, m.renderEntry(e, cw, false, false)...)
			continue
		}
		j := i
		for j+1 < len(m.log) && m.log[j+1].kind == entEngine &&
			engineKind(m.log[j+1].text) == engineKind(e.text) {
			j++
		}
		lines := m.renderEntry(m.log[j], cw, false, false)
		if j > i {
			lines[len(lines)-1] += dim.Render(fmt.Sprintf("  (%d)", j-i))
		}
		out = append(out, lines...)
		i = j
	}
	for k, l := range out { // thin gutter: system reads as a sidebar, chat pops
		out[k] = gutStyle.Render("▏") + l
	}
	return out
}

// sysIcon maps a system-line's first token to an icon + color, shared by
// engine lines and room events: the tail reads at a glance.
func sysIcon(body string) (string, lipgloss.Style) {
	tok := body
	if i := strings.IndexAny(tok, " 	"); i >= 0 {
		tok = tok[:i]
	}
	switch {
	case tok == "playing" || tok == "load":
		return "▶ ", loadStyle
	case tok == "pause":
		return "⏸ ", pausedStyle
	case tok == "play":
		return "▶ ", playingStyle
	case tok == "seek":
		return "⇄ ", seekStyle
	case tok == "err:":
		return "✗ ", errStyle
	case tok == "queued":
		return "+ ", bordQueue
	case tok == "queue:":
		return "≡ ", bordQueue
	case strings.HasSuffix(body, " joined"):
		return "→ ", joinStyle
	case strings.HasSuffix(body, " left"):
		return "← ", leftStyle
	case strings.HasPrefix(body, "two masters"):
		return "! ", noticeStyle
	case strings.HasPrefix(body, "[conn]"):
		return "⌁ ", noticeStyle
	case tok == "room:":
		return "⌂ ", dim
	}
	return "· ", dim
}

func (m Model) renderEntry(e entry, cw int, cont, faded bool) []string {
	switch e.kind {
	case entChat:
		// first line: HH:MM + nick; grouped lines keep the message column but
		// swap time+nick for a speaker-colored anchor (▏) — attribution without noise
		// zen only: a message older than zenDimAfter fades into the calm
		// surface (the group head decides, a continuation inherits it).
		// The whole line is dimmed, so the prefix carries no other color
		// (an inner reset would end the dim mid-line).
		var prefix string
		if faded {
			prefix = e.at.Format("15:04") + " " + e.nick + " "
			if cont {
				prefix = strings.Repeat(" ", 6) + " " + "▏" +
					strings.Repeat(" ", lipgloss.Width(e.nick)-1) + " "
			}
		} else {
			who := nickStyle(e.nick).Render(e.nick)
			prefix = timeStyle.Render(e.at.Format("15:04")) + " " + who + " "
			if cont {
				prefix = strings.Repeat(" ", 6) + " " +
					nickStyle(e.nick).Render("▏") + strings.Repeat(" ", lipgloss.Width(e.nick)-1) + " "
			}
		}
		lines := wrapIndented(e.text, cw-lipgloss.Width(prefix)-1, lipgloss.Width(prefix)+1, prefix)
		if faded {
			for i := range lines {
				lines[i] = dim.Render(lines[i])
			}
		}
		return lines
	case entEngine:
		body := strings.TrimPrefix(e.text, "> ")
		icon, style := sysIcon(body)
		lines := wrapIndented(icon+body, cw, lipgloss.Width(icon), "")
		for i, l := range lines {
			lines[i] = style.Render(l)
		}
		return lines
	default: // entEvent: icon-prefixed, colored by kind
		body := strings.TrimPrefix(e.text, "> ")
		icon, style := sysIcon(body)
		lines := wrapIndented(icon+body, cw, lipgloss.Width(icon), "")
		for i, l := range lines {
			lines[i] = style.Render(l)
		}
		return lines
	}
}

// engineKind is the collapse key: the first token after the ">" prompt, so
// "> pause" and "> play" never merge (the last real state must stay visible)
// while repeats like "> seek @1.0s", "> seek @2.0s" collapse to the newest.
// An actor is part of the fact: "pause by bo" must not collapse into "pause".
func engineKind(text string) string {
	t := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), ">"))
	f := strings.Fields(t)
	if len(f) == 0 {
		return ""
	}
	if len(f) >= 2 && f[1] == stallMark {
		return f[0] + " " + stallMark // a stall pause is its own fact
	}
	if len(f) >= 3 && f[1] == "by" {
		return f[0] + " by " + f[2]
	}
	return f[0]
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

// pickControls is the picker's honest control line: what the keys DO in
// the current mode (F4). The dashboard hotbar shows it while the picker
// is open — the hotbar's shortcuts are dead there.
func (m Model) pickControls() string {
	enter := "enter play"
	if m.pickAct == pickActQueue {
		enter = "enter enqueue"
	}
	return "up/down move · " + enter + " · q queue · esc cancel"
}

func (m Model) pickLines() []string {
	// The footer must say what the keys DO in this mode: the picker is
	// honest about its controls (F4).
	head := "pick a film"
	if m.pickAct == pickActQueue {
		head = "queue a film"
	}
	out := []string{title.Render(head) + dim.Render("  "+m.pickControls())}
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
// now-playing + clock. Movie-night mode collapses them into one row.
func (m Model) headerLines(cw int) (string, string) {
	role := "follower"
	if m.master {
		role = "master"
	}
	if m.zen {
		return joinLR(m.nowChip(cw-lipgloss.Width(m.zenSyncChip())-1), m.zenSyncChip(), cw), ""
	}
	l1 := joinLR(roleStyle[role].Render("watchparty "+role), m.connChip(), cw)
	l2 := joinLR(m.nowChip(cw-lipgloss.Width(m.clockChip())-1), m.clockChip(), cw)
	return l1, l2
}

func (m Model) connChip() string {
	switch m.conn {
	case "connected":
		// the count includes yourself: "alone" must never read as "someone is here"
		w := "alone"
		if n := len(m.peers); n > 0 {
			w = fmt.Sprintf("you + %d", n)
		}
		up := ""
		if !m.connSince.IsZero() {
			up = " · up " + humanDur(time.Since(m.connSince))
		}
		return connStyle["connected"].Render("● connected · "+w) + dim.Render(up)
	case "reconnecting":
		return connStyle["reconnecting"].Render(fmt.Sprintf("○ retry %d · backoff %ds", m.retry, m.retryDelay))
	case "lost":
		return connStyle["lost"].Render("● lost")
	default:
		return dim.Render("○ connecting")
	}
}

func (m Model) nowChip(budget int) string {
	if m.now == "" {
		if m.st != nil && m.st.Pos != nil {
			return dim.Render("—") // playing, but the title is not known yet
		}
		return dim.Render("— nothing playing")
	}
	icon, style := "▶", playingStyle
	if m.roomPaused() {
		icon, style = "⏸", pausedStyle
	}
	// the title must never grow the row past the frame: a long file name
	// would push the header off the terminal
	return style.Render(icon) + " " + fitWidth(m.now, budget-2)
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

// zenSyncChip is the movie-night header's right side: time left in the
// film plus the drift chip (state-colored — the one semantic signal).
// The file itself is the left side of that row.
func (m Model) zenSyncChip() string {
	if m.st == nil || m.st.Pos == nil {
		return ""
	}
	var parts []string
	if m.duration > 0 {
		parts = append(parts, dim.Render("-"+clockStr(max(0, m.duration-*m.st.Pos))+" left"))
	}
	if m.st.Sub != "" { // personal: the local subtitle track
		parts = append(parts, lipgloss.NewStyle().Foreground(lipgloss.Color("45")).Render("⊞ "+m.st.Sub))
	}
	if m.master {
		parts = append(parts, lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Render("◷ master clock"))
	} else {
		parts = append(parts, lipgloss.NewStyle().Foreground(lipgloss.Color(driftColor(m.st.Drift))).
			Render(fmt.Sprintf("≈ %+.1fs", m.st.Drift)))
	}
	return strings.Join(parts, dim.Render(" · "))
}

// progressBar renders the seek bar with a position head and time labels
// at both ends; green while playing, amber while paused so the state reads
// even in the corner of an eye.
func (m Model) progressBar(cw int) string {
	if m.duration <= 0 || m.st == nil || m.st.Pos == nil {
		return ""
	}
	frac := *m.st.Pos / m.duration
	frac = min(max(frac, 0), 1)
	elapsed, total := clockStr(*m.st.Pos), clockStr(m.duration)
	bw := cw - lipgloss.Width(elapsed) - lipgloss.Width(total) - 4
	bw = max(bw, 8)
	fill := int(frac * float64(bw))
	col := lipgloss.Color("2")
	if m.st.Paused {
		col = lipgloss.Color("3")
	}
	head := lipgloss.NewStyle().Foreground(col)
	rest := lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	bar := head.Render(strings.Repeat("█", fill)) +
		rest.Render(strings.Repeat("░", bw-fill))
	return timeStyle.Render(elapsed) + " " + bar + " " + timeStyle.Render(total)
}

// driftColor bands drift for the footer and the dashboard stat row, so the
// classic and dashboard layouts never disagree about what "bad" means.
func driftColor(d float64) string {
	switch {
	case d > 1 || d < -1:
		return "1" // hard re-seek territory
	case d > 0.15 || d < -0.15:
		return "3" // tempo-nudge territory
	default:
		return "2" // inside the smooth-sync band
	}
}

// footerLine: queue preview on the left, the live sync gauge on the right.
func (m Model) footerLine(cw int) string {
	right := ""
	if m.master {
		right = dim.Render(fmt.Sprintf("%d listening", len(m.peers)))
	} else if m.st != nil && m.st.Pos != nil {
		right = lipgloss.NewStyle().Foreground(lipgloss.Color(driftColor(m.st.Drift))).
			Render(fmt.Sprintf("drift %+.1fs", m.st.Drift))
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
	if d < 0 {
		d = 0 // a backwards clock step must not print a negative duration
	}
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

// queueStr renders a queue list for a log line: the first three names
// plus a count. A 25-file queue would otherwise wrap over a dozen lines
// and swallow the whole system tail.
func queueStr(q []string) string {
	if len(q) <= 3 {
		return "[" + strings.Join(q, ", ") + "]"
	}
	return "[" + strings.Join(q[:3], ", ") + fmt.Sprintf(" (+%d)", len(q)-3) + "]"
}
