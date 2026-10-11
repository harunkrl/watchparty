// Package mpv spawns and drives an external mpv process over its JSON IPC
// (unix socket / windows named pipe), mirroring client.py's Player/Ipc.
package mpv

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Fixed numbers from the plan: aggressive LAN readahead so upload hiccups
// never stall playback (user-overridable via WATCHPARTY_MPV).
const (
	demuxerReadaheadSecs = 300
	demuxerMaxBytes      = "2GiB"
)

// State is one poll sample of the player, consumed by the sync engine.
type State struct {
	Pos    *float64 // nil: no time-pos (nothing loaded or idle)
	Pause  bool
	Idle   bool
	Exited bool // mpv window was closed; client should shut down
}

type Player struct {
	base   string
	proc   *exec.Cmd
	conn   net.Conn
	rd     *bufio.Reader
	dir    string        // temp dir holding the socket (removed on Close)
	exited chan struct{} // closed when the mpv process is reaped
	mu     sync.Mutex
	next   int
}

// Start spawns mpv and connects to its IPC socket (50 x 100ms retries,
// python parity). A nil cmd.Stdin means mpv reads from os.DevNull —
// otherwise mpv would steal the terminal keyboard from the TUI.
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
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("could not start mpv (is it installed?): %w", err)
	}
	exited := make(chan struct{})
	p := &Player{base: strings.TrimRight(base, "/"), proc: cmd, dir: dir, exited: exited}
	go func() {
		_ = cmd.Wait()
		close(exited) // Poll reports this as State{Exited: true}
	}()
	for i := 0; i < 50; i++ {
		conn, err := dialPipe(sock)
		if err == nil {
			p.conn = conn
			p.rd = bufio.NewReader(conn)
			return p, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.Close()
	return nil, fmt.Errorf("could not connect to mpv ipc")
}

type ipcMsg struct {
	Data      json.RawMessage `json:"data"`
	Error     string          `json:"error"`
	RequestID int             `json:"request_id"`
}

// cmd runs one IPC command and waits for its response. Events and stale
// fire-and-forget replies are skipped, malformed lines ignored (python parity).
func (p *Player) cmd(args ...any) (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next++
	if err := p.write(struct {
		Command   []any `json:"command"`
		RequestID int   `json:"request_id"`
	}{args, p.next}); err != nil {
		return nil, err
	}
	for {
		line, err := p.rd.ReadString('\n')
		if err != nil {
			return nil, err
		}
		var m ipcMsg
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if m.RequestID != p.next {
			continue
		}
		if m.Error != "success" {
			return nil, fmt.Errorf("mpv: %s: %v", m.Error, args)
		}
		return m.Data, nil
	}
}

// tell fires a command without waiting for its reply (python tell parity).
func (p *Player) tell(args ...any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.write(struct {
		Command []any `json:"command"`
	}{args})
}

func (p *Player) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
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

// SetPause, Seek and ShowText are the engine's actuator surface (Player
// interface); ShowText renders chat as a brief mpv OSD.
func (p *Player) SetPause(paused bool) error {
	return p.tell("set_property", "pause", paused)
}

func (p *Player) Seek(t float64) error {
	return p.tell("seek", t, "absolute")
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
	for i := 0; i < 50; i++ { // wait until the file is actually live
		if got, _ := p.Get("path"); got == vurl {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	for _, ext := range []string{".srt", ".ass"} {
		sub := fmt.Sprintf("%s/sub/%s", p.base, url.PathEscape(stem+ext))
		resp, err := http.Get(sub)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return p.tell("sub-add", sub, "auto")
		}
	}
	return nil
}

// Poll samples player state every period into out until ctx is cancelled
// or mpv exits (plan Q1: polling; observe-property only if M4 measurement
// justifies it).
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
		p.conn.Close() // unblocks any pending IPC read
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
