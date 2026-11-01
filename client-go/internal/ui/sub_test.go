package ui

// v1.8.0 subtitle round: the local sub-delay keys and /subdelay command,
// the /sub attach, and the numeric drift chip. Timing AND choice are
// personal — every pin here is half "mpv got it" and half "the wire (and
// the room) never heard it". The hint itself is a template mpv expands
// (Player.ShowSubDelayHint); the exact wire command is pinned in the mpv
// package (TestSubDelayHintTellsExpandProperties).

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"watchparty/client-go/internal/room"
	"watchparty/client-go/internal/sync"
)

func (p *recPlayer) AddSubDelay(delta float64) error {
	p.subAdds = append(p.subAdds, delta)
	return nil
}

func (p *recPlayer) SetSubDelay(v float64) error {
	p.subSets = append(p.subSets, v)
	return nil
}

func (p *recPlayer) subDelayCalls() (adds, sets []float64) {
	return p.subAdds, p.subSets
}

func (stubPlayer) AddSubDelay(float64) error { return nil }
func (stubPlayer) SetSubDelay(float64) error { return nil }

func (p *matrixPlayer) AddSubDelay(float64) error { return nil }
func (p *matrixPlayer) SetSubDelay(float64) error { return nil }

// subModel is the F5 model plus an OSD capture: the sub-delay hint is a
// template mpv expands on the wire (pinned in the mpv package), so the ui
// pin asserts the ASK, not the rendered value. The channel is the
// engine's Post: any room-bound event lands there for the no-post asserts.
func subModel(t *testing.T) (Model, *recPlayer, *[]string, chan room.Event) {
	t.Helper()
	pl := &recPlayer{pos: 10, hasPos: true}
	posted := make(chan room.Event, 8)
	e := &sync.Engine{
		Master: true, ID: "ada-1", Player: pl,
		Clock: sync.RealClock{},
		Post:  func(ev room.Event) error { posted <- ev; return nil },
	}
	m := New("ada", nil, nil, true, e)
	var osd []string
	m.osdHook = func(text string) { osd = append(osd, text) }
	return m, pl, &osd, posted
}

// assertSilent gives any wrongly-posted event time to arrive, then fails.
func assertSilent(t *testing.T, posted chan room.Event, what string) {
	t.Helper()
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case ev := <-posted:
			t.Fatalf("%s told the room: %+v", what, ev)
		case <-time.After(60 * time.Millisecond):
		}
	}
}

func submitLine(t *testing.T, m Model, line string) Model {
	t.Helper()
	m.input.SetValue(line)
	return upd(t, m, tea.KeyMsg{Type: tea.KeyEnter})
}

func TestAltArrowsNudgeSubDelayLocally(t *testing.T) {
	m, pl, osd, posted := subModel(t)
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyUp, Alt: true})   // +0.5
	m = upd(t, m, tea.KeyMsg{Type: tea.KeyDown, Alt: true}) // -0.5
	waitTells(t, m.engine)
	adds, sets := pl.subDelayCalls()
	if len(adds) != 2 || adds[0] != 0.5 || adds[1] != -0.5 {
		t.Fatalf("alt+arrows nudges: %v, want [0.5 -0.5]", adds)
	}
	if len(sets) != 0 {
		t.Fatalf("alt+arrows must nudge relatively, got sets %v", sets)
	}
	if len(*osd) != 2 {
		t.Fatalf("the mpv window got %d delay hints, want 2", len(*osd))
	}
	if v := m.input.Value(); v != "" {
		t.Fatalf("a sub-delay key leaked into the chat input: %q", v)
	}
	assertSilent(t, posted, "a sub-delay nudge")
}

func TestSubDelayCommandSetsAndResets(t *testing.T) {
	m, pl, osd, posted := subModel(t)
	m = submitLine(t, m, "/subdelay +0.35")
	m = submitLine(t, m, "/subdelay 0")
	waitTells(t, m.engine)
	_, sets := pl.subDelayCalls()
	if len(sets) != 2 || sets[0] != 0.35 || sets[1] != 0 {
		t.Fatalf("/subdelay sets: %v, want [0.35 0]", sets)
	}
	if len(*osd) != 2 {
		t.Fatalf("/subdelay gave %d delay hints, want 2", len(*osd))
	}
	m = submitLine(t, m, "/subdelay later")
	_, sets = pl.subDelayCalls()
	if len(sets) != 2 {
		t.Fatalf("a non-numeric /subdelay must be refused: %v", sets)
	}
	if m.lastErr == "" || !strings.Contains(m.lastErr, "number") {
		t.Fatalf("the refusal is not honest: %q", m.lastErr)
	}
	assertSilent(t, posted, "a /subdelay command")
}

func TestSubWithoutAFileSaysSo(t *testing.T) {
	m, _, _, _ := subModel(t)
	m.now = "" // nothing playing
	m = submitLine(t, m, "/sub")
	m = submitLine(t, m, "/sub movie.tr.srt")
	if m.lastErr == "" || !strings.Contains(m.lastErr, "no file playing") {
		t.Fatalf("bare room must refuse /sub honestly: %q", m.lastErr)
	}
}

