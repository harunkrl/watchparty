// Package mpv spawns and drives an external mpv process over its JSON IPC
// (unix socket / windows named pipe), mirroring client.py's Player/Ipc.
// A dedicated reader goroutine routes command replies by request_id and
// forwards mpv events (pause/unpause/seek) for the instant sync path.
package mpv

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// subClient caps the sidecar subtitle probe; matches client.py's timeout=5.
var subClient = &http.Client{Timeout: 5 * time.Second}

// Fixed numbers from the plan: aggressive LAN readahead so upload hiccups
// never stall playback (user-overridable via WATCHPARTY_MPV).
const (
	demuxerReadaheadSecs = 300
	demuxerMaxBytes      = "2GiB"
)

// Bounds on the IPC layer. Vars, not consts, so tests can shrink them.
var (
	cmdTimeout = 5 * time.Second // one IPC round trip
	LoadWait   = 5 * time.Second // how long WaitLive waits for the file to go live (F6-A1)
)

// State is one poll sample of the player, consumed by the sync engine.
type State struct {
	Pos       *float64 // nil: no time-pos (nothing loaded or idle)
	Pause     bool
	Idle      bool
	Buffering bool      // cache underrun: mpv stalled playback to refill (paused-for-cache)
	Cache     float64   // demuxer-cache-time: seconds of read-ahead buffered
	Volume    float64   // audio volume, percent (display only; mpv window owns the keys)
	Muted     bool      // mpv mute flag (display only)
	Dur       float64   // media duration in seconds (0 = unknown)
	Exited    bool      // mpv window was closed; client should shut down
	At        time.Time // when the sample was taken (a sample older than a load is stale)
	Partial   bool      // the property batch was truncated: the values are incomplete
	Sub       string    // language of the active subtitle track ("" = none/unknown)
	Layout    string    // audio channel layout as mpv names it ("stereo", "5.1"; "" = unknown)
}

// pollProps is what one poll tick reads. They travel as ONE IPC round
// trip (see batchGet): eight separate get_property calls would be eight
// sequential request/response pairs.
var pollProps = []string{
	"pause", "idle-active", "paused-for-cache", "time-pos",
	"demuxer-cache-time", "volume", "mute", "duration",
	"audio-params", "track-list",
}

// Event is one pushed mpv notification (Name is the mpv event id, e.g.
// "pause", "unpause", "seek").
type Event struct {
	Name string
}

type Player struct {
	base   string
	proc   *exec.Cmd
	conn   net.Conn
	dir    string        // temp dir holding the socket (removed on Close)
	drops  int           // logged event drops (bounded; a silent drop hid user seeks for a day)
	exited chan struct{} // closed when the mpv process is reaped
	dead   chan struct{} // closed when the IPC connection dies with mpv still alive
	closed bool          // Close() ran: a dead conn is expected, not a crash
	events chan Event
	// Logf receives player diagnostics. It must not be stderr: in the TUI
	// the alt-screen frame is owned by the renderer and a stray stderr line
	// corrupts it (the engine's log goes through the program for the same
	// reason). Use SetLogf to install it: the mpv stderr pump reads it
	// concurrently.
	Logf     func(format string, args ...any)
	mu       sync.Mutex // guards pend, seq, drops, Logf and writes
	pend     map[int]chan ipcMsg
	seq      int
	stderrCh chan string // mpv's stderr lines, bounded (the ring)
	// closeOnce makes Close idempotent; closeDone makes a second caller
	// WAIT for the first one: the process must not exit while mpv is
	// still being reaped (an orphan mpv is worse than a slow exit).
	closeOnce sync.Once
	closeDone chan struct{}
}

// stderrRingMax is how many mpv stderr lines we keep: mpv must never
// stall on our logging (a full stderr pipe would freeze the player), so
// the oldest line is dropped instead.
const stderrRingMax = 200

