package sync

// MPV1: a truncated property batch must not produce a heartbeat. The
// room's clock must not jump on a partial read — and if this guard ever
// disappears, the room clock silently drifts back and nobody notices.

import (
	"testing"

	"watchparty/client-go/internal/mpv"
)

func TestPartialSamplePostsNoHeartbeat(t *testing.T) {
	e, _, po := newEngine(true)
	e.OnMpvp(mpv.State{Pos: ptr(10), Partial: true})
	for _, ev := range po.all(t) {
		if ev.Type == "position" {
			t.Fatalf("a partial sample posted a heartbeat: %+v", ev)
		}
	}
}
