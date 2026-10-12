package room

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// readIdle watches for scanner progress and would otherwise keep SSE
// connections stuck forever on a silently-dead peer (DERP-replayed TCP
// FIN/RST never arrives). This test makes a server send the hello then
// go silent, with readIdle shrunk to 300ms, and asserts Stream returns
// an error well under 2 seconds.
func TestStreamWatchesDeadIdle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"type\":\"hello\",\"now\":\"\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // hold the connection open until client disconnects
	}))
	defer srv.Close()

	orig := readIdle
	readIdle = 300 * time.Millisecond
	defer func() { readIdle = orig }()

	c := New(srv.URL, "alice-1234")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	out := make(chan Event, 4)
	errc := make(chan error, 1)
	start := time.Now()
	go func() { errc <- c.Stream(ctx, "alice", out) }()

	select {
	case err := <-errc:
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("Stream took %v to detect silent server; readIdle=%v should trip well under 2s",
				elapsed, readIdle)
		}
		if err == nil {
			t.Fatalf("Stream returned nil error for silently-dead SSE peer")
		}
	case <-ctx.Done():
		t.Fatalf("Stream did not return within 3s; watchdog did not fire")
	}
}

// The server's keepalive is a `: ping` comment line, not a data event.
// A stream carrying only pings is healthy and must survive well past
// readIdle; the watchdog resets on any received line (regression: an
// earlier version only reset on delivered events and killed idle-but-
// healthy connections every 45s).
func TestStreamPingsKeepAlive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"type\":\"hello\",\"now\":\"\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		ticker := time.NewTicker(100 * time.Millisecond) // ping-only traffic
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				fmt.Fprint(w, ": ping\n\n")
				if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			}
		}
	}))
	defer srv.Close()

	orig := readIdle
	readIdle = 300 * time.Millisecond
	defer func() { readIdle = orig }()

	c := New(srv.URL, "alice-1234")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out := make(chan Event, 4)
	errc := make(chan error, 1)
	go func() { errc <- c.Stream(ctx, "alice", out) }()

	select {
	case err := <-errc:
		t.Fatalf("Stream died on ping-only traffic (watchdog must count keepalives): %v", err)
	case <-time.After(3 * readIdle):
	}
	cancel()
	<-errc
}
