package ui

// Approved improvement round (plan OPEN item 0), client half: C1 (zen sync
// strip), C2 (duration cache), C3 (muted chip), C4 (reconnect delta),
// C5 (stale heartbeat hint).

import (
	"strings"
	"testing"
	"time"

	"watchparty/client-go/internal/room"
	"watchparty/client-go/internal/sync"
)

// C2: a duration we already measured must never be probed again — the
// reload paints the bar instantly.
func TestDurationCacheAvoidsReprobe(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m = upd(t, m, durationMsg{name: "movie.mkv", dur: 600})
	if m.durCache["movie.mkv"] != 600 {
		t.Fatalf("duration not cached: %v", m.durCache)
	}
	m2, cmd := m.setFile("movie.mkv")
	if m2.duration != 600 {
		t.Fatalf("cached duration lost: %v", m2.duration)
	}
	if cmd != nil {
		t.Fatal("a cached duration must not re-probe mpv")
	}
	// the room's load broadcast for a known file is instant too
	m3 := upd(t, m, room.Event{Type: "load", Name: "movie.mkv"})
	if m3.duration != 600 {
		t.Fatalf("reload did not restore the cached duration: %v", m3.duration)
	}
	// an unknown file still starts from zero
	m4, _ := m.setFile("other.mkv")
	if m4.duration != 0 {
		t.Fatalf("unknown file kept a duration: %v", m4.duration)
	}
}

// C1: movie-night mode gets the sync strip: file (left), time left and
// the state-colored drift chip (right).
func TestZenHeaderShowsSyncStrip(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m.zen = true
	m.now = "movie.mkv"
	m.duration = 600
	p := 65.0
	m = upd(t, m, sync.UiState{Pos: &p, Drift: 0.4})
	l1, l2 := m.headerLines(m.contentWidth())
	if l2 != "" {
		t.Fatalf("zen header must stay one row: %q", l2)
	}
	for _, want := range []string{"movie.mkv", "8:55 left", "+0.4s"} {
		if !strings.Contains(l1, want) {
			t.Fatalf("zen header missing %q: %q", want, l1)
		}
	}
}

// C3: mpv's mute flag reaches the stat row.
func TestStatRowShowsMuted(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	p := 10.0
	m = upd(t, m, sync.UiState{Pos: &p, Volume: 55, Muted: true})
	row := strings.Join(m.nowPlayingLines(60), "\n")
	if !strings.Contains(row, "muted") {
		t.Fatalf("stat row missing the mute hint: %q", row)
	}
}

// C5: a follower whose heartbeat went quiet says so.
func TestStatRowShowsStaleHeartbeat(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	p := 10.0
	m = upd(t, m, sync.UiState{Pos: &p, Stale: true})
	row := strings.Join(m.nowPlayingLines(60), "\n")
	if !strings.Contains(row, "heartbeat stale") {
		t.Fatalf("stat row missing the stale hint: %q", row)
	}
}

// C4: the system pane reports how long the outage lasted.
func TestReconnectNoteInSystemPane(t *testing.T) {
	m := New("t", nil, nil, false, nil)
	m = upd(t, m, ReconnectingMsg{Note: "> [conn] lost (x), retrying in 3s", Try: 1, Delay: 3})
	m.lostAt = time.Now().Add(-3 * time.Second) // deterministic outage length
	m = upd(t, m, room.Event{Type: "hello", Queue: []string{}})
	sys := strings.Join(m.renderSysLines(), "\n")
	if !strings.Contains(sys, "reconnected after 3s") {
		t.Fatalf("reconnect note missing from the system pane: %q", sys)
	}
	if !m.lostAt.IsZero() {
		t.Fatal("the outage stamp must be cleared after the reconnect")
	}
	// a first connection (no outage) must not claim a reconnect
	m2 := New("t", nil, nil, false, nil)
	m2 = upd(t, m2, room.Event{Type: "hello", Queue: []string{}})
	if strings.Contains(strings.Join(m2.renderSysLines(), "\n"), "reconnected") {
		t.Fatal("the first hello must not report a reconnect")
	}
}
