//go:build audit3

package ui

// Audit round 3 (worker2) — EVIDENCE. These tests FAIL against the current
// tree on purpose: each one documents a reported bug. They are behind the
// `audit3` build tag so the normal battery stays green.
//
// Run: go test -tags audit3 ./internal/ui/

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"watchparty/client-go/internal/room"
	"watchparty/client-go/internal/sync"
)

// widestContentLine returns the widest CONTENT lines of a rendered view
// (widest first) and the width of the widest one. The frame's border only
// mirrors the content width, so it never causes an overflow by itself.
func widestContentLine(view string) ([]string, int) {
	type row struct {
		w int
		l string
	}
	var rows []row
	for _, l := range strings.Split(view, "\n") {
		if isBorderOnly(l) {
			continue // the frame border only mirrors the content width
		}
		rows = append(rows, row{lipgloss.Width(l), l})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].w > rows[j].w })
	var out []string
	widest := 0
	for i, r := range rows {
		if i >= 3 {
			break
		}
		out = append(out, fmt.Sprintf("%d cols: %q", r.w, clip(r.l)))
		widest = r.w
	}
	return out, widest
}

// isBorderOnly reports whether a line is nothing but the frame's box
// drawing.
func isBorderOnly(l string) bool {
	t := strings.TrimRight(l, " ")
	for _, r := range t {
		if r != '─' && r != '│' && r != '╭' && r != '╮' && r != '╰' && r != '╯' && r != ' ' {
			return false
		}
	}
	return strings.ContainsAny(t, "╭╰│")
}

// A3-UI1: a backwards wall-clock step (NTP) during an outage makes the
// reconnect delta negative — the system pane prints "reconnected after
// -5s". The delta must never go below zero.
func TestAudit3NegativeReconnectDelta(t *testing.T) {
	if got := humanDur(-5 * time.Second); got != "0s" {
		t.Fatalf("humanDur(-5s) = %q, want 0s (a clock step must not print a negative delta)", got)
	}
}

// A3-UI2: a long file name overflows the stacked layout's header row.
// joinLR drops the right side when the parts collide but never truncates
// the left one, so the frame grows past the terminal — the header scrolls
// off, the very thing the viewport-fit invariant test guards against.
//
// The invariant test only exercises widths >= 100 (the dashboard); the
// stacked layout (< 100 columns) is never width-checked.
func TestAudit3LongFileNameFitsTerminal(t *testing.T) {
	for _, w := range []int{80, 90, 99} {
		m := New("ada", nil, nil, false, nil)
		m = upd(t, m, tea.WindowSizeMsg{Width: w, Height: 30})
		m.now = strings.Repeat("a-very-long-movie-name-", 5)
		m.duration = 600
		p := 10.0
		m = upd(t, m, sync.UiState{Pos: &p})
		rows, widest := widestContentLine(m.View())
		if widest > w {
			t.Fatalf("w=%d: the widest content line is %d cols (> %d):\n%s", w, widest, w, strings.Join(rows, "\n"))
		}
	}
}

// A3-UI3: a long nickname overflows the input row: the prompt is rendered
// as-is while the input width is clamped to a minimum of 10, so the row
// is prompt+10 columns wide no matter how narrow the terminal is.
func TestAudit3LongNickFitsTerminal(t *testing.T) {
	for _, w := range []int{60, 80, 99} {
		m := New(strings.Repeat("n", 90), nil, nil, false, nil)
		m = upd(t, m, tea.WindowSizeMsg{Width: w, Height: 30})
		rows, widest := widestContentLine(m.View())
		if widest > w {
			t.Fatalf("w=%d: the widest content line is %d cols (> %d):\n%s", w, widest, w, strings.Join(rows, "\n"))
		}
	}
}

// A3-UI5: the duration cache is keyed by the file NAME only and is never
// re-probed. If the file behind a name is replaced (a new cut of the same
// film), the bar keeps the old duration forever — and nothing ever asks
// mpv again. The instant render from the cache is fine; the silence is
// not: a background probe would correct itself.
func TestAudit3CachedDurationIsStillVerified(t *testing.T) {
	m := New("ada", nil, nil, false, nil)
	m = upd(t, m, durationMsg{name: "movie.mkv", dur: 600})
	_, cmd := m.setFile("movie.mkv")
	if cmd == nil {
		t.Fatal("a cached duration is never re-probed: a replaced file keeps the old length forever")
	}
}

// master (engine.OnRoom "notice"), but the UI keeps its own `master` flag.
// The TUI then keeps showing the master clock and hides the drift of a
// client that is actually following the other master — the panel lies
// about the room's state.
func TestAudit3DemotionUpdatesTheRole(t *testing.T) {
	e := &sync.Engine{ID: "ada-1", Master: true, Clock: sync.RealClock{}}
	m := New("ada", nil, nil, true, e)
	m = upd(t, m, room.Event{Type: "notice", From: "ada-1",
		Text: "two masters: bo owns the clock; ignoring ada-1"})
	if m.master {
		t.Fatal("the UI still claims the master role after the engine demoted itself")
	}
}
