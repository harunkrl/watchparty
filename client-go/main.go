// watchparty go client — entry point.
// M4: reconnect backoff, drift status, picker. 4-channel wiring (E1).
package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"

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
	setConsoleUTF8() // the console must speak UTF-8 (Windows): box drawing, Turkish nicks
	name := flag.String("name", defaultNick(), "nickname shown to the room")
	master := flag.Bool("master", false, "run on the host side; this player is the clock authority")
	headless := flag.Bool("headless", false, "stdin/stdout mode (test harness)")
	ver := flag.Bool("version", false, "print build version (git commit) and exit")
	flag.Parse()
	if *ver {
		fmt.Println("watchparty", buildVersion())
		return
	}
	args := flag.Args()
	var base string
	// the friend flow runs in a console window that closes with the
	// process: its errors must be read, so they wait for a keypress.
	friendFlow := len(args) == 0 && flag.NFlag() == 0 && !*headless
	if friendFlow {
		// friend flow (plan section 0): unzip, double-click, answer, watch
		last := loadLastServer()
		base = askURL(last.URL)
		def := *name
		if last.Nick != "" {
			def = last.Nick
		}
		if nick := askLine(fmt.Sprintf("your name [%s]: ", def)); nick != "" {
			*name = nick
		}
		// the address is remembered only after it worked (see the hello)
	} else {
		if len(args) != 1 { // url-first ordering would silently drop later flags
			fmt.Fprintln(os.Stderr, usage)
			if usageExitHold(len(args)) {
				waitForEnter() // the console closes with the process: let the user read it
			}
			os.Exit(2)
		}
		base = args[0]
		if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
	}

	// id format is client.py parity: name-1234
	id := fmt.Sprintf("%s-%d", *name, 1000+rand.Intn(9000))
	api := room.New(base, id)
	player, err := mpv.Start(base, strings.Fields(os.Getenv("WATCHPARTY_MPV")))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if friendFlow {
			waitForEnter()
		}
		os.Exit(1)
	}

	chRoom := make(chan room.Event, 16) // SSE -> dispatcher/engine + ui
	chMpv := make(chan mpv.State, 8)    // mpv poller -> sync engine
	chUi := make(chan sync.UiState, 8)  // sync -> ui status bar
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The signal path is SEPARATE from the context. With
	// signal.NotifyContext the clean shutdown's cancel() looked like a
	// signal: the goroutine below woke up and its os.Exit(1) usually won
	// the race -> clean runs exited 1 and the friend-flow notice was
	// skipped (A5 W1). Now only a REAL signal reaches the goroutine, and
	// the exit gate keeps a late signal from stealing the clean exit.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigc)
	gate := &exitGate{}
	go func() { // SIGTERM while headless blocks in the stdin scanner:
		// without this the client dies and leaves its mpv orphaned (153 of
		// them piled up on the test box in one day)
		first := <-sigc
		if swallowSignal(gate.cleanOwnsExit(), first) {
			// The clean shutdown owns the exit: the friend-flow notice
			// must be printed and read. The first ctrl+c is swallowed, the
			// next signal wins.
			<-sigc
		}
		player.Close()
		os.Exit(1)
	}()

	engine := &sync.Engine{
		Master: *master,
		ID:     id,
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
		p = tea.NewProgram(ui.New(*name, api, player, *master, engine), // set before goroutines: they Send on it
			tea.WithAltScreen(),       // the dashboard owns the full viewport: the header can never scroll off
			tea.WithMouseCellMotion()) // wheel scrolls chat; clicks on the bar seek
	}
	// the player's diagnostics go the same way: stderr would corrupt the frame
	player.SetLogf(engine.Logf)

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
					fmt.Fprintln(os.Stderr, "> mpv exited or stopped talking - restart the client")
					gate.beginClean() // this path owns the exit from here
					cancel()
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
		gate.beginClean()
		cancel()
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
		saved := false
		for ev := range chRoom {
			p.Send(ev) // ui first: stays live while OnRoom blocks on loadfile (tea.Program.Send is goroutine-safe)
			engine.OnRoom(ev)
			if ev.Type == "hello" && !saved {
				// the server answered: the address works, remember it
				saved = true
				if err := saveLastServer(base, *name); err != nil {
					p.Send(ui.EngineLogMsg{Line: "> could not remember the server: " + err.Error()})
				}
			}
		}
		p.Send(ui.StreamClosedMsg{})
	}()

	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		gate.beginClean() // this path owns the exit from here
		cancel()
		player.Close()
		if friendFlow {
			waitForEnter()
		}
		os.Exit(1)
	}
	gate.beginClean() // from here on the clean path owns the exit
	cancel()          // risk H: cancel ctx first, ...
	player.Close()    // ...then reap mpv (TERM -> 2s -> Kill, risk C)
	if note := mpvExitNotice(friendFlow, mpvExited.Load()); note != "" {
		// the console closes with the process: the reason must be read
		fmt.Fprintln(os.Stderr, note)
		waitForEnter()
	}
	if mpvExited.Load() {
		os.Exit(1) // python parity: mpv-exited clients exit nonzero
	}
}

