//go:build !windows

package mpv

// Audit round 2: the IPC layer's failure modes. A fake mpv socket stands
// in for the real player: it can swallow commands without answering (a
// wedged mpv) or hang up mid-session (a dead IPC with mpv still alive).

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeIPC is a unix socket that speaks nothing: commands are read and
// dropped, so every Get() waits for its timeout (a wedged mpv).
func fakeIPC(t *testing.T, keepOpen bool) (string, func()) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "mpv.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				if !keepOpen {
					c.Close() // hang up: the IPC dies, mpv "lives"
					return
				}
				io.Copy(io.Discard, c)
			}(c)
		}
	}()
	return sock, func() { ln.Close() }
}

// testPlayer wires a Player to a fake socket without spawning mpv.
func testPlayer(t *testing.T, sock string) *Player {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	return &Player{
		base:   "http://127.0.0.1:1", // nothing listening: the subtitle probe fails fast
		conn:   conn,
		dir:    t.TempDir(),
		exited: make(chan struct{}),
		dead:   make(chan struct{}),
		events: make(chan Event, 8),
		pend:   map[int]chan ipcMsg{},
	}
}

// A wedged mpv must not pin the caller for minutes: Load waits for the
// file to become live, and the engine holds its lock across that call
// (OnRoom -> load -> Player.Load). The wait is bounded, not 50 rounds of
// per-command timeouts.
func TestLoadWaitIsBoundedWhenMpvIsWedged(t *testing.T) {
	origCmd, origWait := cmdTimeout, loadWait
	cmdTimeout, loadWait = 20*time.Millisecond, 200*time.Millisecond
	defer func() { cmdTimeout, loadWait = origCmd, origWait }()

	sock, cleanup := fakeIPC(t, true)
	defer cleanup()
	p := testPlayer(t, sock)
	defer p.Close()

	start := time.Now()
	_ = p.Load("movie.mkv")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Load blocked %s on a wedged mpv (wait must be bounded)", elapsed)
	}
}

// A dead IPC with mpv still alive is invisible today: the read loop dies,
// every command fails, the poll keeps sampling nothing and the client
// freezes on a stale frame while the room's clock stops. The player must
// report the dead connection instead.
func TestDeadIPCIsReported(t *testing.T) {
	sock, cleanup := fakeIPC(t, false) // accepts, then hangs up
	defer cleanup()
	p := testPlayer(t, sock)
	go p.readLoop()

	select {
	case <-p.dead:
	case <-time.After(2 * time.Second):
		t.Fatal("dead IPC not detected")
	}

	out := make(chan State, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Poll(ctx, 50*time.Millisecond, out)

	select {
	case st := <-out:
		if !st.Exited {
			t.Fatalf("dead IPC reported as %+v, want Exited", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("poll never reported the dead IPC")
	}
}

// Close is the normal end of life: the conn dies on purpose, which must
// not be reported as a crash.
func TestCloseDoesNotReportDeadIPC(t *testing.T) {
	sock, cleanup := fakeIPC(t, true)
	defer cleanup()
	p := testPlayer(t, sock)
	go p.readLoop()
	p.Close()
	select {
	case <-p.dead:
		t.Fatal("Close reported a dead IPC")
	case <-time.After(200 * time.Millisecond):
	}
}

// The drop notice must reach the caller's logger: in the TUI a stderr
// write corrupts the alt-screen frame (the engine's log goes through the
// program for the same reason).
func TestEventDropNoticeUsesLogf(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "mpv.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		// flood events: the buffer (cap 8) overflows and the drop path runs
		for i := 0; i < 50; i++ {
			if _, err := c.Write([]byte("{\"event\":\"property-change\",\"name\":\"pause\",\"data\":true}\n")); err != nil {
				return
			}
		}
		<-time.After(time.Second) // keep the connection open
		c.Close()
	}()

	p := testPlayer(t, sock)
	logged := make(chan string, 4)
	p.Logf = func(f string, a ...any) { logged <- fmt.Sprintf(f, a...) }
	go p.readLoop()

	select {
	case msg := <-logged:
		if !strings.Contains(msg, "dropped") {
			t.Fatalf("unexpected log line: %q", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drop notice never reached Logf")
	}
}
