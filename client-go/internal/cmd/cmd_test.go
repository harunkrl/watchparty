package cmd

import (
	"strings"
	"testing"
)

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
		{"queue", "", "", true},
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
