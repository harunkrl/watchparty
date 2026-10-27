// watchparty go client — entry point.
// M4: reconnect backoff, drift status, picker. 4-channel wiring (E1).
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"watchparty/client-go/internal/mpv"
	"watchparty/client-go/internal/room"
	"watchparty/client-go/internal/sync"
	"watchparty/client-go/internal/ui"
)

const usage = "usage: watchparty [--name nick] [--master] [--headless] <server-url>   e.g. watchparty --name ada http://100.101.102.103:8090"

// backoff is the plan's frozen curve: min(30, 3 * 2^min(attempt,4)).
func backoff(attempt int) int {
	d := 3
	for i := 0; i < attempt && i < 4; i++ {
		d *= 2
	}
	if d > 30 {
		d = 30
	}
	return d
}

func main() {
	name := flag.String("name", defaultNick(), "nickname shown to the room")
	master := flag.Bool("master", false, "run on the host side; this player is the clock authority")
	headless := flag.Bool("headless", false, "stdin/stdout mode (test harness)")
	flag.Parse()
	args := flag.Args()
	var base string
	if len(args) == 0 && flag.NFlag() == 0 && !*headless {
		// friend flow (plan section 0): unzip, double-click, answer, watch
		base = askURL()
		if nick := askLine(fmt.Sprintf("your name [%s]: ", *name)); nick != "" {
			*name = nick
		}
	} else {
		if len(args) != 1 { // url-first ordering would silently drop later flags
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		base = args[0]
		if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
	}

	// id format is client.py parity: name-1234
	api := room.New(base, fmt.Sprintf("%s-%d", *name, 1000+rand.Intn(9000)))
	player, err := mpv.Start(base, strings.Fields(os.Getenv("WATCHPARTY_MPV")))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	chRoom := make(chan room.Event, 16) // SSE -> dispatcher/engine + ui
	chMpv := make(chan mpv.State, 8)    // mpv poller -> sync engine
	chUi := make(chan sync.UiState, 8)  // sync -> ui status bar
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	engine := &sync.Engine{
		Master: *master,
		Player: player,
		Post:   func(ev room.Event) error { return api.Post(ev) },
		Clock:  sync.RealClock{},
		Ui:     chUi,
	}
	var mpvExited atomic.Bool
	var p *tea.Program
	if *headless {
		engine.Logf = func(f string, a ...any) { fmt.Printf(f+"\n", a...) }
	} else {
		// engine logs must go through the program: writing to stderr while
		// the bubbletea renderer owns the screen corrupts the frame
		engine.Logf = func(f string, a ...any) { p.Send(ui.EngineLogMsg{Line: fmt.Sprintf(f, a...)}) }
		p = tea.NewProgram(ui.New(*name, api, player, *master, engine)) // set before goroutines: they Send on it
	}

	go player.Poll(ctx, sync.PollSecs*time.Second, chMpv)
	go func() { // pushed mpv events -> instant discrete posts (master only)
		for ev := range player.Events() {
			engine.OnMpvEvent(ev.Name)
		}
	}()
	go func() {
		for {
			var st mpv.State
			select {
			case <-ctx.Done():
				return
			case st = <-chMpv:
			}
			if st.Exited {
				mpvExited.Store(true)
				if *headless {
					fmt.Fprintln(os.Stderr, "> mpv exited (window closed?) - restart the client")
					stop()
					player.Close()
					os.Exit(1)
				}
				p.Send(ui.MpvExitedMsg{})
				return
			}
			engine.OnMpvp(st)
		}
	}()

	go func() { // SSE with reconnect backoff (client.py sse_loop parity)
		attempt := 0
		for {
			err := api.Stream(ctx, *name, chRoom, func() { attempt = 0 })
			if ctx.Err() != nil {
				break
			}
			delay := backoff(attempt)
			attempt++
			note := fmt.Sprintf("> [conn] lost (%v), retrying in %ds", err, delay)
			if *headless {
				fmt.Println(note)
			} else {
				p.Send(ui.ReconnectingMsg{Note: note, Try: attempt, Delay: delay})
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(delay) * time.Second):
			}
		}
		close(chRoom)
	}()

	if *headless {
		err := ui.Run(*name, api, player, engine, chRoom, os.Stdin, os.Stdout)
		stop()
		player.Close()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if mpvExited.Load() {
			os.Exit(1)
		}
		return
	}

	go func() { // chUi -> status bar (drift)
		for {
			select {
			case <-ctx.Done():
				return
			case st := <-chUi:
				p.Send(st)
			}
		}
	}()
	go func() { // dispatcher: one chRoom consumer fans out to ui + engine
		for ev := range chRoom {
			p.Send(ev) // ui first: stays live while OnRoom blocks on loadfile (tea.Program.Send is goroutine-safe)
			engine.OnRoom(ev)
		}
		p.Send(ui.StreamClosedMsg{})
	}()

	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		stop()
		player.Close()
		os.Exit(1)
	}
	stop()         // risk H: cancel ctx first, ...
	player.Close() // ...then reap mpv (TERM -> 2s -> Kill, risk C)
	if mpvExited.Load() {
		os.Exit(1) // python parity: mpv-exited clients exit nonzero
	}
}

func defaultNick() string {
	if n := os.Getenv("USER"); n != "" {
		return n
	}
	return os.Getenv("USERNAME")
}

// askLine prints a prompt and reads one stdin line (friend onboarding).
func askLine(prompt string) string {
	fmt.Print(prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(os.Stderr, "\nno input — exiting")
		os.Exit(2)
	}
	return strings.TrimSpace(line)
}

// askURL asks until the answer looks like an http(s) URL (3 tries).
func askURL() string {
	for i := 0; i < 3; i++ {
		s := askLine("server URL (e.g. http://100.101.102.103:8090): ")
		if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
			return s
		}
		fmt.Println("must start with http:// — ask the host for the address")
	}
	os.Exit(2)
	return ""
}
