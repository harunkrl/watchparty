//go:build audit3

package mpv

// Audit round 3 (worker2) — EVIDENCE. These tests document a reported
// bug; they are behind the `audit3` build tag so the normal battery
// stays green.
//
// Run: go test -tags audit3 ./internal/mpv/
//
// The pin now runs over the platform-neutral fake-conn seam (master-2,
// F3): the unix socket is transport, the truncated batch is logic.

import (
	"context"
	"testing"
	"time"
)

// A3-MPV1: a batch that mpv only partly answers is indistinguishable from
// a complete one. The poll hands the engine a state with no position and
// no flag, so the master reads it as "nothing is playing" and posts NO
// heartbeat for that tick — the room's clock silently stalls, and the
// follower eventually reports a stale master. Nothing in the log says the
// sample was truncated: a systematically truncated batch looks like a
// healthy room that is merely slow.
//
// A truncated sample must not be passed on as if it were a real one.
func TestAudit3TruncatedBatchIsNotPassedOn(t *testing.T) {
	orig := cmdTimeout
	cmdTimeout = 50 * time.Millisecond
	defer func() { cmdTimeout = orig }()

	p, _ := newFakeConf(t, nil, nil, true) // answers the first request only
	out := make(chan State, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Poll(ctx, 50*time.Millisecond, out)

	select {
	case st := <-out:
		if !st.Partial {
			t.Fatalf("a truncated batch produced a state that claims to be complete: %+v", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the poll never reported the truncated batch")
	}
}
