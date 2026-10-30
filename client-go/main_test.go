package main

import (
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf16"
)

// G2: the reconnect steps get a ±20% jitter, and the jitter never
// leaves the plan's cap (the delay stays inside the frozen curve).
func TestBackoffJitterStaysInBounds(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for attempt := 0; attempt < 8; attempt++ {
		base := time.Duration(backoff(attempt)) * time.Second
		for i := 0; i < 200; i++ {
			d := backoffDelay(attempt, r)
			if d < base*4/5 || d > base*6/5 {
				t.Fatalf("attempt %d: delay %v is outside ±20%% of %v", attempt, d, base)
			}
			if d > maxBackoff {
				t.Fatalf("attempt %d: delay %v is past the cap", attempt, d)
			}
			if d < time.Second {
				t.Fatalf("attempt %d: delay %v is under a second", attempt, d)
			}
		}
	}
}

// The jitter must really spread the reconnects (a server that dies must
// not get every client back in the same second) and it must be
// reproducible from its seed (the tests need determinism).
func TestBackoffJitterIsSeededAndSpread(t *testing.T) {
	first := jittered(rand.New(rand.NewSource(1)))
	again := jittered(rand.New(rand.NewSource(1)))
	other := jittered(rand.New(rand.NewSource(2)))
	if !reflect.DeepEqual(first, again) {
		t.Fatalf("the same seed must produce the same delays: %v vs %v", first, again)
	}
	if reflect.DeepEqual(first, other) {
		t.Fatalf("different seeds must spread the delays: %v", first)
	}
}

func jittered(r *rand.Rand) []time.Duration {
	out := make([]time.Duration, 0, 20)
	for i := 0; i < 20; i++ {
		out = append(out, backoffDelay(2, r))
	}
	return out
}

