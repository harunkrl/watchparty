package mpv

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runMPV runs mpv with the client's stream wiring (stdout+stderr into
// the player's log) and returns what reached the log.
func runMPV(t *testing.T, bin, file string) []string {
	t.Helper()
	logged := make(chan string, 32)
	p := &Player{
		Logf:     func(f string, a ...any) { logged <- fmt.Sprintf(f, a...) },
		stderrCh: make(chan string, 64),
	}
	go p.drainStderr()
	w := &stderrWriter{p: p}
	cmd := exec.Command(bin, mpvQuiet, "--no-config", "--vo=null", "--ao=null", "--length=1", file)
	cmd.Stdout = w
	cmd.Stderr = w
	_ = cmd.Run() // a missing file makes mpv fail: that is the point
	var got []string
	for {
		select {
		case line := <-logged:
			got = append(got, line)
		case <-time.After(300 * time.Millisecond):
			return got
		}
	}
}

// ISSUE-5: measured, not assumed — mpv writes its messages to STDOUT
// when stdout is a pipe (stderr stays empty), so the client captures
// BOTH streams. With --quiet the playback chatter is a few lines per
// file (the status line is gone) and the ERRORS come through: the
// friend must see why mpv failed.
func TestMpvErrorReachesTheClientLog(t *testing.T) {
	bin, err := exec.LookPath("mpv")
	if err != nil {
		t.Skip("no mpv in PATH")
	}
	got := runMPV(t, bin, filepath.Join(t.TempDir(), "nope.mp4"))
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "Failed to open") {
		t.Fatalf("mpv's error never reached the client log: %v", got)
	}
}

// ISSUE-7: the playback chatter must stay small — --quiet silences the
// status line, so a file load costs a few lines, not a flood.
func TestMpvPlaybackChatterIsSmall(t *testing.T) {
	bin, err := exec.LookPath("mpv")
	if err != nil {
		t.Skip("no mpv in PATH")
	}
	dir := t.TempDir()
	play := filepath.Join(dir, "silence.wav")
	writeSilentWAV(t, play)
	got := runMPV(t, bin, play)
	if len(got) > 10 {
		t.Fatalf("mpv flooded the log with %d lines: %v", len(got), got)
	}
}

// writeSilentWAV writes a one second silent mono WAV — a playable file
// without needing ffmpeg.
func writeSilentWAV(t *testing.T, path string) {
	t.Helper()
	const rate, seconds = 8000, 1
	data := make([]byte, rate*seconds*2) // 16-bit mono silence
	var buf bytes.Buffer
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+len(data)))
	buf.WriteString("WAVEfmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16)) // fmt chunk size
	binary.Write(&buf, binary.LittleEndian, uint16(1))  // PCM
	binary.Write(&buf, binary.LittleEndian, uint16(1))  // channels
	binary.Write(&buf, binary.LittleEndian, uint32(rate))
	binary.Write(&buf, binary.LittleEndian, uint32(rate*2))
	binary.Write(&buf, binary.LittleEndian, uint16(2)) // block align
	binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(len(data)))
	buf.Write(data)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The client's mpv flags must carry the quiet flag that keeps the
// errors (not --really-quiet, which silences everything).
func TestMpvQuietFlagIsTheOneWePass(t *testing.T) {
	args := mpvArgs("sock", nil)
	found := false
	for _, a := range args {
		if a == "--really-quiet" {
			t.Fatal("--really-quiet silences every mpv message")
		}
		if a == mpvQuiet {
			found = true
		}
	}
	if !found {
		t.Fatalf("the quiet flag %q is missing: %v", mpvQuiet, args)
	}
}