// The /sub probe answers off-thread: the subMsg handler renders the list,
// refuses unknown names and cancels a probe that outlived the file.
// (The attach tell itself is pinned on the wire in the mpv package.)
func TestSubProbeAnswerRendersAndRefuses(t *testing.T) {
	m, _, _, _ := subModel(t)
	m.now = "movie.mkv"
	m = upd(t, m, subMsg{video: "movie.mkv", subs: []string{"movie.es.srt", "movie.tr.srt"}})
	log := strings.Join(logTexts(m.log), "\n")
	for _, want := range []string{"movie.es.srt", "movie.tr.srt", "attach with /sub <name>"} {
		if !strings.Contains(log, want) {
			t.Fatalf("the listing misses %q: %v", want, log)
		}
	}
	n := len(m.log)
	m = upd(t, m, subMsg{video: "movie.mkv", name: "movie.de.srt", subs: []string{"movie.tr.srt"}})
	if m.lastErr == "" || !strings.Contains(m.lastErr, "no such subtitle") {
		t.Fatalf("an unknown sub must be refused honestly: %q", m.lastErr)
	}
	// failf logs the refusal itself: exactly one new entry, no listing
	if len(m.log) != n+1 {
		t.Fatalf("a refusal must add only its own line: %v", logTexts(m.log))
	}
	m = upd(t, m, subMsg{video: "old.mkv", name: "movie.de.srt"}) // probe answered late
	if m.lastErr == "" || !strings.Contains(m.lastErr, "moved to") {
		t.Fatalf("a probe that outlived the file must be cancelled honestly: %q", m.lastErr)
	}
	n = len(m.log)
	m = upd(t, m, subMsg{video: "old.mkv"}) // a stale bare list is noise
	if len(m.log) != n {
		t.Fatalf("a stale listing must be dropped: %v", logTexts(m.log))
	}
}

// The in-flight guard (F7): a second /sub for the SAME video+name while
// the probe is still answering is refused — mpv's sub-add is not
// idempotent, so attaching twice creates a duplicate track; a DIFFERENT
// request stays allowed, and the answer clears the key. The test model
// has no concrete player, so the guard state is seeded the way
// requestSub itself sets it.
func TestSubInFlightGuard(t *testing.T) {
	m, _, _, _ := subModel(t)
	m.now = "movie.mkv"
	key := subKey{video: "movie.mkv", name: "movie.tr.srt"}
	m.subInFlight = map[subKey]bool{key: true}

	m, c := m.requestSub("movie.tr.srt") // same key, still answering
	if c != nil {
		t.Fatal("a duplicate /sub must be refused while the probe runs")
	}
	logs := logTexts(m.log)
	if !strings.Contains(logs[len(logs)-1], "already being looked up") {
		t.Fatalf("the refusal is not honest: %q", logs[len(logs)-1])
	}

	m2, _ := m.requestSub("movie.es.srt") // a DIFFERENT request must pass the guard
	last := logTexts(m2.log)
	if strings.Contains(last[len(last)-1], "already being looked up") {
		t.Fatal("the guard must be keyed: a different /sub stays allowed")
	}
	// ...and it must have reached the PLAYER check: the ui model's player
	// is a real *mpv.Player (the windows CI job has no mpv), so the
	// request stops there. That stop IS the proof that the guard let it
	// through — the guard's message would have fired first.
	if !strings.Contains(last[len(last)-1], "no file playing") {
		t.Fatalf("the different /sub did not get past the guard: %q", last[len(last)-1])
	}

	// the probe answer clears the key — the stale-cancel path included
	m = upd(t, m, subMsg{key: key, video: "old.mkv", name: "movie.tr.srt"})
	if m.subInFlight[key] {
		t.Fatal("the probe answer must clear the in-flight key")
	}
}

func TestDriftChipAtBandBoundaries(t *testing.T) {
	orig := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(orig)

	cases := []struct {
		d      float64
		text   string
		ansi16 string
	}{
		{0.1, "▸ +0.1s", "\x1b[32m"},   // inside the smooth-sync band
		{0.15, "▸ +0.1s", "\x1b[32m"},  // exactly at the amber edge stays green (d > 0.15, not >=)
		{0.16, "▸ +0.2s", "\x1b[33m"},  // tempo-nudge territory
		{-0.16, "▸ -0.2s", "\x1b[33m"}, // both directions
		{1.0, "▸ +1.0s", "\x1b[33m"},   // exactly at the red edge stays amber
		{1.04, "▸ +1.0s", "\x1b[31m"},  // hard re-seek territory (color ≠ rounding)
		{-2.3, "▸ -2.3s", "\x1b[31m"},
	}
	for _, tc := range cases {
		m := New("t", nil, nil, false, nil)
		p := 65.0
		m.st = &sync.UiState{Pos: &p, Drift: tc.d, Room: true} // F9: synced
		chip := m.zenSyncChip()
		if !strings.Contains(chip, tc.text) {
			t.Fatalf("drift %v: chip %q misses %q", tc.d, chip, tc.text)
		}
		if !strings.Contains(chip, tc.ansi16) {
			t.Fatalf("drift %v: chip %q misses band color %q", tc.d, chip, tc.ansi16)
		}
	}
}
