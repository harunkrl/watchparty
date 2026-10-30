package mpv

// master-2 P2-1: the idempotent Close must still WAIT. A second caller
// returns only after the first one finished: the process must not exit
// while mpv is still being reaped (an orphan mpv is worse than a slow
// exit). Platform-neutral: the helper child is the test binary itself.

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestCloseHelper is not a real test: it is the child process the Close
// test spawns (the classic helper-process pattern).
func TestCloseHelper(t *testing.T) {
	if os.Getenv("GO_WANT_CLOSE_HELPER") != "1" {
		return
	}
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

// A concurrent double Close must be safe: no panic, no deadlock, and the
// child must end up reaped. (That the second caller WAITS is pinned on
// unix in player_test.go, where the 2s grace makes the wait observable.)
func TestConcurrentDoubleCloseIsSafe(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestCloseHelper")
	cmd.Env = append(os.Environ(), "GO_WANT_CLOSE_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &Player{proc: cmd, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	go p.Close() // one of the two reaps the child
	p.Close()    // the other must not break anything
	select {
	case <-p.exited:
	default:
		t.Fatal("the child was not reaped")
	}
}
