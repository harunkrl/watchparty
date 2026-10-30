package room

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// m2-S4: the reconnect backoff must reset only when the server admits us
// with a hello. An HTTP 200 alone proves nothing — during an outage the
// POSTs still answer while the SSE is dead, and resetting the backoff
// there would hammer the server.
func TestStreamResetsBackoffOnlyOnHello(t *testing.T) {
	// a server that answers 200 but never says hello
	pingOnly := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		ticker := time.NewTicker(50 * time.Millisecond)
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
	defer pingOnly.Close()

	connected := make(chan struct{}, 1)
	c := New(pingOnly.URL, "alice-1234")
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan Event, 4)
	errc := make(chan error, 1)
	go func() { errc <- c.Stream(ctx, "alice", out, func() { connected <- struct{}{} }) }()

	select {
	case <-connected:
		cancel()
		<-errc
		t.Fatal("a 200 without a hello must not reset the backoff")
	case <-time.After(500 * time.Millisecond):
	}
	cancel()
	<-errc // the stream must be gone before the next test touches readIdle

	// a server that says hello: the backoff resets
	hello := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"type\":\"hello\",\"now\":\"\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer hello.Close()

	c2 := New(hello.URL, "alice-1234")
	ctx2, cancel2 := context.WithCancel(context.Background())
	out2 := make(chan Event, 4)
	connected2 := make(chan struct{}, 1)
	errc2 := make(chan error, 1)
	go func() {
		errc2 <- c2.Stream(ctx2, "alice", out2, func() { connected2 <- struct{}{} })
	}()

	select {
	case <-connected2:
	case <-time.After(2 * time.Second):
		cancel2()
		<-errc2
		t.Fatal("the hello must reset the backoff")
	}
	cancel2()
	<-errc2
}
