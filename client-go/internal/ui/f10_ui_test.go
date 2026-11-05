package ui

// F10 client pins (ui half) — C4 (/sub rides the tell lane), C6/W2-06
// (the volume OSD dies with its action), C5 (setFile clears the stale
// clock). Each was RED on the v1.9.1 wiring (evidence:
// .pi-state/analysis10/*.md + .pi-state/f10-worker1-raw.md).

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
	"watchparty/client-go/internal/sync"
)

// The sync.Player interface grew AttachSub (F10 C4): every test-double
// implementor needs the method.
func (p *recPlayer) AttachSub(name string) error {
	p.subNames = append(p.subNames, name)
	return nil
}

func (stubPlayer) AttachSub(string) error { return nil }

func (p *matrixPlayer) AttachSub(string) error { return nil }

// f10VolPlayer: a recPlayer whose SetVolume can WEDGE (the drop probe) and
// whose OSD texts are recorded (the survive-together probe). Sequential
// like every recPlayer user — no mutex (the house style).
type f10VolPlayer struct {
	recPlayer
	osdText []string
	block   chan struct{} // when set, SetVolume blocks until closed
}

func (p *f10VolPlayer) SetVolume(delta float64) error {
	p.vols = append(p.vols, delta)
	if p.block != nil {
		<-p.block
	}
	return nil
}

func (p *f10VolPlayer) ShowText(s string, ms int) error {
	p.osdText = append(p.osdText, s)
	return nil
}

func (p *f10VolPlayer) volsN() int {
	return len(p.vols)
}

func (p *f10VolPlayer) osdN() int {
	return len(p.osdText)
}

// PIN F10.10 (C4 / W2-03): the /sub attach rides the engine's personal
// tell lane — the UI Update returns instantly even though the player is
// wedged, and the room never hears a subtitle choice (personal law).
func TestF10C4SubAttachRidesTellLane(t *testing.T) {
	pl := &recPlayer{pos: 10, hasPos: true}
	posted := make(chan room.Event, 8)
	e := &sync.Engine{
		Master: true, ID: "ada-1", Player: pl,
		Clock: sync.RealClock{},
		Post:  func(ev room.Event) error { posted <- ev; return nil },
	}
	m := New("ada", nil, nil, true, e)
	m.now = "movie.mkv"

	done := make(chan Model, 1)
	go func() {
		done <- upd(t, m, subMsg{video: "movie.mkv", name: "movie.tr.srt", subs: []string{"movie.tr.srt"}})
	}()
	select {
	case m = <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("C4: the UI Update blocked on the sub attach (the W2-03 raw write is still there)")
	}
	waitTells(t, e)
	if len(pl.subNames) != 1 || pl.subNames[0] != "movie.tr.srt" {
		t.Fatalf("C4: the attach did not land on the player via the tell lane: %v", pl.subNames)
	}
	select {
	case ev := <-posted:
		t.Fatalf("C4: a subtitle attach leaked to the room: %v", ev.Type)
	default:
	}
}

// PIN F10.11 (C6 / W2-06): the volume OSD travels INSIDE the action tell —
// drop-oldest kills them together. v1.9.1 let an OSD survive a dropped
// action (the mpv window claimed "vol +5%" while the volume never moved).
func TestF10C6VolumeOSDDiesWithItsAction(t *testing.T) {
	pl := &f10VolPlayer{block: make(chan struct{})}
	e := &sync.Engine{Master: true, ID: "ada-1", Player: pl, Clock: sync.RealClock{},
		Post: func(ev room.Event) error { return nil }}
	m := New("ada", nil, nil, true, e)

	// 12 wedged ctrl+up presses: 12 action+OSD tells (the fix) vs 24 loose
	// tells (v1.9.1). The personal cap 8 drop-oldest then leaves exactly
	// the surviving pairs equal.
	for i := 0; i < 12; i++ {
		m = upd(t, m, tea.KeyMsg{Type: tea.KeyCtrlUp})
	}

	close(pl.block) // the wedge ends: the queue drains
	waitTells(t, e)
	v, o := pl.volsN(), pl.osdN()
	if v == 0 {
		t.Fatalf("C6: no volume change survived (osds=%d) — OSDs outliving dropped actions", o)
	}
	if v != o {
		t.Fatalf("C6: action/OSD split under drop-oldest: %d volume changes vs %d OSDs", v, o)
	}
}

// PIN F10.12 (C5): setFile clears the stale clock — the honest gap.
func TestF10C5SetFileClearsStaleClock(t *testing.T) {
	m, _ := f9model(t)
	old := 12.5
	m.st = &sync.UiState{Pos: &old, Room: true}
	m, _ = m.setFile("b.mp4")
	if m.st != nil {
		t.Fatalf("C5: the old film's position survived the file change: %v", *m.st.Pos)
	}
}

// the mpv import keeps the concrete type in scope for the harness above
var _ *mpv.Player = nil
