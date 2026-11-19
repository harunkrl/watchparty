package sync

// L1 lobby-first pins (decisions #86/#86a): an idle room's hello holds
// the last watched file back as a lobby instead of auto-loading it;
// ResumeLobby posts load -> pause (only when the room was paused — the
// server's load handler resets state.paused, F10 S5) -> seek after the
// file is live, and adopts through the SAME hello machinery. The gate
// pins were RED on the v1.9.3 tree (every hello auto-loaded); raw trail
// in .pi-state/l1-worker1-raw.md.

import (
	"testing"
	"time"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
)

func lobbyEngine(master bool) (*Engine, *fakePlayer, *fakePost, chan UiState) {
	e, pl, po := newEngine(master)
	ch := make(chan UiState, 8)
	e.Ui = ch
	return e, pl, po, ch
}

// idleHello is the idle-room hello the gate fires on: the single roster
// entry is always us (the server registers us before writing hello).
func idleHello() room.Event {
	return room.Event{
		Type: "hello", Now: "a.mkv", Roster: []string{"me"},
		Pos: ptr(120), P: true,
	}
}

// drainLobbyUi reads one UiState off the engine's channel (the gate
// pushes immediately so the card does not wait for the next poll).
func drainLobbyUi(t *testing.T, ch chan UiState) UiState {
	t.Helper()
	select {
	case st := <-ch:
		return st
	case <-time.After(time.Second):
		t.Fatal("the engine never surfaced a UiState")
		return UiState{}
	}
}

// PIN L1.1: an idle room's hello with a differing file must NOT load —
// it holds the file as a lobby (with the room's position and pause) and
// surfaces it on the Ui channel at once. RED on v1.9.3: the hello
// auto-loaded unconditionally.
func TestLobbyGateIdleHoldsBackLoad(t *testing.T) {
	e, pl, po, ch := lobbyEngine(false)
	e.OnRoom(idleHello())
	if pl.startLoadCount() != 0 {
		t.Fatalf("an idle room's hello auto-loaded — the gate did not hold")
	}
	st := drainLobbyUi(t, ch)
	if st.Lobby == nil {
		t.Fatalf("the gate never surfaced the lobby")
	}
	if st.Lobby.Now != "a.mkv" || st.Lobby.Pos == nil || *st.Lobby.Pos != 120 || !st.Lobby.Paused {
		t.Fatalf("lobby = %+v, want {a.mkv 120 paused}", st.Lobby)
	}
	// holding back tells the room nothing
	if evs := po.all(t); len(evs) != 0 {
		t.Fatalf("the gate posted %v", evType(t, evs))
	}
	// the engine's mirror is the same truth
	if e.lobby == nil || e.lobby.Now != "a.mkv" {
		t.Fatalf("engine lobby = %+v", e.lobby)
	}
}

// PIN L1.2: a LIVE room (2 unique nicks) keeps the core behavior byte-
// compatible: auto-load + adopt (the position waits as pendingPos while
// the duration is unknown — F1), and no lobby ever appears.
func TestLobbyGateLiveRoomLoads(t *testing.T) {
	e, pl, _, ch := lobbyEngine(false)
	e.OnRoom(room.Event{
		Type: "hello", Now: "a.mkv", Roster: []string{"me", "her"},
		Pos: ptr(120), P: true,
	})
	if pl.startLoadCount() == 0 {
		t.Fatalf("a live room's hello did not load — the core feature regressed")
	}
	if e.lobby != nil {
		t.Fatalf("a live room must never lobby")
	}
	if e.pendingPos == nil || *e.pendingPos != 120 {
		t.Fatalf("the live room's adoption lost the position: %+v", e.pendingPos)
	}
	select {
	case st := <-ch:
		if st.Lobby != nil {
			t.Fatalf("a live room surfaced a lobby: %+v", st.Lobby)
		}
	default: // no push is fine: nothing to surface
	}
}

