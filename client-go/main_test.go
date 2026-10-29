package main

import (
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
