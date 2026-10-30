package room

// F3: the position heartbeats carry the file they describe (faz-3 gate,
// #49a). The wire shape matters: an old client (no file field) must keep
// the old behaviour, so the field is omitted when it is unknown.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPositionCarriesTheFileOnTheWire(t *testing.T) {
	b, err := json.Marshal(Event{Type: "position", T: 12.5, P: false, File: "movie.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"file":"movie.mkv"`) {
		t.Fatalf("the file did not reach the wire: %s", b)
	}
}

func TestPositionWithoutAFileStaysOldShaped(t *testing.T) {
	b, err := json.Marshal(Event{Type: "position", T: 12.5})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"file"`) {
		t.Fatalf("an unknown file must stay off the wire: %s", b)
	}
}