// PIN L1.3: a SAME-FILE hello (idle or not) is never a lobby — the F3
// guard skips the reload and the pos/pause adoption keeps working (the
// solo SSE blip path).
func TestLobbySameFileHelloAdopts(t *testing.T) {
	e, pl, _, _ := lobbyEngine(false)
	e.loaded = "a.mkv" // mpv already plays the room's file (F3 shape)
	e.OnRoom(idleHello())
	if pl.startLoadCount() != 0 {
		t.Fatalf("a same-file hello reloaded the file (F3 regression)")
	}
	if e.lobby != nil {
		t.Fatalf("a same-file hello must never lobby")
	}
	if e.pendingPos == nil || *e.pendingPos != 120 {
		t.Fatalf("the same-file adoption lost the position: %+v", e.pendingPos)
	}
	waitTells(t, e) // the pause tell executes on the F8 tell worker
	pauses := pl.pausesSnapshot()
	if len(pauses) == 0 || !pauses[len(pauses)-1] {
		t.Fatalf("the same-file hello did not adopt the room's pause: %v", pauses)
	}
}

// PIN L1.4: ResumeLobby with no lobby refuses without touching the room
// (the visible refusal itself is the UI's — F9 yardstick).
func TestResumeLobbyRefusal(t *testing.T) {
	e, _, po, _ := lobbyEngine(false)
	if e.ResumeLobby() {
		t.Fatalf("ResumeLobby claimed success without a lobby")
	}
	if evs := po.all(t); len(evs) != 0 {
		t.Fatalf("a refused resume posted %v", evType(t, evs))
	}
}

// PIN L1.5 (the #86a named pin): a resume of a PAUSED lobby posts
// load FIRST (the handoff: other idle clients load through the existing
// load handler), pause SECOND (the server's load handler resets
// state.paused — the room must be told before anyone adopts), and seek
// LAST, only after the file is live. The adopt runs through the same
// hello machinery: the position waits as pendingPos for the F1 clamp.
func TestResumeLobbyPausedOrder(t *testing.T) {
	e, pl, po, ch := lobbyEngine(false)
	e.OnRoom(idleHello())
	drainLobbyUi(t, ch) // the gate push

	if !e.ResumeLobby() {
		t.Fatalf("ResumeLobby refused an existing lobby")
	}
	if e.lobby != nil {
		t.Fatalf("the resume did not clear the lobby (§4 choke point)")
	}
	select {
	case st := <-ch:
		if st.Lobby != nil {
			t.Fatalf("the clearing never surfaced: %+v", st.Lobby)
		}
	default:
	}
	if e.loaded != "a.mkv" {
		t.Fatalf("startLoad never claimed the file (loaded=%q)", e.loaded)
	}
	if e.pendingPos == nil || *e.pendingPos != 120 {
		t.Fatalf("the resume did not adopt through the hello machinery (pendingPos=%v)", e.pendingPos)
	}
	// startLoad actuates play (F11 C2), the adopt re-pauses (CASE B).
	// The two pause tells COALESCE to the latest (F8 synced-kind law —
	// the enqueue count is not an observable): the last state is what
	// the player must end in.
	waitTells(t, e) // the pause tells execute on the F8 tell worker
	pauses := pl.pausesSnapshot()
	if len(pauses) == 0 || pauses[len(pauses)-1] != true {
		t.Fatalf("the resumed file did not end paused: %v", pauses)
	}
	// the wire order: load -> pause -> seek, posted immediately (L1b G3:
	// the G1 park protects the peer; no WaitLive deferral remains).
	evs := po.all(t)
	types := evType(t, evs)
	if len(types) != 3 || types[0] != "load" || types[1] != "pause" || types[2] != "seek" {
		t.Fatalf("resume posts = %v, want [load pause seek]", types)
	}
	if evs[0].Name != "a.mkv" || evs[2].T != 120 {
		t.Fatalf("handoff payload wrong: %+v", evs)
	}
}

