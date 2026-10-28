// Package mpv spawns and drives an external mpv process over its JSON IPC
// (unix socket / windows named pipe), mirroring client.py's Player/Ipc.
// A dedicated reader goroutine routes command replies by request_id and
// forwards mpv events (pause/unpause/seek) for the instant sync path.
package mpv

import (
	"bufio"
	"context"
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
	cmdTimeout           = 5 * time.Second
)

// State is one poll sample of the player, consumed by the sync engine.
type State struct {
	Pos       *float64 // nil: no time-pos (nothing loaded or idle)
	Pause     bool
	Idle      bool
	Buffering bool // cache underrun: mpv stalled playback to refill (paused-for-cache)
	Exited    bool // mpv window was closed; client should shut down
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
	events chan Event
	mu     sync.Mutex // guards pend, seq, drops and writes
	pend   map[int]chan ipcMsg
	seq    int
}

func Start(base string, extra []string) (*Player, error) {
	var dir, sock string
	if runtime.GOOS == "windows" {
		sock = fmt.Sprintf(`\\.\pipe\watchparty-%d`, os.Getpid())
	} else {
		var err error
		dir, err = os.MkdirTemp("", "watchparty-") // unguessable, python parity
		if err != nil {
			return nil, err
		}
		sock = filepath.Join(dir, fmt.Sprintf("mpv-%d.sock", os.Getpid()))
	}
	args := append([]string{
		"--idle", "--force-window", "--really-quiet", "--title=Watchparty",
		"--input-ipc-server=" + sock,
		"--cache=yes",
		fmt.Sprintf("--demuxer-readahead-secs=%d", demuxerReadaheadSecs),
		"--demuxer-max-bytes=" + demuxerMaxBytes,
	}, extra...)
	cmd := exec.Command("mpv", args...)
	// mpv.exe is bundled next to watchparty.exe; LookPath warns relative-cwd
	// resolution (Go 1.19+ ErrDot); we deliberately accept the sibling.
	if errors.Is(cmd.Err, exec.ErrDot) {
		cmd.Err = nil
	}
	// Stdin nil = os.DevNull: otherwise mpv steals the keyboard from the TUI
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("could not start mpv (is it installed?): %w", err)
	}
	exited := make(chan struct{})
	p := &Player{
		base: strings.TrimRight(base, "/"), proc: cmd, dir: dir, exited: exited,
		events: make(chan Event, 64),
		pend:   map[int]chan ipcMsg{},
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
				if p.drops < 3 {
					p.drops++
					fmt.Fprintln(os.Stderr, "mpv: event dropped (engine stalled?)")
				}
				p.mu.Unlock()
			}
		}
	}
	p.mu.Lock()
	for rid, ch := range p.pend {
		delete(p.pend, rid)
		ch <- ipcMsg{Error: "ipc closed"}
	}
	p.mu.Unlock()
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

// Load starts playing name, waits until the file is actually live, then
// attaches a sidecar subtitle if one sits next to the video. A missing
// subtitle (404) is not an error worth logging (python parity).
func (p *Player) Load(name string) error {
	vurl := fmt.Sprintf("%s/v/%s", p.base, url.PathEscape(name))
	if err := p.tell("loadfile", vurl); err != nil {
		return err
	}
	for i := 0; i < 50; i++ { // wait until loaded AND seekable: a seek issued
		// between loadfile and file-loaded is silently dropped by mpv
		if got, _ := p.Get("path"); got == vurl {
			if v, _ := p.Get("seekable"); v == true {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	// a loadfile that failed (bad file, 404, unsupported) drops mpv back to
	// idle — surface it instead of returning success for a player that never
	// started (the engine logs this as "mpv: ..." next to its "playing" line)
	if got, _ := p.Get("path"); got != vurl {
		if idle, _ := p.Get("idle-active"); idle == true {
			return fmt.Errorf("load failed: %s", name)
		}
	}
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	// R8: subtitle siblings (movie.tr.srt, movie.es.srt, ...) ride along as
	// selectable tracks — each user switches locally with mpv's j/J keys in
	// the player window; subtitle choice never enters the sync layer. The
	// plain <stem>.srt goes first and is selected, matching the old
	// single-subtitle behaviour; the rest are added unselected.
	subs, stemPrefix := p.subList(), stem+"."
	ordered := make([]string, 0, len(subs))
	for _, exact := range []string{stem + ".srt", stem + ".ass"} {
		if slices.Contains(subs, exact) {
			ordered = append(ordered, exact)
		}
	}
	for _, s := range subs { // server list is sorted: en < es < tr ...
		if strings.HasPrefix(s, stemPrefix) && !slices.Contains(ordered, s) {
			ordered = append(ordered, s)
		}
	}
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
	return nil
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
		case <-t.C:
		}
		st := State{}
		if v, err := p.Get("pause"); err == nil && v != nil {
			st.Pause, _ = v.(bool)
		}
		if v, err := p.Get("idle-active"); err == nil && v != nil {
			st.Idle, _ = v.(bool)
		}
		if v, err := p.Get("paused-for-cache"); err == nil && v != nil {
			st.Buffering, _ = v.(bool)
		}
		if v, err := p.Get("time-pos"); err == nil && v != nil {
			if f, ok := v.(float64); ok {
				st.Pos = &f
			}
		}
		select {
		case out <- st:
		case <-ctx.Done():
			return
		}
	}
}

// Close reaps mpv: SIGTERM, 2s grace, then Kill — orphaned mpv must be
// impossible (risk C) — and removes the temp socket dir.
func (p *Player) Close() {
	if p.conn != nil {
		p.conn.Close() // unblocks the reader and any pending cmd
	}
	if p.proc.Process != nil {
		_ = terminate(p.proc.Process)
		select {
		case <-p.exited:
		case <-time.After(2 * time.Second):
			_ = p.proc.Process.Kill()
			<-p.exited
		}
	}
	os.RemoveAll(p.dir)
}
