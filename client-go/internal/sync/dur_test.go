package sync

// S3 completion: the master reports the media duration with its heartbeat
// so the server can clamp a long-downtime join before EOF. Without a
// sender the server's stored duration stays dead in production.

import (
	"testing"

	"watchparty/client-go/internal/mpv"
)

func TestHeartbeatCarriesDuration(t *testing.T) {
	e, _, po := newEngine(true)
	e.OnMpvp(mpv.State{Pos: ptr(10), Dur: 120})
	found := false
	for _, ev := range po.all(t) {
		if ev.Type == "position" && ev.Dur == 120 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the heartbeat carried no duration: %v", evType(t, po.all(t)))
	}
}

func TestHeartbeatWithoutDurationCarriesNone(t *testing.T) {
	e, _, po := newEngine(true)
	e.OnMpvp(mpv.State{Pos: ptr(10)}) // duration still unknown
	for _, ev := range po.all(t) {
		if ev.Type == "position" && ev.Dur != 0 {
			t.Fatalf("an unknown duration was reported: %+v", ev)
		}
	}
}
