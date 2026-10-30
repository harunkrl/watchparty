package room

import (
	"testing"
	"time"
)

// The SSE watchdog must outlast the server's 15s keepalive ping (a
// missed ping must not flap the connection) but must not keep a
// sleeping laptop waiting for a stale room.
func TestReadIdleCoversTheServerPing(t *testing.T) {
	if readIdle <= 15*time.Second {
		t.Fatalf("readIdle %v must outlast the server's 15s ping", readIdle)
	}
	if readIdle > 30*time.Second {
		t.Fatalf("readIdle %v is too long: a sleeping laptop waits that long", readIdle)
	}
}