func Start(base string, extra []string) (*Player, error) {
	var dir, sock string
	if runtime.GOOS == "windows" {
		sock = ipcPipeName()
	} else {
		var err error
		dir, err = os.MkdirTemp("", "watchparty-") // unguessable, python parity
		if err != nil {
			return nil, err
		}
		sock = filepath.Join(dir, fmt.Sprintf("mpv-%d.sock", os.Getpid()))
	}
	args := mpvArgs(sock, extra)
	bin, err := findMPV()
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	cmd := exec.Command(bin, args...)
	// a relative hit warns about cwd resolution (Go 1.19+ ErrDot); we
	// deliberately accept it.
	if errors.Is(cmd.Err, exec.ErrDot) {
		cmd.Err = nil
	}
	exited := make(chan struct{})
	p := &Player{
		base: strings.TrimRight(base, "/"), proc: cmd, dir: dir, exited: exited,
		dead:     make(chan struct{}),
		events:   make(chan Event, 64),
		pend:     map[int]chan ipcMsg{},
		stderrCh: make(chan string, stderrRingMax),
	}
	// mpv's diagnostics go to the client's log, not into the void: the
	// friend must see WHY mpv failed (a missing DLL, a bad file).
	// MEASURED: mpv writes its messages to STDOUT when stdout is a pipe
	// (stderr stays empty), so BOTH streams are wired to the same writer.
	out := &stderrWriter{p: p}
	cmd.Stdout = out
	cmd.Stderr = out
	go p.drainStderr()
	// Stdin nil = os.DevNull: otherwise mpv steals the keyboard from the TUI
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("could not start mpv (%s): %w", bin, err)
	}
	go func() {
		_ = cmd.Wait()
		close(exited) // Poll reports this as State{Exited: true}
	}()
	for i := 0; i < 50; i++ {
		conn, err := dialPipe(sock)
		if err == nil {
			p.conn = conn
			// mpv pushes nothing to passive clients: subscribe to the
			// pause property so the master path hears space-bar presses
			// instantly (the reply carries request_id 0 and is dropped)
			_ = p.tell("observe_property", 1, "pause")
			go p.readLoop()
			return p, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.Close()
	return nil, fmt.Errorf("could not connect to mpv ipc")
}

// mpvQuiet keeps mpv's error messages (unlike --really-quiet, which
// silences everything) while --quiet still silences the status line.
// Measured: the playback chatter goes to STDOUT (the client discards
// it), the errors go to STDERR (the client forwards them).
const mpvQuiet = "--quiet"

// mpvArgs are the flags the client always passes to mpv. --quiet (not
// --really-quiet) keeps mpv's error messages: the friend must see WHY
// mpv failed. --quiet still silences the status line.
func mpvArgs(sock string, extra []string) []string {
	return append([]string{
		"--idle", "--force-window", mpvQuiet, "--title=Watchparty",
		"--input-ipc-server=" + sock,
		"--cache=yes",
		fmt.Sprintf("--demuxer-readahead-secs=%d", demuxerReadaheadSecs),
		"--demuxer-max-bytes=" + demuxerMaxBytes,
	}, extra...)
}

// ipcPipeName is the Windows named pipe the client and mpv talk over.
// The PID alone can collide: an orphaned mpv still holds the old name
// and Windows reuses PIDs — the random suffix kills that.
func ipcPipeName() string {
	return fmt.Sprintf(`\\.\pipe\watchparty-%d-%s`, os.Getpid(), randSuffix())
}

// randSuffix is a short random hex tag for the pipe name.
func randSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0"
	}
	return hex.EncodeToString(b[:])
}

// stderrWriter forwards mpv's diagnostics (stdout AND stderr) to the
// player's log. exec serializes the writes when both streams share one
// comparable writer, so the lines keep their order.
type stderrWriter struct{ p *Player }

func (w *stderrWriter) Write(b []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(b), "\r\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			w.p.pushStderr(line)
		}
	}
	return len(b), nil
}

