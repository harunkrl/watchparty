//go:build !windows

package mpv

import (
	"os/exec"
	"testing"
	"time"
)

// newTestPlayer mirrors Start's invariant: an exited channel closed by a
// reaper goroutine once the child is reaped (Close depends on it).
func newTestPlayer(t *testing.T, cmd *exec.Cmd) *Player {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &Player{proc: cmd, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	return p
}

// Risk C: Close must reap the child no matter what — polite TERM first.
func TestCloseTerminatesPoliteChild(t *testing.T) {
	p := newTestPlayer(t, exec.Command("sleep", "30"))
	start := time.Now()
	p.Close()
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("polite child took %s to die (TERM not honored?)", elapsed)
	}
	if p.proc.ProcessState == nil {
		t.Fatal("child not waited: orphan risk")
	}
}

// Risk C: a child that ignores SIGTERM must be killed within the 2s grace.
func TestCloseKillsStubbornChild(t *testing.T) {
	// pi-lens-ignore: exec
	p := newTestPlayer(t, exec.Command("sh", "-c", `trap '' TERM; sleep 30`))
	start := time.Now()
	p.Close()
	if elapsed := time.Since(start); elapsed > 3500*time.Millisecond {
		t.Fatalf("stubborn child took %s to die (Kill path too slow)", elapsed)
	}
	if p.proc.ProcessState == nil {
		t.Fatal("child not waited: orphan risk")
	}
}

// Risk D: nil cmd.Stdin must read from os.DevNull — a stdin reader would
// see instant EOF instead of hanging on our terminal.
func TestStdinNilIsDevNull(t *testing.T) {
	cmd := exec.Command("cat") // copies stdin to stdout; EOF exits 0
	cmd.Stdin = nil            // mpv.Start relies on this default
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cat failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		cmd.Process.Kill()
		t.Fatal("cat hung: nil Stdin did not provide EOF (DEVNULL broken)")
	}
}
