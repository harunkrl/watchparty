package sync

import "testing"

// G1: SeekTo must move the local player AND tell the room: the server
// never echoes a poster's own event back, so a POST-only seek does
// nothing for the clicker.
func TestSeekToSeeksLocalAndPosts(t *testing.T) {
	e, pl, po := newEngine(true)
	e.SeekTo(42)
	if len(pl.seeks) != 1 || pl.seeks[0] != 42 {
		t.Fatalf("local seek = %v, want [42]", pl.seeks)
	}
	if len(po.events) != 1 || po.events[0].Type != "seek" || po.events[0].T != 42 {
		t.Fatalf("posted = %v, want one seek at 42s", po.events)
	}
}

// SeekTo clamps a negative target: the server rejects it.
func TestSeekToClampsNegative(t *testing.T) {
	e, pl, po := newEngine(true)
	e.SeekTo(-5)
	if len(pl.seeks) != 1 || pl.seeks[0] != 0 {
		t.Fatalf("local seek = %v, want [0]", pl.seeks)
	}
	if len(po.events) != 1 || po.events[0].T != 0 {
		t.Fatalf("posted = %v, want a seek at 0s", po.events)
	}
}
