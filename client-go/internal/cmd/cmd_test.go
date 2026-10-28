package cmd

import (
	"strings"
	"testing"
)

func TestIsCommand(t *testing.T) {
	// C8: only a verb is a command; everything else is chat.
	for _, c := range []string{"list", "pick", "play a.mkv", "queue", "skip", "chat hi", "quit", "help"} {
		if !IsCommand(c) {
			t.Errorf("IsCommand(%q) = false, want true", c)
		}
	}
	for _, c := range []string{"", "hello", "playlist", "hey there", "playlists", "skipping"} {
		if IsCommand(c) {
			t.Errorf("IsCommand(%q) = true, want false", c)
		}
	}
	// a verb with trailing words is still that verb (cmd.Parse parity:
	// no-argument verbs ignore the extra text)
}

func TestParse(t *testing.T) {
	cases := []struct {
		in    string
		kind  string
		arg   string
		isErr bool
	}{
		{"list", List, "", false},
		{"  list  ", List, "", false},
		{"list extra ignored", List, "", false}, // client.py parity: arg ignored
		{"pick", Pick, "", false},
		{"skip", Skip, "", false},
		{"quit", Quit, "", false},
		{"help", Help, "", false},
		{"play movie.mkv", Play, "movie.mkv", false},
		{"play", "", "", true},
		{"queue  a b.mkv ", Queue, "a b.mkv", false},
		{"queue", Queue, "", false}, // bare queue lists the queue
		{"chat hi there", Chat, "hi there", false},
		{"chat", "", "", true},
		{strings.Repeat("x", ChatMaxLen), "", "", true},
		{"chat " + strings.Repeat("x", ChatMaxLen), Chat, strings.Repeat("x", ChatMaxLen), false},
		{"chat " + strings.Repeat("x", ChatMaxLen+1), "", "", true},
		{"", "", "", true},
		{"frobnicate", "", "", true},
	}
	for _, tc := range cases {
		got, err := Parse(tc.in)
		if tc.isErr {
			if err == nil {
				t.Errorf("Parse(%q) = %v, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got.Kind != tc.kind || got.Arg != tc.arg {
			t.Errorf("Parse(%q) = {%s %q}, want {%s %q}", tc.in, got.Kind, got.Arg, tc.kind, tc.arg)
		}
	}
}
