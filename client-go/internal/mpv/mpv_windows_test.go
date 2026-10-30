//go:build windows

package mpv

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

// W7 pin: a pipe that does not exist must fail FAST (ERROR_FILE_NOT_FOUND),
// not wait for the dial timeout — the client's start-up loop (50 tries)
// depends on the fail-fast behaviour, otherwise a missing mpv costs 15s.
func TestDialPipeFailsFastOnMissingPipe(t *testing.T) {
	name := `\\.\pipe\watchparty-missing-` + randSuffix()
	to := 2 * time.Second
	start := time.Now()
	conn, err := winio.DialPipe(name, &to)
	elapsed := time.Since(start)
	if err == nil {
		conn.Close()
		t.Fatal("dialing a pipe that does not exist must fail")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("dialing a missing pipe took %v: it must fail fast, not wait for the timeout", elapsed)
	}
}

// W6 pin: the pipe name must be unique per client, not PID-only (an
// orphaned mpv holding the old name plus a reused PID would break the
// new client).
func TestIPCPipeNameIsUniqueOnWindows(t *testing.T) {
	a, b := ipcPipeName(), ipcPipeName()
	if a == b {
		t.Fatalf("the pipe name repeats: %q", a)
	}
}

// TestWindowsHelperProcess is not a real test: it is the child process
// the terminate test spawns (the classic os/exec helper pattern — one
// process, no orphans).
func TestWindowsHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

// terminate() must kill a real child on Windows: there is no SIGTERM
// there, so the Kill path (TerminateProcess) is the only reaper.
func TestTerminateKillsChild(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestWindowsHelperProcess")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	time.Sleep(200 * time.Millisecond) // let the child settle

	if err := terminate(cmd.Process); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the child survived terminate()")
	}
}
