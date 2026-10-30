package ui

import (
	"strings"
	"testing"

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