// mpvExitNotice is the reason the client is closing, in the friend flow.
// The console window closes with the process: a message that is not
// printed is lost, and "mpv exited" is the one the friend must read.
func mpvExitNotice(friendFlow, mpvExited bool) string {
	if !friendFlow || !mpvExited {
		return ""
	}
	return "mpv exited or stopped talking - restart the client"
}

// usageExitHold: a double-clicked console closes with the process, so
// the usage hint waits for a keypress there; a terminal user reads it
// as it scrolls by.
func usageExitHold(argc int) bool { return argc == 0 }

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
		waitForEnter()
		os.Exit(2)
	}
	return strings.TrimSpace(line)
}

// urlPrompt is the friend-flow prompt: the bare address form. The
// scheme is added by the client, so the prompt must not teach it —
// and the remembered address is shown the way a friend types it.
func urlPrompt(def string) string {
	if def == "" {
		return "server address (e.g. 192.168.1.181:8090): "
	}
	return fmt.Sprintf("server URL [%s]: ", bareAddr(def))
}

// bareAddr strips the scheme from an address.
func bareAddr(s string) string {
	return strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
}

// waitForEnter keeps the console window open until the user has read
// the error: the friend flow's console closes with the process, so an
// error would vanish unread. A closed stdin returns at once.
func waitForEnter() {
	fmt.Fprintln(os.Stderr, "press Enter to close")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

// askURL asks until the answer is a usable server address (3 tries).
// def is the remembered server: Enter takes it.
func askURL(def string) string {
	for i := 0; i < 3; i++ {
		s := askLine(urlPrompt(def))
		if s == "" && def != "" {
			return def
		}
		if u := normalizeServerURL(s); u != "" {
			return u
		}
		fmt.Println("that is not a server address — ask the host for it")
	}
	waitForEnter() // the console closes with the process: let the user read it
	os.Exit(2)
	return ""
}

// normalizeServerURL turns what a friend types into a server URL:
// "host" becomes "http://host:8090", "host:port" becomes
// "http://host:port", a full http(s) URL is kept as it is. "" when the
// input cannot be an address.
func normalizeServerURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "://"); i >= 0 {
		scheme := strings.ToLower(s[:i])
		if i == 0 || (scheme != "http" && scheme != "https") {
			return "" // not an address we can talk to
		}
		rest := strings.TrimRight(s[i+3:], "/")
		if rest == "" {
			return "" // "http://" alone is not an address
		}
		return scheme + "://" + rest
	}
	if strings.ContainsAny(s, " \t") {
		return "" // a host has no spaces
	}
	s = strings.TrimRight(s, "/") // "1.2.3.4:8090/" is still an address
	host, port := s, ""
	if h, p, err := net.SplitHostPort(s); err == nil {
		host, port = h, p
	}
	if port == "" {
		port = defaultServerPort
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]" // a bare IPv6 address
	}
	return "http://" + host + ":" + port
}

