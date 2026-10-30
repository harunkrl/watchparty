package sync

// F3: the client's half of the faz-3 gate (#49a).
//
// (1) The master's position heartbeats carry the file they describe: the
// server drops a position whose file is not the room's, so a master that
// still plays the old file cannot overwrite the new one's position.
// (2) The room's "moved on" notice is INFORMATION, not an order: the
// client renders it and lets the human decide — no auto-reload.

import (
	"testing"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
)

// The heartbeat names the file it describes.
func TestMasterHeartbeatCarriesTheFile(t *testing.T) {
	e, _, po := newEngine(true)
	e.loaded = "movie.mkv"
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false})
	if len(po.all(t)) != 1 {
		t.Fatalf("events = %v, want one heartbeat", evType(t, po.all(t)))
	}
	ev := po.all(t)[0]
	if ev.Type != "position" || ev.File != "movie.mkv" {
		t.Fatalf("the heartbeat did not carry the file: %+v", ev)
	}
}

// The parked-at-EOF heartbeat carries it too: that post is the one that
// holds the room's clock claim, so it must be the one that names the file.
func TestParkedHeartbeatCarriesTheFile(t *testing.T) {
	e, _, po := newEngine(true)
	e.loaded = "movie.mkv"
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false}) // the last position
	e.OnMpvp(mpv.State{Idle: true})                 // the film ended
	var parked *room.Event
	for i := range po.all(t) {
		if po.all(t)[i].Type == "position" {
			parked = &po.all(t)[i]
		}
	}
	if parked == nil {
		t.Fatalf("the parked master did not heartbeat: %v", evType(t, po.all(t)))
	}
	if parked.File != "movie.mkv" {
		t.Fatalf("the parked heartbeat did not carry the file: %+v", *parked)
	}
}

// A follower posts no positions at all: the field belongs to the master.
func TestFollowerPostsNoPosition(t *testing.T) {
	e, _, po := newEngine(false)
	e.loaded = "movie.mkv"
	e.OnMpvp(mpv.State{Pos: ptr(10), Pause: false})
	for _, ev := range po.all(t) {
		if ev.Type == "position" {
			t.Fatalf("a follower posted a position: %+v", ev)
		}
	}
}

// The notice is rendered, and the player is NOT reloaded: the humans
// decide when the room moves on.
func TestRoomMovedOnNoticeDoesNotReload(t *testing.T) {
	e, pl, _ := newEngine(true)
	e.loaded = "movie.mkv"
	e.OnRoom(room.Event{Type: "notice", Text: "the room moved on"})
	if len(pl.loads) != 0 {
		t.Fatalf("the notice reloaded the player: %v", pl.loads)
	}
	if e.loaded != "movie.mkv" {
		t.Fatalf("the notice changed our file: %q", e.loaded)
	}
	if len(pl.showText) != 1 || pl.showText[0] != "the room moved on" {
		t.Fatalf("the notice was not shown: %v", pl.showText)
	}
}
