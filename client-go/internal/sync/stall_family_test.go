package sync

// Batch C: the stall family. A follower may only pause the room when
// the master is really heartbeating AND the follower is really
// starving: a silent master and a resume (sleep/suspend) are not
// starvation (m2-S1, m2-S2).

import (
	"testing"
	"time"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
)

// m2-S1: a silent master is not a starving follower. Without a fresh
// heartbeat there is no live reference to converge to, and pausing the
// room would punish the follower for the master's silence.
func TestSilentMasterDoesNotPauseTheRoom(t *testing.T) {
	e, pl, po := newEngine(false)
	t0 := time.Unix(5000, 0)
	e.Clock = stepClock{now: t0}
	e.OnRoom(masterAt(10, false)) // the master's last heartbeat
	e.Clock = stepClock{now: t0.Add(10 * time.Second)}
	// we are 10s behind (expected 20) and stay there
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false})
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false})
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false})
	if n := countType(t, po.all(t), "pause"); n != 0 {
		t.Fatalf("the follower paused the room for a silent master: %v", evType(t, po.all(t)))
	}
	if len(pl.seeks) != 0 {
		t.Fatalf("the follower chased a stale snapshot: %v", pl.seeks)
	}
}

// m2-S1: a master that comes back must be able to hand the clock over
// again — the silence must not have poisoned the follower's state.
func TestSilentMasterThenHeartbeatRecovers(t *testing.T) {
	e, _, po := newEngine(false)
	t0 := time.Unix(5000, 0)
	e.Clock = stepClock{now: t0}
	e.OnRoom(masterAt(10, false))
	e.Clock = stepClock{now: t0.Add(10 * time.Second)}
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false})
	// the master is back
	e.Clock = stepClock{now: t0.Add(11 * time.Second)}
	e.OnRoom(masterAt(21, false))
	e.Clock = stepClock{now: t0.Add(12 * time.Second)}
	e.OnMpvp(mpv.State{Pos: ptr(21), Pause: false}) // expected 22: on live
	if n := countType(t, po.all(t), "pause"); n != 0 {
		t.Fatalf("the room was paused during the outage: %v", evType(t, po.all(t)))
	}
}

// m2-S2: a resume (sleep/suspend/long outage) is a big jump, not
// starvation: the follower re-seeks and never pauses the room for it.
func TestResumeDriftSeeksWithoutPausing(t *testing.T) {
	e, pl, po := newEngine(false)
	t0 := time.Unix(5000, 0)
	e.Clock = stepClock{now: t0}
	e.OnRoom(masterAt(100, false)) // the master is at 100s
	e.Clock = stepClock{now: t0.Add(2 * time.Second)}
	e.OnMpvp(mpv.State{Pos: ptr(40), Pause: false}) // we are 62s behind: a resume
	if len(pl.seeks) != 1 {
		t.Fatalf("the resume did not re-seek: %v", pl.seeks)
	}
	if pl.seeks[0] < 100 || pl.seeks[0] > 103 {
		t.Fatalf("re-seek to %v, want the live position (~102)", pl.seeks[0])
	}
	if n := countType(t, po.all(t), "pause"); n != 0 {
		t.Fatalf("the resume paused the room: %v", evType(t, po.all(t)))
	}
	// the settle window: no seek storm, no pause
	e.OnMpvp(mpv.State{Pos: ptr(40), Pause: false})
	e.OnMpvp(mpv.State{Pos: ptr(40), Pause: false})
	if len(pl.seeks) != 1 {
		t.Fatalf("seek storm after the resume: %v", pl.seeks)
	}
	if n := countType(t, po.all(t), "pause"); n != 0 {
		t.Fatalf("the resume paused the room: %v", evType(t, po.all(t)))
	}
}

// The honest starvation still pauses the room — the master is alive and
// the follower cannot keep up.
func TestRealStallStillPausesTheRoom(t *testing.T) {
	e, _, po := newEngine(false)
	t0 := time.Unix(5000, 0)
	e.Clock = stepClock{now: t0}
	e.OnRoom(masterAt(100, false))
	e.Clock = stepClock{now: t0.Add(2 * time.Second)}
	e.OnMpvp(mpv.State{Pos: ptr(95), Pause: false}) // 7s behind: not a resume jump
	e.OnMpvp(mpv.State{Pos: ptr(95), Pause: false})
	if n := countType(t, po.all(t), "pause"); n != 1 {
		t.Fatalf("the honest stall did not pause the room: %v", evType(t, po.all(t)))
	}
}

