package mpv

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeMPVName is the file name a real mpv has on this platform: on
// Windows LookPath only finds "mpv.exe" (PATHEXT), never an
// extension-less "mpv" — a test that wrote "mpv" would fail there.
func fakeMPVName() string {
	if runtime.GOOS == "windows" {
		return "mpv.exe"
	}
	return "mpv"
}

// writeFakeMPV drops a fake mpv binary into dir.
func writeFakeMPV(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, fakeMPVName()), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// W5: the release bundle ships a digest-pinned mpv next to the exe: a
// stale system mpv on PATH must not shadow it.
func TestBundleMpvWinsOverPath(t *testing.T) {
	bundle, sys := t.TempDir(), t.TempDir()
	writeFakeMPV(t, bundle)
	writeFakeMPV(t, sys)
	t.Setenv("PATH", sys)
	old := mpvExeDir
	mpvExeDir = func() string { return bundle }
	defer func() { mpvExeDir = old }()

	got, err := findMPV()
	if err != nil {
		t.Fatalf("findMPV: %v", err)
	}
	if want := filepath.Join(bundle, fakeMPVName()); got != want {
		t.Fatalf("findMPV = %q, want the bundled %q (PATH mpv must not shadow it)", got, want)
	}
}

// W5: without a bundled mpv the PATH one is still used.
func TestPathMpvUsedWhenNoBundle(t *testing.T) {
	bundle, sys := t.TempDir(), t.TempDir()
	writeFakeMPV(t, sys)
	t.Setenv("PATH", sys)
	old := mpvExeDir
	mpvExeDir = func() string { return bundle }
	defer func() { mpvExeDir = old }()

	got, err := findMPV()
	if err != nil {
		t.Fatalf("findMPV: %v", err)
	}
	if want := filepath.Join(sys, fakeMPVName()); got != want {
		t.Fatalf("findMPV = %q, want the PATH mpv %q", got, want)
	}
}

// W6: the pipe name must not be PID-only: an orphaned mpv holding the
// old name plus a reused PID would break the new client.
func TestIPCPipeNameIsUnique(t *testing.T) {
	a, b := ipcPipeName(), ipcPipeName()
	if a == b {
		t.Fatalf("the pipe name repeats: %q", a)
	}
	if !strings.Contains(a, strconv.Itoa(os.Getpid())) {
		t.Fatalf("the pipe name must carry the pid: %q", a)
	}
	if !strings.HasPrefix(a, `\\.\pipe\watchparty-`) {
		t.Fatalf("the pipe name must stay in the watchparty namespace: %q", a)
	}
}

// W15: mpv's diagnostics must reach the client log: the friend must see
// WHY mpv failed, not silence.
func TestStderrReachesTheLog(t *testing.T) {
	got := make(chan string, 4)
	p := &Player{
		Logf:     func(f string, a ...any) { got <- fmt.Sprintf(f, a...) },
		stderrCh: make(chan string, 8),
	}
	go p.drainStderr()
	w := &stderrWriter{p: p}
	if _, err := w.Write([]byte("Failed to open x.mp4.\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-got:
		if !strings.Contains(line, "Failed to open x.mp4.") {
			t.Fatalf("wrong line logged: %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mpv's stderr never reached the log")
	}
}

// ISSUE-4: mpv starts before the client can wire its logger: the early
// lines must wait (a stray stderr line would corrupt the alt-screen
// frame) and be flushed when the logger arrives.
func TestStderrWaitsForTheLogger(t *testing.T) {
	logged := make(chan string, 4)
	p := &Player{stderrCh: make(chan string, 8)}
	go p.drainStderr()
	w := &stderrWriter{p: p}
	if _, err := w.Write([]byte("early line\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-logged:
		t.Fatalf("the early line was logged before a logger existed: %q", line)
	case <-time.After(200 * time.Millisecond):
	}
	p.SetLogf(func(f string, a ...any) { logged <- fmt.Sprintf(f, a...) })
	select {
	case line := <-logged:
		if !strings.Contains(line, "early line") {
			t.Fatalf("wrong line flushed: %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the early line was never flushed")
	}
}

// ISSUE-7: a stderr flood must never block mpv: the buffer is bounded
// and the oldest line is dropped.
func TestStderrFloodDoesNotBlock(t *testing.T) {
	p := &Player{stderrCh: make(chan string, 4)}
	w := &stderrWriter{p: p}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			if _, err := w.Write([]byte(fmt.Sprintf("line %d\n", i))); err != nil {
				t.Error(err)
			}
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the stderr writer blocked on a full buffer")
	}
}

// W15: --really-quiet silences mpv's messages entirely; --quiet keeps
// the errors (and still silences the status line).
func TestMpvArgsKeepTheMessages(t *testing.T) {
	args := mpvArgs(`\\.\pipe\x`, nil)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--really-quiet") {
		t.Fatalf("--really-quiet silences every mpv message: %v", args)
	}
	if !strings.Contains(joined, "--quiet") {
		t.Fatalf("mpv must keep its error messages: %v", args)
	}
}
