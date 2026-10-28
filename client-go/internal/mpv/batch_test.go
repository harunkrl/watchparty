//go:build !windows

package mpv

// C6: one poll tick must cost ONE IPC round trip. A fake mpv answers
// get_property requests and reports how many commands arrived in a single
// read: the batch has to travel as one write.

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// fakeMpv answers get_property requests with a canned value per property.
func fakeMpv(t *testing.T, reads chan<- int) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "mpv.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 8192)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				select {
				case reads <- bytes.Count(buf[:n], []byte("\n")):
				default:
				}
				var lines [][]byte
				for _, line := range bytes.Split(buf[:n], []byte("\n")) {
					if len(bytes.TrimSpace(line)) == 0 {
						continue
					}
					lines = append(lines, line)
				}
				// answer in REVERSE order: the client must route the
				// replies by request_id, not by arrival order
				for i := len(lines) - 1; i >= 0; i-- {
					var req struct {
						Command   []any `json:"command"`
						RequestID int   `json:"request_id"`
					}
					if json.Unmarshal(lines[i], &req) != nil || len(req.Command) < 2 {
						continue
					}
					prop, _ := req.Command[1].(string)
					resp, _ := json.Marshal(map[string]any{
						"error":      "success",
						"data":       fakeValue(prop),
						"request_id": req.RequestID,
					})
					if _, err := c.Write(append(resp, '\n')); err != nil {
						return
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return sock
}

func fakeValue(prop string) any {
	switch prop {
	case "time-pos":
		return 12.5
	case "volume":
		return 55.0
	case "demuxer-cache-time":
		return 3.5
	case "mute":
		return true
	case "duration":
		return 90.0
	case "pause", "idle-active", "paused-for-cache":
		return false
	}
	return nil
}

func TestBatchGetIsOneRoundTrip(t *testing.T) {
	reads := make(chan int, 4)
	sock := fakeMpv(t, reads)
	p := testPlayer(t, sock)
	go p.readLoop()

	props := p.batchGet(pollProps)
	if len(props) != len(pollProps) {
		t.Fatalf("got %d of %d properties: %v", len(props), len(pollProps), props)
	}
	if v, ok := props["time-pos"].(float64); !ok || v != 12.5 {
		t.Fatalf("time-pos = %v, want 12.5", props["time-pos"])
	}
	if v, ok := props["mute"].(bool); !ok || !v {
		t.Fatalf("mute = %v, want true", props["mute"])
	}
	if v, ok := props["duration"].(float64); !ok || v != 90 {
		t.Fatalf("duration = %v, want 90", props["duration"])
	}
	select {
	case n := <-reads:
		if n != len(pollProps) {
			t.Fatalf("the batch travelled in %d reads, want 1 (one round trip)", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the fake mpv never saw the batch")
	}
}

// The poll tick reads the whole set in one round trip and still fills the
// state exactly as before (plus the mute flag and the media duration).
func TestPollReadsBatchInOneRoundTrip(t *testing.T) {
	reads := make(chan int, 8)
	sock := fakeMpv(t, reads)
	p := testPlayer(t, sock)
	go p.readLoop()

	out := make(chan State, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Poll(ctx, 50*time.Millisecond, out)

	select {
	case st := <-out:
		if st.Pos == nil || *st.Pos != 12.5 {
			t.Fatalf("pos = %v, want 12.5", st.Pos)
		}
		if st.Volume != 55 || st.Cache != 3.5 {
			t.Fatalf("volume/cache = %v/%v", st.Volume, st.Cache)
		}
		if !st.Muted {
			t.Fatal("mute flag not polled")
		}
		if st.Dur != 90 {
			t.Fatalf("duration = %v, want 90", st.Dur)
		}
		if st.Pause || st.Idle || st.Buffering {
			t.Fatalf("unexpected flags: %+v", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the poll never produced a state")
	}
	select {
	case n := <-reads:
		if n != len(pollProps) {
			t.Fatalf("poll used %d round trips, want 1", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the fake mpv never saw the poll batch")
	}
}