// PIN L1.6 (ruling #86a condition 1): a resume of a PLAYING lobby posts
// NO pause — load + seek only. A position-less lobby (the F9 CASE B
// server shape) skips the seek too.
func TestResumeLobbyPlayingPostsNoPause(t *testing.T) {
	e, pl, po, _ := lobbyEngine(false)
	e.OnRoom(room.Event{
		Type: "hello", Now: "a.mkv", Roster: []string{"me"}, P: false,
	})
	if !e.ResumeLobby() {
		t.Fatalf("ResumeLobby refused an existing lobby")
	}
	for _, ev := range po.all(t) {
		if ev.Type == "pause" {
			t.Fatalf("a playing lobby's resume posted a pause: %v", evType(t, po.all(t)))
		}
	}
	if n := len(pl.pausesSnapshot()); n != 1 || pl.pausesSnapshot()[0] != false {
		t.Fatalf("the player was not left to play the resumed file: %v", pl.pausesSnapshot())
	}
}

// PIN L1.7: a received load event clears the lobby AND loads (the
// handoff path — the other idle client's card dies when the room moves).
func TestReceivedLoadClearsLobby(t *testing.T) {
	e, pl, _, _ := lobbyEngine(false)
	e.OnRoom(idleHello())
	e.OnRoom(room.Event{Type: "load", Name: "b.mkv"})
	if e.lobby != nil {
		t.Fatalf("a received load event left the lobby up")
	}
	if e.loaded != "b.mkv" || pl.startLoadCount() == 0 {
		t.Fatalf("the handoff load never ran (loaded=%q)", e.loaded)
	}
}

// PIN L1.8: the picker's own play (OwnLoad — it bypasses startLoad)
// clears the lobby too: the §4 choke point is the load-bookkeeping
// entries, not one function.
func TestOwnLoadClearsLobby(t *testing.T) {
	e, _, _, _ := lobbyEngine(false)
	e.OnRoom(idleHello())
	e.OwnLoad("b.mkv")
	if e.lobby != nil {
		t.Fatalf("an own load left the lobby up")
	}
}

// PIN L1-X1 (#86b): a follower joining a PAUSED live room must land at
// the room's position and stay paused. The hello position parks as
// pendingPos (the F1 duration wait); the master's first heartbeat used
// to CLEAR it ('a fresh heartbeat supersedes') — and a paused room never
// drift-corrects, so 0:00 was forever. The heartbeat during the park
// window must UPDATE the pending value (it IS fresher truth), bounded by
// the first flush; after the flush a scrub while paused stays untouched
// (no pending → no seek → no paused-heartbeat seek loop).
func TestX1PausedRoomJoinLandsAtRoomPos(t *testing.T) {
	e, pl, _, _ := lobbyEngine(false)
	e.OnRoom(room.Event{
		Type: "hello", Now: "a.mkv", Roster: []string{"me", "her"},
		Pos: ptr(120), P: true,
	})
	if e.pendingPos == nil || *e.pendingPos != 120 {
		t.Fatalf("the hello position never parked: %+v", e.pendingPos)
	}
	// the master's heartbeat arrives while the position is parked: its
	// value is fresher room truth and must REPLACE the parked one
	e.OnRoom(room.Event{Type: "position", T: 125, P: true})
	if e.pendingPos == nil || *e.pendingPos != 125 {
		t.Fatalf("the parked position was not updated by the heartbeat: %+v", e.pendingPos)
	}
	// the duration becomes known: the flush lands us at the room position
	e.OnMpvp(mpv.State{Pos: ptr(0), Pause: true, Dur: 200})
	waitTells(t, e)
	seeked := pl.seeksSnapshot()
	if len(seeked) == 0 || seeked[len(seeked)-1] != 125 {
		t.Fatalf("the flush never seeked to the room position: %v", seeked)
	}
	// load-scoped: after the flush, heartbeats while paused must NOT seek
	// (a user scrubbing while paused stays where they put it)
	e.OnRoom(room.Event{Type: "position", T: 125, P: true})
	e.OnMpvp(mpv.State{Pos: ptr(125), Pause: true, Dur: 200})
	waitTells(t, e)
	if n := len(pl.seeksSnapshot()); n != len(seeked) {
		t.Fatalf("a paused heartbeat seeked after the flush (loop): %v", pl.seeksSnapshot())
	}
}

// --- L1b fix batch (#86c: F1 auto-join, G1 seek park, G3 immediate seek) ---