// W3: a friend types the host, not a URL.
func TestNormalizeServerURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"host", "http://host:8090"},
		{"host:9000", "http://host:9000"},
		{"100.101.102.103", "http://100.101.102.103:8090"},
		{"  10.0.0.9  ", "http://10.0.0.9:8090"},
		{"http://100.1.2.3:8090", "http://100.1.2.3:8090"},
		{"https://example.com", "https://example.com"},
		{"http://example.com/", "http://example.com"},
		{"", ""},
		{"not a host", ""},
		// W11: a friend types a trailing slash or an upper-case scheme
		{"192.168.1.181:8090/", "http://192.168.1.181:8090"},
		{"HTTP://192.168.1.181:8090", "http://192.168.1.181:8090"},
		{"ftp://192.168.1.181", ""},
		{"http://", ""},
	}
	for _, c := range cases {
		if got := normalizeServerURL(c.in); got != c.want {
			t.Errorf("normalizeServerURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// W4: the server and the nick are remembered for the next start.
func TestLastServerIsRemembered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "last-server.txt")
	old := lastServerPath
	lastServerPath = func() string { return path }
	defer func() { lastServerPath = old }()

	if got := loadLastServer(); got.URL != "" || got.Nick != "" {
		t.Fatalf("nothing was remembered yet, got %+v", got)
	}
	saveLastServer("http://10.0.0.5:8090", "ada")
	got := loadLastServer()
	if got.URL != "http://10.0.0.5:8090" || got.Nick != "ada" {
		t.Fatalf("remembered %+v", got)
	}
	saveLastServer("http://10.0.0.6:8090", "bob")
	got = loadLastServer()
	if got.URL != "http://10.0.0.6:8090" || got.Nick != "bob" {
		t.Fatalf("remembered %+v after the update", got)
	}
}

// W10: a UTF-8 BOM (older Notepad writes one) must not end up in the
// remembered URL — the client would try a bogus address.
func TestLastServerStripsBOM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "last-server.txt")
	if err := os.WriteFile(path, []byte("\ufeffhttp://10.0.0.5:8090\r\nada\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := lastServerPath
	lastServerPath = func() string { return path }
	defer func() { lastServerPath = old }()

	got := loadLastServer()
	if got.URL != "http://10.0.0.5:8090" {
		t.Fatalf("BOM leaked into the remembered URL: %q", got.URL)
	}
	if got.Nick != "ada" {
		t.Fatalf("CRLF/BOM broke the remembered nick: %q", got.Nick)
	}
}

// W10: a read-only folder must not break the run: the save fails and
// the caller warns and continues.
func TestSaveLastServerReportsFailure(t *testing.T) {
	old := lastServerPath
	lastServerPath = func() string { return filepath.Join(t.TempDir(), "missing", "last-server.txt") }
	defer func() { lastServerPath = old }()

	if err := saveLastServer("http://10.0.0.5:8090", "ada"); err == nil {
		t.Fatal("an unwritable path must be reported")
	}
}

// W5: the prompt teaches the bare address form — the scheme never
// appears in it.
func TestURLPromptTeachesTheBareForm(t *testing.T) {
	p := urlPrompt("")
	if strings.Contains(p, "http://") {
		t.Fatalf("the prompt must not teach the scheme: %q", p)
	}
	if !strings.Contains(p, "192.168.1.181:8090") {
		t.Fatalf("the prompt must show the bare example: %q", p)
	}
	// the remembered address is shown the way a friend types it
	d := urlPrompt("http://192.168.1.181:8090")
	if strings.Contains(d, "http://") {
		t.Fatalf("the default must not show the scheme: %q", d)
	}
	if !strings.Contains(d, "192.168.1.181:8090") {
		t.Fatalf("the default must show the address: %q", d)
	}
}

// W4: in the friend flow the console closes with the process: the
// "mpv exited" reason must be printed (and waited for) instead of
// vanishing with the window.
func TestMpvExitNoticeIsHeldForTheFriend(t *testing.T) {
	if got := mpvExitNotice(true, true); got == "" {
		t.Fatal("the friend flow must get a readable notice")
	}
	if got := mpvExitNotice(false, true); got != "" {
		t.Fatalf("a terminal user must not get the notice: %q", got)
	}
	if got := mpvExitNotice(true, false); got != "" {
		t.Fatalf("a normal exit must not print the notice: %q", got)
	}
}

// W14: a double-clicked console closes with the process: the usage hint
// must stay readable there.
func TestUsageHintIsHeldWithoutArgs(t *testing.T) {
	if !usageExitHold(0) {
		t.Fatal("no args (double-click) must hold the usage hint")
	}
	if usageExitHold(1) {
		t.Fatal("a terminal user must not be held")
	}
}

// W2: the console code page fix must be safe to call everywhere (a
// build without a console has no code page at all).
func TestSetConsoleUTF8IsSafe(t *testing.T) {
	setConsoleUTF8()
}

// A5 W1: the clean shutdown owns the process exit. A late signal must
// not steal it — that race made clean runs exit 1 and skipped the
// friend-flow notice.
func TestExitGateKeepsTheCleanPath(t *testing.T) {
	g := &exitGate{}
	if g.cleanOwnsExit() {
		t.Fatal("before the clean shutdown the signal path owns the exit")
	}
	g.beginClean()
	if !g.cleanOwnsExit() {
		t.Fatal("the clean shutdown must own the exit once it started")
	}
}

// A5 W1 (master-2 P3-a): during the friend-flow wait a ctrl+c must not
// be swallowed forever — the next signal wins. A SIGTERM (the console
// window closed) always wins.
func TestSwallowSignal(t *testing.T) {
	if !swallowSignal(true, os.Interrupt) {
		t.Fatal("a ctrl+c during the clean shutdown must be swallowed once")
	}
	if swallowSignal(true, syscall.SIGTERM) {
		t.Fatal("a closed console (SIGTERM) must always win")
	}
	if swallowSignal(false, os.Interrupt) {
		t.Fatal("outside the clean shutdown a ctrl+c must win at once")
	}
}

// A5 W1: a clean quit must leave with 0. The signal goroutine used to
// win the race with the clean path and exit 1 (python parity: only an
// mpv exit is nonzero).
func TestCleanQuitExitsZero(t *testing.T) {
	if _, err := exec.LookPath("mpv"); err != nil {
		t.Skip("no mpv in PATH")
	}
	bin := buildClient(t)
	// headless + an immediate EOF on stdin: the clean path, no server needed
	cmd := exec.Command(bin, "--headless", "--name", "tester", "http://127.0.0.1:1")
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a clean quit must exit 0, got %v:\n%s", err, out)
	}
}

// buildClient compiles the client: the exit code is a property of the
// process, it cannot be observed from inside it.
func buildClient(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "watchparty")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// A5 W11: an older Notepad saves the remembered file as UTF-16: the BOM
// must be decoded, not read as garbage (the friend would start with a
// nonsense default address).
func TestLastServerReadsUTF16(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "last-server.txt")
	var b []byte
	b = append(b, 0xff, 0xfe) // UTF-16 LE, BOM
	for _, u := range utf16.Encode([]rune("http://10.0.0.5:8090\r\nada\r\n")) {
		b = append(b, byte(u), byte(u>>8))
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	old := lastServerPath
	lastServerPath = func() string { return path }
	defer func() { lastServerPath = old }()

	got := loadLastServer()
	if got.URL != "http://10.0.0.5:8090" {
		t.Fatalf("the UTF-16 file was not decoded: %q", got.URL)
	}
	if got.Nick != "ada" {
		t.Fatalf("the UTF-16 nick was not decoded: %q", got.Nick)
	}
}

// A5 W11: the write is atomic: the temp file must not survive and the
// target must hold the whole address (a half-written file would leave
// the next start with a truncated address).
func TestSaveLastServerLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "last-server.txt")
	old := lastServerPath
	lastServerPath = func() string { return path }
	defer func() { lastServerPath = old }()

	if err := saveLastServer("http://10.0.0.5:8090", "ada"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "last-server.txt" {
		t.Fatalf("the atomic write left something behind: %v", entries)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "http://10.0.0.5:8090\nada\n" {
		t.Fatalf("the remembered file is not complete: %q", string(b))
	}
}
