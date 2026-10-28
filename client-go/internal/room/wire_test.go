package room_test

// C7: t and p must always be on the wire. With omitempty a seek to 0 and
// a paused=false heartbeat lost their payload and vanished (the server
// gates on `typeof t === "number"`).

import (
	"encoding/json"
	"testing"

	"watchparty/client-go/internal/room"
)

func TestWireAlwaysCarriesTandP(t *testing.T) {
	for _, ev := range []room.Event{
		{Type: "seek"},     // seek to 0: the payload used to vanish
		{Type: "position"}, // paused=false: the flag used to vanish
		{Type: "chat", Text: "x"},
	} {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if _, ok := m["t"]; !ok {
			t.Fatalf("%+v serialized without t: %s", ev, b)
		}
		if _, ok := m["p"]; !ok {
			t.Fatalf("%+v serialized without p: %s", ev, b)
		}
	}
}

// The server's idle notice carries `from: null` (S2). A JSON null must
// not break the decode: From stays empty, so the own-echo filter still
// lets the notice through.
func TestNoticeWithNullFromDecodes(t *testing.T) {
	var ev room.Event
	if err := json.Unmarshal([]byte(`{"type":"notice","from":null,"text":"nothing playing; queue keeps waiting"}`), &ev); err != nil {
		t.Fatalf("a null from broke the decode: %v", err)
	}
	if ev.Type != "notice" || ev.From != "" || ev.Text == "" {
		t.Fatalf("bad decode: %+v", ev)
	}
}

// S3: the duration rides the heartbeat, but only while it is known — an
// unknown duration must not reach the wire at all (old servers ignore it,
// the server keeps its last known value).
func TestDurOnTheWireOnlyWhenKnown(t *testing.T) {
	b, err := json.Marshal(room.Event{Type: "position", T: 10})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["dur"]; ok {
		t.Fatalf("an unknown duration reached the wire: %s", b)
	}
	b2, err := json.Marshal(room.Event{Type: "position", T: 10, Dur: 120})
	if err != nil {
		t.Fatal(err)
	}
	var m2 map[string]any
	if err := json.Unmarshal(b2, &m2); err != nil {
		t.Fatal(err)
	}
	if m2["dur"] != 120.0 {
		t.Fatalf("a known duration did not reach the wire: %s", b2)
	}
}