// PIN L1b-F1: a heartbeat while a lobby exists IS the "room went live"
// signal (#86c AUTO-JOIN): the card dies, the room's file loads, and the
// heartbeat's t/p adopts through the live path (parks in pendingPos for
// the F1 flush; dur is 0 right after startLoad). RED on the review tree:
// the heartbeat only touched pendingPos, so a stale card survived into a
// live room and the resume imposed its stale position + dropped pause.
func TestF1HeartbeatWhileLobbyAutoJoins(t *testing.T) {
	e, pl, _, ch := lobbyEngine(false)
	e.OnRoom(idleHello())
	lob := drainLobbyUi(t, ch).Lobby // the published pointer (the Ui may hold it)

	e.OnRoom(room.Event{Type: "position", T: 300, P: true, File: "a.mkv"})
	if e.lobby != nil {
		t.Fatalf("the lobby survived a live room's heartbeat")
	}
	if e.loaded != "a.mkv" || pl.startLoadCount() == 0 {
		t.Fatalf("the auto-join never loaded the room's file (loaded=%q)", e.loaded)
	}
	if e.pendingPos == nil || *e.pendingPos != 300 {
		t.Fatalf("the heartbeat position never parked: %+v", e.pendingPos)
	}
	waitTells(t, e)
	pauses := pl.pausesSnapshot()
	if len(pauses) == 0 || !pauses[len(pauses)-1] {
		t.Fatalf("the auto-join did not adopt the room's pause: %v", pauses)
	}
	// immutable-pointer law (#86c): the published Lobby struct is cleared
	// by swap, never mutated — the card the Ui may still hold keeps the
	// hello's snapshot.
	if lob == nil || lob.Now != "a.mkv" || lob.Pos == nil || *lob.Pos != 120 || !lob.Paused {
		t.Fatalf("the published lobby was mutated: %+v", lob)
	}
}

// PIN L1b-F1b: the heartbeat's file stamp (faz-3: the server drops
// positions whose file is not the room's) wins over the lobby's stale
// guess — the room moved on, we join what actually plays.
func TestF1HeartbeatRoomFileWins(t *testing.T) {
	e, _, _, _ := lobbyEngine(false)
	e.OnRoom(idleHello())
	e.OnRoom(room.Event{Type: "position", T: 30, P: false, File: "b.mkv"})
	if e.loaded != "b.mkv" {
		t.Fatalf("the auto-join loaded %q, want the room's b.mkv", e.loaded)
	}
	if e.pendingPos == nil || *e.pendingPos != 30 {
		t.Fatalf("the heartbeat position never parked: %+v", e.pendingPos)
	}
}

// PIN L1b-F1c (no self-trigger): a master with nothing loaded posts no
// positions — an idle master's lobby can never auto-join itself.
func TestF1IdleMasterPostsNoPositions(t *testing.T) {
	e, _, po, _ := lobbyEngine(true)
	e.OnRoom(room.Event{Type: "hello", Now: "", Roster: []string{"me"}})
	e.OnMpvp(mpv.State{Idle: true, Pause: true})
	e.OnMpvp(mpv.State{Idle: true, Pause: true})
	if evs := po.all(t); len(evs) != 0 {
		t.Fatalf("an idle master posted %v", evType(t, evs))
	}
}

// PIN L1b-G1: a seek the file cannot act on yet — still loading, no
// duration — PARKS in pendingPos and rides the existing flush when the
// duration arrives, instead of firing a tell mpv can lose at file head
// (the co-resident lobby peer's handoff seek, #86c).
func TestG1SeekParksWhileFileLoads(t *testing.T) {
	e, pl, _, _ := lobbyEngine(false)
	e.OnRoom(room.Event{Type: "load", Name: "a.mkv"}) // handoff load: claimed, not live
	e.OnRoom(room.Event{Type: "seek", T: 500})
	if e.pendingPos == nil || *e.pendingPos != 500 {
		t.Fatalf("the not-yet-actable seek never parked: %+v", e.pendingPos)
	}
	waitTells(t, e)
	if n := len(pl.seeksSnapshot()); n != 0 {
		t.Fatalf("the parked seek still fired a tell: %v", pl.seeksSnapshot())
	}
	e.OnMpvp(mpv.State{Pos: ptr(0), Pause: false, Dur: 900}) // the flush
	waitTells(t, e)
	seeked := pl.seeksSnapshot()
	if len(seeked) == 0 || seeked[len(seeked)-1] != 500 {
		t.Fatalf("the flush never seeked to the parked position: %v", seeked)
	}
}

