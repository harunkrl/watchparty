//go:build audit3 && !windows

package mpv

// Audit round 3 (worker2) — EVIDENCE. These tests FAIL against the current
// tree on purpose: each one documents a reported bug. They are behind the
// `audit3` build tag so the normal battery stays green.
//
// Run: go test -tags audit3 ./internal/mpv/

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// silentAfterFirstIPC is a fake mpv that answers the FIRST request of a
// batch and then goes quiet: the batch is truncated.
func silentAfterFirstIPC(t *testing.T) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "mpv.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 8192)
		answered := 0
		for {
			n, err := c.Read(buf)
			if n > 0 {
				for _, line := range bytes.Split(buf[:n], []byte("\n")) {
					if len(bytes.TrimSpace(line)) == 0 {
						continue
					}
					answered++
					if answered > 1 {
						continue // only the first request of the batch is answered
					}
					var req struct {
						Command   []any `json:"command"`
						RequestID int   `json:"request_id"`
					}
					if json.Unmarshal(line, &req) != nil || len(req.Command) < 2 {
						continue
					}
					resp, _ := json.Marshal(map[string]any{
						"error":      "success",
						"data":       false, // "pause"
						"request_id": req.RequestID,
					})
					if _, err := c.Write(append(resp, '\n')); err != nil {
						return
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return sock
}

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

	sock := silentAfterFirstIPC(t)
	p := testPlayer(t, sock)
	go p.readLoop()

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
