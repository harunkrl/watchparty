package mpv

// v1.8.0: subCandidates is the one "what belongs to this video" rule —
// AttachSubs attaches from it and /sub lists from it, so the two surfaces
// can never disagree. The ordering contract: the plain <stem>.srt first
// (it is the selected default), then <stem>.ass, then the <stem>.* family
// in server order; anything outside the family stays out.
//
// The tell pins at the bottom assert the exact WIRE commands: mpv
// disables property expansion on the JSON array API by default, so the
// sub-delay hint's expand-properties prefix is a behaviour, not a detail.

import (
	"fmt"
	"net/url"
	"reflect"
	"testing"
	"time"
)

func TestSubCandidatesKeepsTheFamilyOrdered(t *testing.T) {
	subs := []string{ // server order (sorted): en < es < tr
		"b-movie.en.srt", "movie.ass", "movie.en.srt", "movie.es.srt", "movie.srt", "movie.tr.srt", "other.srt",
	}
	got := subCandidates(subs, "movie.mkv")
	want := []string{"movie.srt", "movie.ass", "movie.en.srt", "movie.es.srt", "movie.tr.srt"}
	if len(got) != len(want) {
		t.Fatalf("subCandidates: %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("subCandidates: %v, want %v", got, want)
		}
	}
	if got := subCandidates(subs, "nothing.mkv"); len(got) != 0 {
		t.Fatalf("a file without sidecars must list none: %v", got)
	}
}

// newRecordingPlayer is newFakePlayer plus a command recorder (a wedged
// mpv: tells never get answers, which is exactly what tells expect).
func newRecordingPlayer(t *testing.T) (*Player, *pipeIPC) {
	t.Helper()
	p, fake := newFakePlayer(t, nil)
	fake.tells = make(chan []any, 8)
	return p, fake
}

func waitTell(t *testing.T, fake *pipeIPC) []any {
	t.Helper()
	select {
	case cmd := <-fake.tells:
		return cmd
	case <-time.After(3 * time.Second):
		t.Fatal("the command never reached the wire")
		return nil
	}
}

// The sub-delay hint must ride the expand-properties prefix (the array
// API expands nothing by default — the user would see the literal
// ${sub-delay}), and the shared ShowText must stay prefix-free: it also
// renders other users' chat, and a chat line containing ${...} must
// never be expanded. The template carries no unit: mpv renders the delay
// in its own milliseconds format ("500 ms").
func TestSubDelayHintTellsExpandProperties(t *testing.T) {
	p, fake := newRecordingPlayer(t)
	if err := p.ShowSubDelayHint(); err != nil {
		t.Fatalf("the hint command failed: %v", err)
	}
	cmd := waitTell(t, fake)
	want := []any{"expand-properties", "show-text", "sub-delay ${sub-delay}", float64(1500)}
	if !reflect.DeepEqual(cmd, want) {
		t.Fatalf("hint command %v, want %v", cmd, want)
	}
	if err := p.ShowText("bo: hi ${filename}", 1500); err != nil { // chat shape
		t.Fatalf("ShowText failed: %v", err)
	}
	cmd = waitTell(t, fake)
	if len(cmd) > 0 && cmd[0] == "expand-properties" {
		t.Fatalf("the shared ShowText must not expand (chat could carry ${...}): %v", cmd)
	}
}

// The /sub attach is a sub-add tell with the select flag — never a
// loadfile (the film must keep playing).
func TestAttachSubTellsSubAddSelect(t *testing.T) {
	p, fake := newRecordingPlayer(t)
	if err := p.AttachSub("movie.tr.srt"); err != nil {
		t.Fatalf("the attach command failed: %v", err)
	}
	cmd := waitTell(t, fake)
	want := []any{"sub-add", fmt.Sprintf("%s/sub/%s", p.base, url.PathEscape("movie.tr.srt")), "select"}
	if !reflect.DeepEqual(cmd, want) {
		t.Fatalf("attach command %v, want %v", cmd, want)
	}
}
