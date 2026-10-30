package ui

import (
	"strings"
	"testing"

	"watchparty/client-go/internal/room"
	"watchparty/client-go/internal/sync"
)

// m2-S2: a mechanical pause (a stall on the other side's link) must not
// be shown as "paused by <nick>" — nobody pressed anything.
func TestMechanicalPauseChipNamesNobody(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	pos := 10.0
	m = upd(t, m, sync.UiState{Pos: &pos, Paused: true, Mechanical: true})
	row := strings.Join(m.nowPlayingLines(100), "\n")
	if !strings.Contains(row, "⏸ paused (stall)") {
		t.Fatalf("the mechanical pause chip is missing: %q", row)
	}
	if strings.Contains(row, "paused by") {
		t.Fatalf("the mechanical pause named an actor: %q", row)
	}
}

// A human pause still names the actor.
func TestHumanPauseChipNamesTheActor(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	pos := 10.0
	m = upd(t, m, sync.UiState{Pos: &pos, Paused: true, PausedBy: "bo"})
	row := strings.Join(m.nowPlayingLines(100), "\n")
	if !strings.Contains(row, "⏸ paused by bo") {
		t.Fatalf("the actor chip is missing: %q", row)
	}
	if strings.Contains(row, "(stall)") {
		t.Fatalf("a human pause was labelled mechanical: %q", row)
	}
}

// A5 W4: the receiving side's LOG must not blame a human for a
// mechanical pause either — the chip already doesn't (m2-S2).
func TestMechanicalPauseLogNamesNobody(t *testing.T) {
	line := RenderEvent(room.Event{Type: "pause", Nick: "bo", Mechanical: true}, "ada")
	if strings.Contains(line, "bo") {
		t.Fatalf("the log blames a human for a stall pause: %q", line)
	}
	if !strings.Contains(line, "stall") {
		t.Fatalf("the log must mark the stall pause: %q", line)
	}
}

// A human pause still names the actor in the log.
func TestHumanPauseLogNamesTheActor(t *testing.T) {
	line := RenderEvent(room.Event{Type: "pause", Nick: "bo"}, "ada")
	if !strings.Contains(line, "by bo") {
		t.Fatalf("the log lost the actor: %q", line)
	}
}

// A5 W10: the room is paused (we detected a stall) even before our own
// player caught up: the chip must not claim "playing" for two seconds.
func TestMechanicalPauseChipIsImmediate(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	pos := 10.0
	m = upd(t, m, sync.UiState{Pos: &pos, Paused: false, Mechanical: true})
	row := strings.Join(m.nowPlayingLines(100), "\n")
	if strings.Contains(row, "▶ playing") {
		t.Fatalf("the chip claims playing while the room is paused: %q", row)
	}
	if !strings.Contains(row, "paused (stall)") {
		t.Fatalf("the stall pause is not shown: %q", row)
	}
}