// PIN L1b-G1b (the F8 seam): a seek on a LIVE file (duration known) acts
// immediately, unchanged — parking is only for the load window.
func TestG1SeekActsWhenLive(t *testing.T) {
	e, pl, _, _ := lobbyEngine(false)
	e.OnRoom(room.Event{Type: "load", Name: "a.mkv"})
	e.OnMpvp(mpv.State{Pos: ptr(1), Pause: false, Dur: 900}) // the file went live
	e.OnRoom(room.Event{Type: "seek", T: 500})
	waitTells(t, e)
	seeked := pl.seeksSnapshot()
	if len(seeked) == 0 || seeked[len(seeked)-1] != 500 {
		t.Fatalf("a live-file seek did not act: %v", seeked)
	}
	if e.pendingPos != nil {
		t.Fatalf("a live-file seek parked: %+v", e.pendingPos)
	}
}

// PIN L1b-G3: the resume's seek post is IMMEDIATE in the wire order —
// not gated on our own WaitLive. The G1 park protects a co-resident peer
// whose file is still loading; a wedged load must not delay the room's
// seek (the old WaitLive deferral is what this pin kills).
func TestG3ResumeSeekPostedWithoutWaitingLive(t *testing.T) {
	e, pl, po, ch := lobbyEngine(false)
	e.OnRoom(idleHello())
	drainLobbyUi(t, ch)
	pl.wedgeWait = make(chan struct{}) // our load never goes live
	if !e.ResumeLobby() {
		t.Fatalf("ResumeLobby refused an existing lobby")
	}
	evs := po.all(t)
	types := evType(t, evs)
	if len(types) != 3 || types[0] != "load" || types[1] != "pause" || types[2] != "seek" || evs[2].T != 120 {
		t.Fatalf("the resume seek was not posted immediately in order: %v (%+v)", types, evs)
	}
}

// PIN L1b-G1c (the pendingPause-clobber watch-point, worker2's L1-9):
// a parked seek must carry the ROOM's pause truth — the peer actuated
// the room's pause from the pause event, and the flush must not wipe it
// with a zero-value false (the peer would play in a paused room).
func TestG1SeekParkCarriesPauseTruth(t *testing.T) {
	e, pl, _, _ := lobbyEngine(false)
	e.OnRoom(room.Event{Type: "load", Name: "a.mkv"})       // handoff load: claimed, not live
	e.OnRoom(room.Event{Type: "pause"})                     // the room's pause lands first
	e.OnRoom(room.Event{Type: "seek", T: 500})              // parks (dur == 0)
	e.OnMpvp(mpv.State{Pos: ptr(0), Pause: true, Dur: 900}) // the flush
	waitTells(t, e)
	if e.snap == nil || !e.snap.paused {
		t.Fatalf("the flush wiped the room's pause: snap=%+v pauses=%v", e.snap, pl.pausesSnapshot())
	}
	seeked := pl.seeksSnapshot()
	if len(seeked) == 0 || seeked[len(seeked)-1] != 500 {
		t.Fatalf("the flush never seeked to the parked position: %v", seeked)
	}
}

