// Package cmd parses terminal input lines into commands.
package cmd

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ChatMaxLen mirrors the plan's fixed numbers: a client-side guard,
// the server does not limit chat length.
const ChatMaxLen = 500

const (
	List  = "list"
	Pick  = "pick"
	Play  = "play"
	Queue = "queue"
	Skip  = "skip"
	Chat  = "chat"
	Quit  = "quit"
	Help  = "help"
)

const Usage = "commands: list | pick | play <file> | queue [file] | skip | chat <msg> | quit | help"

type Cmd struct {
	Kind string
	Arg  string
}

// IsCommand reports whether the line is one of the known verbs. Anything
// else is chat. The headless adapter is verb-first (no leading '/');
// the TUI instead requires the slash and sends everything else as chat.
func IsCommand(line string) bool {
	verb, _, _ := strings.Cut(strings.TrimSpace(line), " ")
	switch verb {
	case List, Pick, Play, Queue, Skip, Chat, Quit, Help:
		return true
	}
	return false
}

// Parse splits a line into verb + argument. Like client.py, verbs that
// take no argument ignore any extra text.
func Parse(line string) (Cmd, error) {
	verb, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
	arg = strings.TrimSpace(arg)
	switch verb {
	case List, Pick, Skip, Quit, Help:
		return Cmd{Kind: verb}, nil
	case Queue:
		// bare "queue" lists the queue; "queue <file>" enqueues
		return Cmd{Kind: Queue, Arg: arg}, nil
	case Play, Chat:
		if arg == "" {
			return Cmd{}, fmt.Errorf("%s needs an argument\n%s", verb, Usage)
		}
		if verb == Chat && utf8.RuneCountInString(arg) > ChatMaxLen {
			return Cmd{}, fmt.Errorf("chat too long (max %d chars)", ChatMaxLen)
		}
		return Cmd{Kind: verb, Arg: arg}, nil
	default:
		return Cmd{}, errors.New(Usage)
	}
}
