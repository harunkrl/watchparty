package sync

import (
	"testing"
	"time"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
)

func TestDbgF11C7(t *testing.T) {
	e, pl, _ := newEngine(false)
	g := &f11Post{}
	gate := make(chan struct{})
	g.gate = gate
	e.Post = g.post
	e.Logf = func(f string, a ...any) { t.Logf(f, a...) }
	pos := 10.0
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false})
	waitTells(t, e)
	e.OnRoom(room.Event{Type: "hello", Now: "f.mkv", Pos: &pos, P: false})
	waitTells(t, e)
	e.OnMpvp(mpv.State{Pos: &pos, Pause: false, Dur: 120, At: time.Now()})
	waitTells(t, e)
	e.PauseToggle()
	time.Sleep(100 * time.Millisecond)
	e.mu.Lock()
	t.Logf("blocked phase: attempts=%d posted=%v", g.triedCount(), e.postedPause != nil)
	e.mu.Unlock()
	e.OnRoom(room.Event{Type: "position", T: 12, P: true})
	close(gate)
	waitTells(t, e)
	time.Sleep(200 * time.Millisecond)
	e.mu.Lock()
	t.Logf("after release: attempts=%d retryTyp=%q snapPaused=%v localPause=%v mpvPauses=%v",
		g.triedCount(), e.retryTyp, e.snap != nil && e.snap.paused, e.localPause, pl.pausesSnapshot())
	e.mu.Unlock()
	t.Logf("posted events: %v", g.log())
}