// m2-S2: the stall pause is mechanical — nobody pressed anything, so it
// must not be shown as "paused by <nick>".
func TestStallPauseIsMechanical(t *testing.T) {
	e, _, po := newEngine(false)
	t0 := time.Unix(5000, 0)
	e.Clock = stepClock{now: t0}
	e.OnRoom(masterAt(100, false))
	e.Clock = stepClock{now: t0.Add(2 * time.Second)}
	e.OnMpvp(mpv.State{Pos: ptr(95), Pause: false})
	e.OnMpvp(mpv.State{Pos: ptr(95), Pause: false})
	for _, ev := range po.all(t) {
		if ev.Type == "pause" && !ev.Mechanical {
			t.Fatalf("the stall pause is not marked mechanical: %+v", ev)
		}
	}
	// the initiator's own UI says so too
	ui := make(chan UiState, 8)
	e.Ui = ui
	e.OnMpvp(mpv.State{Pos: ptr(95), Pause: false})
	st := <-ui
	if !st.Mechanical {
		t.Fatalf("the initiator does not see a mechanical pause: %+v", st)
	}
	if st.PausedBy != "" {
		t.Fatalf("the mechanical pause named %q", st.PausedBy)
	}
}

// The receiving side must not blame a human for a mechanical pause.
func TestMechanicalPauseNamesNobody(t *testing.T) {
	ui := make(chan UiState, 4)
	e, _, _ := newEngine(true) // the master receives the follower's stall pause
	e.Ui = ui
	e.OnRoom(room.Event{Type: "pause", Nick: "bob", Mechanical: true})
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: true})
	st := <-ui
	if st.PausedBy != "" {
		t.Fatalf("the mechanical pause named %q", st.PausedBy)
	}
	if !st.Mechanical {
		t.Fatalf("the mechanical pause was not marked: %+v", st)
	}
}

// A human pause still names the actor — the mechanical marker must not
// swallow the real attribution.
func TestHumanPauseStillNamesTheActor(t *testing.T) {
	ui := make(chan UiState, 4)
	e, _, _ := newEngine(true)
	e.Ui = ui
	e.OnRoom(room.Event{Type: "pause", Nick: "bob"})
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: true})
	st := <-ui
	if st.PausedBy != "bob" {
		t.Fatalf("the actor was lost: %+v", st)
	}
	if st.Mechanical {
		t.Fatalf("a human pause was marked mechanical: %+v", st)
	}
}

// (A) a silent master in a PAUSED room: the follower must still converge
// to the pause (local, reversible — setPause posts nothing). The stale
// gate stops wire actions, it must not keep the follower playing.
func TestSilentMasterPausedRoomStillConverges(t *testing.T) {
	e, pl, po := newEngine(false)
	t0 := time.Unix(5000, 0)
	e.Clock = stepClock{now: t0}
	e.OnRoom(masterAt(10, true)) // the room is paused
	e.Clock = stepClock{now: t0.Add(10 * time.Second)}
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false}) // we are still playing
	if len(pl.pauses) != 1 || !pl.pauses[0] {
		t.Fatalf("the follower did not converge to the pause: %v", pl.pauses)
	}
	if n := countType(t, po.all(t), "pause"); n != 0 {
		t.Fatalf("the follower posted to the wire: %v", evType(t, po.all(t)))
	}
}

// (B) the settle window must not outlive the file: a stale window could
// silence the next file's stall detection for 30s.
func TestResumeSettleDoesNotSurviveANewFile(t *testing.T) {
	e, _, po := newEngine(false)
	t0 := time.Unix(5000, 0)
	e.Clock = stepClock{now: t0}
	e.OnRoom(masterAt(100, false))
	e.Clock = stepClock{now: t0.Add(2 * time.Second)}
	e.OnMpvp(mpv.State{Pos: ptr(40), Pause: false}) // a resume: the window opens
	e.Clock = stepClock{now: t0.Add(3 * time.Second)}
	e.OnRoom(room.Event{Type: "load", Name: "b.mkv"}) // the room moves on
	e.Clock = stepClock{now: t0.Add(6 * time.Second)}
	e.OnRoom(masterAt(10, false))
	e.OnMpvp(mpv.State{Pos: ptr(2), Pause: false}) // 8s behind
	e.Clock = stepClock{now: t0.Add(10 * time.Second)}
	e.OnRoom(masterAt(14, false))
	e.OnMpvp(mpv.State{Pos: ptr(2), Pause: false}) // still behind -> stall
	if n := countType(t, po.all(t), "pause"); n != 1 {
		t.Fatalf("the new file's stall was swallowed by the stale settle window: %v", evType(t, po.all(t)))
	}
}
