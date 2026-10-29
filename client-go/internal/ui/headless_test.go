package ui

// C8: the headless adapter must speak the TUI's language — bare text is
// chat, only a verb is a command.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"watchparty/client-go/internal/room"
	"watchparty/client-go/internal/sync"
)

// stubPlayer satisfies sync.Player: the headless chat path never touches
// mpv, but the engine would call it on a room event.
type stubPlayer struct{}

func (stubPlayer) Load(string) error          { return nil }
func (stubPlayer) SetPause(bool) error        { return nil }
func (stubPlayer) Seek(float64) error         { return nil }
func (stubPlayer) Speed(float64) error        { return nil }
func (stubPlayer) ShowText(string, int) error { return nil }
func (stubPlayer) Pos() (float64, bool)       { return 0, false }

func TestHeadlessBareTextIsChat(t *testing.T) {
	posted := make(chan room.Event, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cmd" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var ev room.Event
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			t.Errorf("bad /cmd body: %v", err)
			return
		}
		posted <- ev
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	api := room.New(srv.URL, "ada-1")
	engine := &sync.Engine{
		Player: stubPlayer{},
		Post:   func(ev room.Event) error { return api.Post(ev) },
		Clock:  sync.RealClock{},
	}
	out := &bytes.Buffer{}
	// "hello there" is not a verb: it must reach the room as chat
	if err := Run("ada", api, nil, engine, make(chan room.Event), strings.NewReader("hello there\n"), out); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-posted:
		if ev.Type != "chat" || ev.Nick != "ada" || ev.Text != "hello there" {
			t.Fatalf("bare text did not become chat: %+v", ev)
		}
	default:
		t.Fatalf("nothing was posted: %q", out.String())
	}
	if !strings.Contains(out.String(), "[ada] hello there") {
		t.Fatalf("local echo missing: %q", out.String())
	}
}

// syncBuffer is a bytes.Buffer that survives the headless printer goroutine.
type syncBuffer struct {
	mu  gosync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// C-F parity: the headless adapter prints the same actor line as the TUI
// ("> pause by bo"), and our own action stays bare.
func TestHeadlessPrintsActorLines(t *testing.T) {
	api := room.New("http://127.0.0.1:1", "ada-1")
	engine := &sync.Engine{
		Player: stubPlayer{},
		Post:   func(ev room.Event) error { return nil },
		Clock:  sync.RealClock{},
	}
	out := &syncBuffer{}
	ch := make(chan room.Event, 2)
	ch <- room.Event{Type: "pause", Nick: "bo"}
	ch <- room.Event{Type: "pause", Nick: "ada"}
	close(ch)
	if err := Run("ada", api, nil, engine, ch, strings.NewReader(""), out); err != nil {
		t.Fatal(err)
	}
	// the room printer runs in its own goroutine: wait for it
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "> pause by bo") && strings.Contains(out.String(), "> pause\n") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := out.String()
	if !strings.Contains(got, "> pause by bo") {
		t.Fatalf("headless lost the actor: %q", got)
	}
	if !strings.Contains(got, "> pause\n") {
		t.Fatalf("our own action must stay bare: %q", got)
	}
}