// defaultServerPort is the port the room server listens on.
const defaultServerPort = "8090"

// lastServerPath is where the remembered server and nick live (next to
// the executable). A var so the tests can point it elsewhere.
var lastServerPath = defaultLastServerPath

func defaultLastServerPath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), "last-server.txt")
}

// lastServer is the remembered server URL and nick.
type lastServer struct{ URL, Nick string }

// exitGate decides who owns the process exit. The clean shutdown marks
// itself first: after that a late signal must not steal the exit — the
// friend-flow notice still has to be printed and the exit code must stay
// 0 (A5 W1). Before the gate the signal goroutine's os.Exit(1) usually
// won the race with the clean path.
type exitGate struct{ clean atomic.Bool }

// beginClean: the clean shutdown has started — it owns the exit now.
func (g *exitGate) beginClean() { g.clean.Store(true) }

// cleanOwnsExit: the clean shutdown owns the exit (a late signal must
// not steal it).
func (g *exitGate) cleanOwnsExit() bool { return g.clean.Load() }

// swallowSignal: the clean shutdown owns the exit, so a ctrl+c there is
// swallowed once — the friend may still be reading the notice. The NEXT
// signal wins. A SIGTERM (the console window closed) always wins: there
// is no console left to read anything.
func swallowSignal(cleanOwnsExit bool, first os.Signal) bool {
	return cleanOwnsExit && first == os.Interrupt
}

// loadLastServer reads the remembered server file (empty when there is
// none or it cannot be read).
func loadLastServer() lastServer {
	path := lastServerPath()
	if path == "" {
		return lastServer{}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return lastServer{}
	}
	lines := strings.Split(strings.ReplaceAll(decodeRemembered(b), "\r\n", "\n"), "\n")
	ls := lastServer{}
	if len(lines) > 0 {
		// a UTF-8 BOM (older Notepad writes one) must not end up in the URL
		ls.URL = strings.TrimSpace(strings.TrimPrefix(lines[0], "\ufeff"))
	}
	if len(lines) > 1 {
		ls.Nick = strings.TrimSpace(strings.TrimPrefix(lines[1], "\ufeff"))
	}
	return ls
}

// decodeRemembered decodes the remembered file: the client writes UTF-8,
// but an older Notepad saves UTF-16 (the BOM says which endianness) —
// read as UTF-8 the URL would be garbage and the friend would start with
// a nonsense default address (A5 W11).
func decodeRemembered(b []byte) string {
	switch {
	case len(b) >= 2 && b[0] == 0xff && b[1] == 0xfe:
		return decodeUTF16(b[2:], binary.LittleEndian)
	case len(b) >= 2 && b[0] == 0xfe && b[1] == 0xff:
		return decodeUTF16(b[2:], binary.BigEndian)
	}
	return string(b)
}

// decodeUTF16 turns UTF-16 code units into a Go string.
func decodeUTF16(b []byte, order binary.ByteOrder) string {
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		units = append(units, order.Uint16(b[i:i+2]))
	}
	return string(utf16.Decode(units))
}

// saveLastServer remembers the server and the nick for the next start
// (best effort: a read-only folder must not break the run — the caller
// warns and continues). The write is atomic: a half-written file would
// leave the next start with a truncated (garbage) address (A5 W11).
func saveLastServer(url, nick string) error {
	path := lastServerPath()
	if path == "" {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(url+"\n"+nick+"\n"), 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// buildVersion reports the git commit this binary was built from
// (stamped by go build's VCS info) — the "am I on the new build?" check.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(unknown: no build info)"
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev == "" {
		return "(unknown: not built from a git checkout)"
	}
	return rev[:8] + dirty
}
