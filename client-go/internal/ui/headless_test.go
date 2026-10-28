package ui

// C8: the headless adapter must speak the TUI's language — bare text is
// chat, only a verb is a command.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
