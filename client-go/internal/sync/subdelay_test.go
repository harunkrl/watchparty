package sync

// v1.8.0: the sub-delay surfaces are LOCAL — the personal-subtitle law
// (mute_test/f5 pattern). The essential pin is the NO-POST half: the
// player calls must land and the wire must stay silent.

import "testing"

func (f *fakePlayer) AddSubDelay(delta float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subAdds = append(f.subAdds, delta)
	return nil
}

func (f *fakePlayer) SetSubDelay(v float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subSets = append(f.subSets, v)
	return nil
}

// subDelayCalls returns the recorded nudges and absolute sets safely: the
// post worker can touch the player on its own goroutine.
func (f *fakePlayer) subDelayCalls() (adds, sets []float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]float64(nil), f.subAdds...), append([]float64(nil), f.subSets...)
}

func TestSubDelayChangesReachMpvAndNeverPost(t *testing.T) {
	e, pl, po := newEngine(true)
	e.SubDelayBy(0.5)
	e.SubDelayBy(-0.5)
	e.SubDelaySet(0.35)
	e.SubDelaySet(0)
	adds, sets := pl.subDelayCalls()
	if len(adds) != 2 || adds[0] != 0.5 || adds[1] != -0.5 {
		t.Fatalf("sub-delay nudges: %v, want [0.5 -0.5]", adds)
	}
	if len(sets) != 2 || sets[0] != 0.35 || sets[1] != 0 {
		t.Fatalf("sub-delay sets: %v, want [0.35 0]", sets)
	}
	// the essential half: the wire stayed silent (a subtitle delay is
	// personal — telling the room would fight every other viewer's sync)
	if evs := po.all(t); len(evs) != 0 {
		t.Fatalf("a sub-delay change told the room: %v", evType(t, evs))
	}
}
