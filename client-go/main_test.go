package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
