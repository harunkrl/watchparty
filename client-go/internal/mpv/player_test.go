//go:build !windows

package mpv

import (
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
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

// TestCloseStubbornHelper is not a real test: it is the child process
// the Close test spawns. It ignores the polite SIGTERM (so the reap has
// to fall back to Kill) and announces that it is ready.
func TestCloseStubbornHelper(t *testing.T) {
	if os.Getenv("GO_WANT_CLOSE_STUBBORN_HELPER") != "1" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	os.Stdout.Write([]byte("r")) // ready: the polite TERM will not kill me
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

// master-2 P2-1: the idempotent Close must still WAIT. A stubborn child
// puts the first Close into its 2s grace: the second caller must not
// return before the reap is done (the process must not exit while mpv
// is still being reaped — an orphan mpv is worse than a slow exit).
func TestSecondCloseWaitsForTheReap(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestCloseStubbornHelper")
	cmd.Env = append(os.Environ(), "GO_WANT_CLOSE_STUBBORN_HELPER=1")
	cmd.Stdout = w
	p := newTestPlayer(t, cmd)
	w.Close() // the parent keeps only the read end
	ready := make([]byte, 1)
	if _, err := io.ReadFull(r, ready); err != nil {
		t.Fatal("the child never became ready:", err)
	}
	go p.Close() // the first Close is in its grace window
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	p.Close() // the second one must wait for the reap
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Fatalf("the second Close returned after %s: it did not wait for the reap", elapsed)
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
