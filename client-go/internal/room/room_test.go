// M1 review smoke: drive internal/room and internal/cmd against a real
// local server without bubbletea (which hangs the parent pty in raw mode).
package room_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"watchparty/client-go/internal/cmd"
	"watchparty/client-go/internal/room"
)

func freePort(t *testing.T) (int, *exec.Cmd) {
	t.Helper()
	port := 19500 + os.Getpid()%100
	dir, _ := os.MkdirTemp("", "wp-m1rev-")
	for _, name := range []string{"sample.mkv", "with space.mp4", "film 2.mov"} {
		f, _ := os.Create(dir + "/" + name)
		f.Close()
	}
	root, _ := os.Getwd()
	for i := 0; i < 4; i++ {
		if _, err := os.Stat(root + "/server.js"); err == nil {
			break
		}
		root = root + "/.."
	}
	srv := exec.Command("node", root+"/server.js")
	srv.Env = append(os.Environ(), "PORT="+itoa(port), "VIDEOS_DIR="+dir)
	srv.Stdout = nil
	srv.Stderr = os.Stderr
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if r, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/files", port)); err == nil {
			r.Body.Close()
			return port, srv
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("server did not come up")
	return 0, srv
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

func TestM1_StreamReceivesHello(t *testing.T) {
	port, srv := freePort(t)
	defer srv.Process.Kill()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	c := room.New(base, "alice-1234")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out := make(chan room.Event, 8)
	errc := make(chan error, 1)
	go func() { errc <- c.Stream(ctx, "alice", out) }()
	select {
	case ev := <-out:
		if ev.Type != "hello" {
			t.Fatalf("first event type=%q want hello", ev.Type)
		}
		// Server sends `now: null` until a load happens, so Now == "" is the
		// correct wire shape; what matters is that the SSE + json round-trip
		// produced an event with the expected type and a queue field.
		if ev.Queue == nil {
			t.Fatalf("hello missing queue: %+v", ev)
		}
	case <-ctx.Done():
		t.Fatal("no hello within 5s")
	}
	cancel()
}

func TestM1_FilesIncludesSpaces(t *testing.T) {
	port, srv := freePort(t)
	defer srv.Process.Kill()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	c := room.New(base, "obs-1234")
	files, err := c.Files()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"sample.mkv": true, "with space.mp4": true, "film 2.mov": true}
	for _, f := range files {
		if !want[f] {
			t.Fatalf("unexpected file: %q (list=%v)", f, files)
		}
		delete(want, f)
	}
	if len(want) != 0 {
		t.Fatalf("missing files: %v (got %v)", want, files)
	}
}

func TestM1_PostRoundTripAndOwnEchoFiltered(t *testing.T) {
	port, srv := freePort(t)
	defer srv.Process.Kill()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	me := "alice-1234"
	c := room.New(base, me)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	out := make(chan room.Event, 16)
	errc := make(chan error, 1)
	go func() { errc <- c.Stream(ctx, "alice", out) }()
	<-out // drain hello

	// post a chat from us; own-echo filter must drop it (only hello passes)
	if err := c.Post(room.Event{Type: "chat", Nick: "alice", Text: "self"}); err != nil {
		t.Fatal(err)
	}

	// post a chat from a remote id by hand; the stream must surface it
	go func() {
		body, _ := json.Marshal(room.Event{Type: "chat", From: "bob-9", Nick: "bob", Text: "remote-selam"})
		resp, _ := http.Post(base+"/cmd", "application/json", strings.NewReader(string(body)))
		if resp != nil {
			resp.Body.Close()
		}
	}()

	deadline := time.After(4 * time.Second)
	for {
		select {
		case ev := <-out:
			if ev.Type == "chat" {
				if ev.Nick == "alice" {
					t.Fatalf("own chat echoed (filter broken): %+v", ev)
				}
				if ev.Nick == "bob" && ev.Text == "remote-selam" {
					return // pass
				}
			}
		case <-deadline:
			t.Fatal("did not receive remote chat within 4s")
		}
	}
}

func TestM1_CmdParseParity(t *testing.T) {
	// plan: verbs that take no argument ignore extra text (client.py parity)
	cases := []struct {
		in    string
		kind  string
		arg   string
		isErr bool
	}{
		{"list foo", cmd.List, "", false},
		{"skip bar", cmd.Skip, "", false},
		{"play", "", "", true},
		{"chat", "", "", true},
		{"chat " + strings.Repeat("x", cmd.ChatMaxLen), cmd.Chat, strings.Repeat("x", cmd.ChatMaxLen), false},
		{"chat " + strings.Repeat("x", cmd.ChatMaxLen+1), "", "", true},
	}
	for _, tc := range cases {
		got, err := cmd.Parse(tc.in)
		if tc.isErr {
			if err == nil {
				t.Errorf("%q: want error, got %+v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error: %v", tc.in, err)
			continue
		}
		if got.Kind != tc.kind || got.Arg != tc.arg {
			t.Errorf("%q: got {%s %q}, want {%s %q}", tc.in, got.Kind, got.Arg, tc.kind, tc.arg)
		}
	}
}
