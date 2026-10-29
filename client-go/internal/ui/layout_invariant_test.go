package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"watchparty/client-go/internal/room"
	"watchparty/client-go/internal/sync"
)

func TestViewFitsTerminal(t *testing.T) {
	// the stacked layout (< 100 columns) is checked too: it has no boxes
	// to truncate a long title, so the header and the input row must
	// fit the frame themselves (A3-UI2/A3-UI3).
	for _, h := range []int{20, 24, 30, 36, 40, 44, 50} {
		for _, w := range []int{60, 80, 99, 100, 110, 118, 130, 160} {
			for _, tc := range []string{"connecting", "playing", "menu", "picker", "sysoff", "zen", "menu-playing"} {
				m := New("sen", nil, nil, true, nil)
				mm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
				m = mm.(Model)
				send := func(msg tea.Msg) {
					x, _ := m.Update(msg)
					m = x.(Model)
				}
				pos := 12.0
				switch tc {
				case "playing", "menu-playing":
					send(sync.UiState{Pos: &pos, Drift: 0.2, Cache: 12, Volume: 85})
					m.now, m.duration = "vid.mp4", 64
					send(room.Event{Type: "join", Nick: "o"})
					send(room.Event{Type: "chat", Nick: "o", Text: "selam"})
				}
				switch tc {
				case "menu", "menu-playing":
					m.input.SetValue("/")
					m = m.openMenu()
				case "picker":
					send(tea.KeyMsg{Type: tea.KeyCtrlP})
				case "sysoff":
					send(tea.KeyMsg{Type: tea.KeyCtrlG})
				case "zen":
					send(tea.KeyMsg{Type: tea.KeyCtrlZ})
				}
				m = m.stepEq()
				v := m.View()
				lines := strings.Split(v, "\n")
				if n := len(lines); n > h {
					t.Errorf("%s w=%d h=%d: View has %d lines (> terminal %d)\nfirst overflow near: %q",
						tc, w, h, n, h, clip(lines[h-1]))
				}
				for i, l := range lines {
					if lw := lipgloss.Width(l); lw > w {
						t.Errorf("%s w=%d h=%d: line %d is %d cols (> %d): %q", tc, w, h, i, lw, w, clip(l))
						break
					}
				}
			}
		}
	}
}

func clip(s string) string {
	if len(s) > 70 {
		return s[:70]
	}
	return s
}