// pushStderr queues one mpv stderr line WITHOUT blocking: mpv must not
// stall on our logging (a full stderr pipe would freeze the player). The
// buffer is bounded — the oldest line is dropped instead.
func (p *Player) pushStderr(line string) {
	select {
	case p.stderrCh <- line:
	default:
		select { // drop the oldest, keep the newest
		case <-p.stderrCh:
		default:
		}
		select {
		case p.stderrCh <- line:
		default:
		}
	}
}

// drainStderr forwards the queued mpv lines to the client log. Until the
// caller installs a logger the lines wait: mpv starts before the client
// can wire it, and a stray stderr line would corrupt the alt-screen
// frame.
func (p *Player) drainStderr() {
	for line := range p.stderrCh {
		p.logStderrNow(line)
	}
}

// logStderrNow logs one mpv line, waiting for the logger to be installed.
func (p *Player) logStderrNow(line string) {
	for {
		p.mu.Lock()
		f := p.Logf
		p.mu.Unlock()
		if f != nil {
			f("mpv: %s", line)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// SetLogf installs the diagnostics sink. Use it instead of writing the
// field: the mpv stderr pump reads the field concurrently.
func (p *Player) SetLogf(f func(format string, args ...any)) {
	p.mu.Lock()
	p.Logf = f
	p.mu.Unlock()
}

// mpvBinaryNames are the file names an mpv build can carry.
var mpvBinaryNames = []string{"mpv", "mpv.exe"}

// mpvExeDir is the folder that holds the executable: the release bundle
// ships its mpv there. A var so the tests can point it elsewhere.
var mpvExeDir = defaultMPVExeDir

func defaultMPVExeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

// findMPV locates the mpv binary: the bundle next to the executable
// first, then PATH. A release zip ships mpv in a subfolder (or beside
// the exe), and on Windows there is no PATH entry for it — without this
// the client would die with "could not start mpv". The bundle wins over
// PATH: it is the digest-pinned mpv, a stale system mpv must not shadow
// it.
func findMPV() (string, error) {
	looked := []string{"PATH"}
	if dir := mpvExeDir(); dir != "" {
		looked = []string{dir, filepath.Join(dir, "mpv*"), "PATH"}
		if p := findMPVIn(dir); p != "" {
			return p, nil
		}
	}
	var relative string
	if p, err := exec.LookPath("mpv"); err == nil {
		if !errors.Is(err, exec.ErrDot) {
			return p, nil
		}
		relative = p // "./mpv": the current folder happens to hold it
	}
	if relative != "" {
		return relative, nil
	}
	return "", fmt.Errorf("mpv not found (looked in %s)", strings.Join(looked, ", "))
}

// findMPVIn returns the mpv binary inside dir: a sibling first, then an
// mpv* subfolder (the bundle layout). "" when there is none.
func findMPVIn(dir string) string {
	for _, name := range mpvBinaryNames {
		if p := filepath.Join(dir, name); isRegularFile(p) {
			return p
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(strings.ToLower(e.Name()), "mpv") {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		for _, name := range mpvBinaryNames {
			if p := filepath.Join(sub, name); isRegularFile(p) {
				return p
			}
		}
	}
	return ""
}

func isRegularFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// Events exposes mpv's pushed notifications (never closed; drops when full).
func (p *Player) Events() <-chan Event { return p.events }

type ipcMsg struct {
	Data      json.RawMessage `json:"data"`
	Error     string          `json:"error"`
	RequestID int             `json:"request_id"`
	Event     string          `json:"event"`
	PropName  string          `json:"name"` // property-change payload
}

// eventName maps mpv notifications to engine event names: the engine
// speaks pause/unpause, not raw property changes.
func eventName(m ipcMsg) string {
	if m.Event == "property-change" && m.PropName == "pause" {
		if string(m.Data) == "true" {
			return "pause"
		}
		return "unpause"
	}
	return m.Event
}

// readLoop owns the connection: routes replies to their waiting cmd() call
// and forwards mpv events. On connection loss it fails all waiters.
func (p *Player) readLoop() {
	sc := bufio.NewScanner(p.conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var m ipcMsg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue // malformed line: skip, python parity
		}
		if m.RequestID != 0 {
			p.mu.Lock()
			ch, ok := p.pend[m.RequestID]
			delete(p.pend, m.RequestID)
			p.mu.Unlock()
			if ok {
				ch <- m // buffered(1); the waiter may have timed out and left
			}
			continue
		}
		if m.Event != "" {
			select {
			case p.events <- Event{Name: eventName(m)}:
			default:
				// a full buffer means the engine is stalled (e.g. a slow seek
				// settle under load); dropping keeps playback control alive —
				// but never silently again: silent drops hid user seeks for a
				// whole day of soak flake
				p.mu.Lock()
				dropped := p.drops < 3
				if dropped {
					p.drops++
				}
				p.mu.Unlock()
				if dropped {
					p.logf("mpv: event dropped (engine stalled?)")
				}
			}
		}
	}
	p.mu.Lock()
	if !p.closed && p.dead != nil {
		// the IPC died with mpv alive: every later command fails and the
		// client would freeze on a stale frame — report it instead
		close(p.dead)
	}
	for rid, ch := range p.pend {
		delete(p.pend, rid)
		ch <- ipcMsg{Error: "ipc closed"}
	}
	p.mu.Unlock()
}

// logf routes a diagnostic to the caller's logger. stderr would corrupt
// the TUI's alt-screen frame, so the caller must provide Logf.
func (p *Player) logf(format string, args ...any) {
	p.mu.Lock()
	f := p.Logf
	p.mu.Unlock()
	if f != nil {
		f(format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// cmd runs one IPC command and waits for its response.
func (p *Player) cmd(args ...any) (json.RawMessage, error) {
	p.mu.Lock()
	p.seq++
	rid := p.seq
	ch := make(chan ipcMsg, 1)
	p.pend[rid] = ch
	err := p.writeLocked(struct {
		Command   []any `json:"command"`
		RequestID int   `json:"request_id"`
	}{args, rid})
	p.mu.Unlock()
	if err != nil {
		p.mu.Lock()
		delete(p.pend, rid)
		p.mu.Unlock()
		return nil, err
	}
	select {
	case m := <-ch:
		if m.Error != "success" {
			return nil, fmt.Errorf("mpv: %s: %v", m.Error, args)
		}
		return m.Data, nil
	case <-time.After(cmdTimeout):
		p.mu.Lock()
		delete(p.pend, rid)
		p.mu.Unlock()
		return nil, fmt.Errorf("mpv: timeout: %v", args)
	}
}

// tell fires a command without waiting for its reply (python tell parity).
func (p *Player) tell(args ...any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.writeLocked(struct {
		Command []any `json:"command"`
	}{args})
}

func (p *Player) writeLocked(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	// a hung write (busy mpv, full socket buffer) must not stall the
	// engine's mutex: bound every IPC write
	_ = p.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err = p.conn.Write(append(b, '\n'))
	return err
}

// Get reads one mpv property; nil means "no value" (e.g. time-pos when idle).
func (p *Player) Get(prop string) (any, error) {
	raw, err := p.cmd("get_property", prop)
	if err != nil || len(raw) == 0 {
		return nil, err
	}
	var v any
	return v, json.Unmarshal(raw, &v)
}

// batchGet reads several properties in ONE IPC round trip: the requests
// go out as a single write and the replies come back tagged with their
// request_id (the read loop already routes them). A property mpv does
// not answer is simply absent from the result. The second return value
// says whether the batch was COMPLETE: a truncated batch (mpv wedged
// mid-way) must not be mistaken for a real sample.
func (p *Player) batchGet(props []string) (map[string]any, bool) {
	out := map[string]any{}
	if p.conn == nil {
		return out, false
	}
	type req struct {
		rid  int
		prop string
		ch   chan ipcMsg
	}
	p.mu.Lock()
	reqs := make([]req, 0, len(props))
	var buf bytes.Buffer
	for _, prop := range props {
		p.seq++
		rid := p.seq
		ch := make(chan ipcMsg, 1)
		p.pend[rid] = ch
		reqs = append(reqs, req{rid, prop, ch})
		b, err := json.Marshal(struct {
			Command   []any `json:"command"`
			RequestID int   `json:"request_id"`
		}{[]any{"get_property", prop}, rid})
		if err != nil {
			p.mu.Unlock()
			return out, false
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	_ = p.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	n, err := p.conn.Write(buf.Bytes())
	p.mu.Unlock()
	if err != nil || n != buf.Len() { // a short write would split a command
		p.mu.Lock()
		for _, r := range reqs {
			delete(p.pend, r.rid)
		}
		p.mu.Unlock()
		return out, false
	}
	// one budget for the whole batch: a wedged mpv must not stretch a poll
	// tick into seven command timeouts
	deadline := time.NewTimer(cmdTimeout)
	defer deadline.Stop()
	for i, r := range reqs {
		select {
		case m := <-r.ch:
			if m.Error != "success" || len(m.Data) == 0 {
				continue
			}
			var v any
			if json.Unmarshal(m.Data, &v) == nil && v != nil {
				out[r.prop] = v
			}
		case <-deadline.C:
			p.mu.Lock()
			for _, rest := range reqs[i:] {
				delete(p.pend, rest.rid)
			}
			p.mu.Unlock()
			return out, false // truncated: the sample is incomplete
		}
	}
	return out, true
}

// numProp reads a numeric property out of a batch result.
func numProp(props map[string]any, key string) (float64, bool) {
	v, ok := props[key]
	if !ok || v == nil {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
}

// boolProp reads a boolean property out of a batch result.
func boolProp(props map[string]any, key string) bool {
	v, ok := props[key]
	if !ok || v == nil {
		return false
	}
	b, _ := v.(bool)
	return b
}

// audioLayout reads the channel layout name out of the batch
// ("stereo", "5.1", ...). mpv only answers audio-params while an audio
// stream is loaded, so an unknown layout stays EMPTY: the UI must not
// claim "stereo" it cannot see.
func audioLayout(props map[string]any) string {
	m, ok := props["audio-params"].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m["channels"].(string)
	return s
}

// subLang returns the language of the active subtitle track. Empty when
// there is none, when mpv does not know it ("und") or when the property
// is missing (an older build).
func subLang(props map[string]any) string {
	list, ok := props["track-list"].([]any)
	if !ok {
		return ""
	}
	for _, t := range list {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if ty, _ := m["type"].(string); ty != "sub" {
			continue
		}
		if sel, _ := m["selected"].(bool); !sel {
			continue
		}
		lang, _ := m["lang"].(string)
		if lang == "" || lang == "und" {
			return ""
		}
		return lang
	}
	return ""
}

// Pos returns the current playback position; ok=false when nothing plays.
func (p *Player) Pos() (float64, bool) {
	v, err := p.Get("time-pos")
	if err != nil || v == nil {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
}

// SetPause, Seek and ShowText are the engine's actuator surface (Player
// interface); ShowText renders chat as a brief mpv OSD.
func (p *Player) SetPause(paused bool) error {
	return p.tell("set_property", "pause", paused)
}

// SetVolume nudges the audio volume by delta percent over mpv's own add
// property command (clamped by mpv): F5's local audio keys. Tell-only —
// no round trip; the next poll reports the real value to the stats bar.
func (p *Player) SetVolume(delta float64) error {
	return p.tell("add", "volume", delta)
}

// ToggleMute cycles the mute flag (tell-only, same deal as SetVolume: the
// next poll names the real state — no read-first round trip).
func (p *Player) ToggleMute() error {
	return p.tell("cycle", "mute")
}

// AddSubDelay shifts subtitle timing by delta seconds (positive = subs
// appear later): mpv's own relative `add`, so the read-add-set happens
// inside mpv as one step — tell-only like SetVolume, no round trip to read
// the current value; the next OSD renders mpv's real value.
func (p *Player) AddSubDelay(delta float64) error {
	return p.tell("add", "sub-delay", delta)
}

// SetSubDelay pins subtitle timing to v seconds (0 = back in sync).
func (p *Player) SetSubDelay(v float64) error {
	return p.tell("set_property", "sub-delay", v)
}

// ShowSubDelayHint renders the subtitle-timing hint in the mpv window.
// The JSON array API mpv receives over the IPC has property expansion
// OFF by default — the dedicated expand-properties prefix turns it on,
// so the window shows the REAL current delay (mpv renders the value in
// its own milliseconds format). The shared ShowText must stay
// prefix-free: it also displays other users' chat, and a chat line
// containing ${...} must never be expanded.
func (p *Player) ShowSubDelayHint() error {
	return p.tell("expand-properties", "show-text", "sub-delay ${sub-delay}", 1500)
}

func (p *Player) Seek(t float64) error {
	return p.tell("seek", t, "absolute")
}

// Speed applies the follower's smooth-sync tempo nudge (set_property;
// fires no pause/seek event, so no actuation gating is needed).
func (p *Player) Speed(f float64) error {
	return p.tell("set_property", "speed", f)
}

func (p *Player) ShowText(text string, ms int) error {
	return p.tell("show-text", text, ms)
}

// StartLoad tells mpv to play vurl and returns the URL the file will live
// under. F6-A1: this tell is the ONLY load step allowed under the engine
// lock (a bounded write); the wait and the subtitles continue off it.
func (p *Player) StartLoad(name string) (string, error) {
	vurl := fmt.Sprintf("%s/v/%s", p.base, url.PathEscape(name))
	if err := p.tell("loadfile", vurl); err != nil {
		return "", err
	}
	return vurl, nil
}

// WaitLive blocks until name is the live, seekable file or the deadline
// passes. F6-A1: this is the load settle's GUARANTEED signal — the mpv
// events channel drops under stall, so it can never be the trigger. Every
// IPC round trip is taken only while budget remains: a wedged mpv may
// overshoot the deadline by ONE cmdTimeout, never by two (master-2
// condition on the old loop's tail).
func (p *Player) WaitLive(name string, deadline time.Time) bool {
	vurl := fmt.Sprintf("%s/v/%s", p.base, url.PathEscape(name))
	pathOK := false
	for time.Until(deadline) > 0 {
		if !pathOK {
			got, _ := p.Get("path") // may eat the whole remaining budget
			pathOK = got == vurl
		} else {
			if v, _ := p.Get("seekable"); v == true {
				return true
			}
			pathOK = false // seekable lags: re-confirm the path next round
		}
		if time.Until(deadline) <= 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// one final honest check after the deadline — a single Get pair
	// (up to ~2x cmdTimeout; the caller is the lock-free continuation,
	// so this tail cannot stall the UI)
	if got, _ := p.Get("path"); got == vurl {
		if v, _ := p.Get("seekable"); v == true {
			return true
		}
	}
	return false
}

// AttachSubs attaches the sidecar subtitle tracks (HTTP probe + sub-add
// tells + the j-key OSD). F6-A1: a continuation step — best-effort, run
// OFF the engine lock.
func (p *Player) AttachSubs(name string) {
	// R8: subtitle siblings (movie.tr.srt, movie.es.srt, ...) ride along as
	// selectable tracks — each user switches locally with mpv's j/J keys in
	// the player window; subtitle choice never enters the sync layer. The
	// plain <stem>.srt goes first and is selected, matching the old
	// single-subtitle behaviour; the rest are added unselected.
	ordered := subCandidates(p.subList(), name)
	added := 0
	for _, s := range ordered {
		flag := "auto"
		if added == 0 {
			flag = "select" // show the default track immediately
		}
		if err := p.tell("sub-add", fmt.Sprintf("%s/sub/%s", p.base, url.PathEscape(s)), flag); err == nil {
			added++
		}
	}
	if added > 1 {
		_ = p.ShowText(fmt.Sprintf("%d subtitles: press j in the player window to switch", added), 4000)
	}
}

// subCandidates filters the server's /subs listing down to the sidecars of
// video name: the exact <stem>.srt/.ass first, then the rest of the
// <stem>.* family in server order (the server list is sorted). Pure string
// logic so AttachSubs and /sub agree on what belongs to the file.
func subCandidates(subs []string, name string) []string {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	ordered := make([]string, 0, len(subs))
	for _, exact := range []string{stem + ".srt", stem + ".ass"} {
		if slices.Contains(subs, exact) {
			ordered = append(ordered, exact)
		}
	}
	for _, s := range subs {
		if strings.HasPrefix(s, stem+".") && !slices.Contains(ordered, s) {
			ordered = append(ordered, s)
		}
	}
	return ordered
}

// SubCandidates lists the sidecar subtitles for video name (the /sub
// listing): the same probe and ordering AttachSubs attaches from.
func (p *Player) SubCandidates(name string) []string {
	return subCandidates(p.subList(), name)
}

// AttachSub selects ONE sidecar subtitle for the CURRENT video (a sub-add
// tell; never a loadfile — the film keeps playing). /sub's attach step.
func (p *Player) AttachSub(name string) error {
	return p.tell("sub-add", fmt.Sprintf("%s/sub/%s", p.base, url.PathEscape(name)), "select")
}

// subList fetches the server's subtitle listing; subtitles are best-effort
// (python parity: a missing subtitle is not an error worth logging).
func (p *Player) subList() []string {
	resp, err := subClient.Get(p.base + "/subs")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var subs []string
	if err := json.NewDecoder(resp.Body).Decode(&subs); err != nil {
		return nil
	}
	return subs
}

// Poll samples player state every period into out until ctx is cancelled
// or mpv exits (plan Q1: polling stays the heartbeat; pushed events carry
// the instant discrete transitions).
func (p *Player) Poll(ctx context.Context, period time.Duration, out chan<- State) {
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.exited:
			select {
			case out <- State{Exited: true}:
			case <-ctx.Done():
			}
			return
		case <-p.dead:
			// the IPC died under us: the client cannot drive mpv any more
			select {
			case out <- State{Exited: true}:
			case <-ctx.Done():
			}
			return
		case <-t.C:
		}
		st := State{}
		props, complete := p.batchGet(pollProps) // one IPC round trip for the whole tick
		st.At = time.Now()                       // when the sample was taken
		st.Partial = !complete                   // a truncated batch is not a real sample
		st.Pause = boolProp(props, "pause")
		st.Idle = boolProp(props, "idle-active")
		st.Buffering = boolProp(props, "paused-for-cache")
		st.Muted = boolProp(props, "mute")
		if f, ok := numProp(props, "time-pos"); ok {
			st.Pos = &f
		}
		st.Cache, _ = numProp(props, "demuxer-cache-time")
		st.Volume, _ = numProp(props, "volume")
		st.Dur, _ = numProp(props, "duration")
		st.Layout = audioLayout(props)
		st.Sub = subLang(props)
		select {
		case out <- st:
		case <-ctx.Done():
			return
		}
	}
}

// Close reaps mpv: SIGTERM, 2s grace, then Kill — orphaned mpv must be
// impossible (risk C) — and removes the temp socket dir. It is safe to
// call twice (the clean path and the signal path can both reach it) and
// a second caller WAITS for the first one to finish: the process must
// not exit while mpv is still being reaped (master-2 P2-1).
func (p *Player) Close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		if p.closeDone == nil {
			p.closeDone = make(chan struct{})
		}
		p.closed = true // from here on a dead conn is expected, not a crash
		p.mu.Unlock()
		defer close(p.closeDone)
		if p.conn != nil {
			p.conn.Close() // unblocks the reader and any pending cmd
		}
		if p.proc != nil && p.proc.Process != nil {
			_ = terminate(p.proc.Process)
			select {
			case <-p.exited:
			case <-time.After(2 * time.Second):
				_ = p.proc.Process.Kill()
				<-p.exited
			}
		}
		os.RemoveAll(p.dir)
	})
	<-p.closeDone
}