// PIN L1b-G1d (the SYMMETRIC leg, master's #86c addendum): the park
// window TRACKS the room's live pause truth — a play/pause event after
// the park flips the pending flag, so the flush can neither resurrect a
// stale pause (room resumed before the flush → the peer would re-pause
// at the seek) nor drop a fresh one. Deterministic unit drive: the pty
// timing for this leg would be flaky.
func TestG1SeekParkTracksPauseFlips(t *testing.T) {
	// park → play → flush: the flush anchors PLAYING
	e, pl, _, _ := lobbyEngine(false)
	e.OnRoom(room.Event{Type: "load", Name: "a.mkv"})
	e.OnRoom(room.Event{Type: "seek", T: 500}) // parks (playing room)
	e.OnRoom(room.Event{Type: "play"})         // the room resumed while parked
	e.OnMpvp(mpv.State{Pos: ptr(0), Pause: false, Dur: 900})
	waitTells(t, e)
	if e.snap == nil || e.snap.paused {
		t.Fatalf("the flush resurrected a stale pause: snap=%+v pauses=%v", e.snap, pl.pausesSnapshot())
	}
	if n := len(pl.seeksSnapshot()); n == 0 || pl.seeksSnapshot()[n-1] != 500 {
		t.Fatalf("the flush never seeked: %v", pl.seeksSnapshot())
	}

	// park → pause → flush: the flush anchors PAUSED
	e2, pl2, _, _ := lobbyEngine(false)
	e2.OnRoom(room.Event{Type: "load", Name: "a.mkv"})
	e2.OnRoom(room.Event{Type: "seek", T: 500}) // parks (playing room)
	e2.OnRoom(room.Event{Type: "pause"})        // the room paused while parked
	e2.OnMpvp(mpv.State{Pos: ptr(0), Pause: true, Dur: 900})
	waitTells(t, e2)
	if e2.snap == nil || !e2.snap.paused {
		t.Fatalf("the flush dropped a fresh pause: snap=%+v pauses=%v", e2.snap, pl2.pausesSnapshot())
	}
}

// PIN L1c-F3 (master-2 M27, F10 C6 through the new path): the auto-join
// must refresh the master's toggle baseline — the hello path sets
// e.lastPause = ev.P (C6: without it the first toggle posts the WRONG
// direction) and masterTick would post a redundant pause naming us.
func TestF3AutoJoinRefreshesToggleBaseline(t *testing.T) {
	e, _, _, _ := lobbyEngine(true) // a MASTER that never posts a position keeps e.Master
	// the hello carries the room's LAST-WATCHED file and says PLAYING:
	// the card forms (idle room), the baseline starts false
	e.OnRoom(room.Event{Type: "hello", Now: "a.mkv", Roster: []string{"me"}, P: false})
	if e.lobby == nil {
		t.Fatalf("harness: the idle hello never formed the lobby")
	}
	if e.lastPause {
		t.Fatalf("harness: the hello carried a pause")
	}
	// the room turns live and is PAUSED: the auto-join fires
	e.OnRoom(room.Event{Type: "position", T: 90, P: true, File: "a.mkv"})
	if !e.lastPause {
		t.Fatalf("the auto-join left the toggle baseline stale (lastPause=false, room paused)")
	}
	if e.lobby != nil || e.loaded != "a.mkv" {
		t.Fatalf("the auto-join itself regressed: lobby=%+v loaded=%q", e.lobby, e.loaded)
	}
}

// PIN L1c-G5 (master-2 M23/M23b): the parked seek's "stale pause
// resurrection" leg — park while the room is PAUSED (pendingPause
// stamped true), the room RESUMES (play flips the pending flag), and the
// flush must anchor PLAYING, not resurrect the pause. Deterministic unit
// drive; this is the leg TestG1SeekParkTracksPauseFlips's playing-room
// park cannot cover (its play flip is a no-op on an already-false flag).
func TestG5ParkedSeekPlaySideAfterPause(t *testing.T) {
	e, pl, _, _ := lobbyEngine(false)
	e.OnRoom(room.Event{Type: "load", Name: "a.mkv"})
	e.OnRoom(room.Event{Type: "pause"})                      // pendingPause stamped true by the park? — no: stamped by THIS pause event's tracker
	e.OnRoom(room.Event{Type: "seek", T: 500})               // parks (carries lastPause=true)
	e.OnRoom(room.Event{Type: "play"})                       // the room resumed while parked
	e.OnMpvp(mpv.State{Pos: ptr(0), Pause: false, Dur: 900}) // the flush
	waitTells(t, e)
	if e.snap == nil || e.snap.paused {
		t.Fatalf("the flush resurrected the stale pause: snap=%+v pauses=%v", e.snap, pl.pausesSnapshot())
	}
	if n := len(pl.seeksSnapshot()); n == 0 || pl.seeksSnapshot()[n-1] != 500 {
		t.Fatalf("the flush never seeked: %v", pl.seeksSnapshot())
	}
}
