// Headless adapter: the same engine wiring as the TUI with plain
// stdin/stdout I/O — pause_sync.mjs drives this (client.py parity).
package ui

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"watchparty/client-go/internal/cmd"
	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
	"watchparty/client-go/internal/sync"
)

// Run reads commands from in until EOF (Ctrl+D) or "quit", printing room
// events to out while feeding the sync engine (plan H: headless exits on
// stdin EOF).
func Run(nick string, api *room.Client, player *mpv.Player, engine *sync.Engine, roomCh <-chan room.Event, in io.Reader, out io.Writer) error {
	fmt.Fprintln(out, cmd.Usage)
	go func() { // room events -> log + engine (TUI dispatcher parity)
		for ev := range roomCh {
			if line := RenderEvent(ev); line != "" {
				fmt.Fprintln(out, line)
			}
			engine.OnRoom(ev)
		}
	}()
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if !cmd.IsCommand(line) {
			// bare text is chat: the headless adapter is verb-first, so
			// anything that is not a known verb goes to the room as chat
			if utf8.RuneCountInString(line) > cmd.ChatMaxLen {
				fmt.Fprintf(out, "> chat too long (max %d chars)\n", cmd.ChatMaxLen)
				continue
			}
			fmt.Fprintf(out, "[%s] %s\n", nick, line) // local echo
			report(out, api.Post(room.Event{Type: "chat", Nick: nick, Text: line}))
			continue
		}
		c, err := cmd.Parse(line)
		if err != nil {
			fmt.Fprintln(out, ">", err)
			continue
		}
		switch c.Kind {
		case cmd.Quit:
			return nil
		case cmd.Help:
			fmt.Fprintln(out, ">", cmd.Usage)
		case cmd.Chat:
			fmt.Fprintf(out, "[%s] %s\n", nick, c.Arg) // local echo
			report(out, api.Post(room.Event{Type: "chat", Nick: nick, Text: c.Arg}))
		case cmd.List, cmd.Pick: // headless has no cursor UI; pick falls back to list
			files, err := api.Files()
			if err != nil {
				fmt.Fprintln(out, "> err:", err)
				continue
			}
			for _, f := range files {
				fmt.Fprintln(out, "  "+f)
			}
		case cmd.Play:
			fmt.Fprintf(out, "> playing %s\n", c.Arg) // client.py parity
			if err := player.Load(c.Arg); err != nil {
				fmt.Fprintln(out, "> err:", err)
				continue
			}
			engine.OwnLoad(c.Arg) // our own load never echoes back; the engine must know (F3)
			report(out, api.Post(room.Event{Type: "load", Name: c.Arg}))
		case cmd.Queue:
			report(out, api.Post(room.Event{Type: "enqueue", Name: c.Arg}))
		case cmd.Skip:
			report(out, api.Post(room.Event{Type: "skip"}))
		}
	}
	return sc.Err() // nil on EOF: clean exit like client.py's for-loop end
}

func report(out io.Writer, err error) {
	if err != nil {
		fmt.Fprintln(out, "> err:", err)
	}
}
