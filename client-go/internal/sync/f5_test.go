package sync

import (
	"testing"
	"time"

	"watchparty/client-go/internal/room"
)

func (f *fakePlayer) SetVolume(delta float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vols = append(f.vols, delta)
	return nil
}

func (f *fakePlayer) ToggleMute() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mutes++
	return nil
}

func (f *fakePlayer) volumeCalls() []float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]float64(nil), f.vols...)
}

func (f *fakePlayer) muteCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mutes
}

// F5: the volume keys are LOCAL — audio belongs to the viewer's ears; a
// master volume change must never reach the room. The essential pin is the
// NO-POST half: SetVolume/ToggleMute must be called and the wire must stay
// silent.

func TestVolumeKeysNudgeLocallyAndNeverPost(t *testing.T) {
	pl := &fakePlayer{}
	posted := make(chan room.Event, 8)
	e := &Engine{
		Master: true, ID: "ada-1", Player: pl,
		Clock: RealClock{},
		Post:  func(ev room.Event) error { posted <- ev; return nil },
	}
	e.VolumeUp()
	e.VolumeDown()
	e.VolumeMute()
	vols, mutes := pl.volumeCalls(), pl.muteCalls()
	if len(vols) != 2 || vols[0] != 5 || vols[1] != -5 {
		t.Fatalf("volume nudges: %v, want [5 -5]", vols)
	}
	if mutes != 1 {
		t.Fatalf("mute toggles: %d, want 1", mutes)
	}
	// the essential half: the wire stayed silent (a volume key that told
	// the room would fight every other viewer's volume)
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case ev := <-posted:
			t.Fatalf("a volume key told the room: %+v", ev)
		case <-time.After(60 * time.Millisecond):
		}
	}
}
