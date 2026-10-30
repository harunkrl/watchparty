package mpv

// W6: the IPC layer must be pinned on EVERY platform. The unix-socket
// fakes are !windows-tagged, so the windows CI job executed none of it
// (A4's "the IPC layer finally executes" was wrong). These pins drive the
// IPC over net.Pipe: platform-neutral, no mpv, no real socket.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// pipeIPC is the far end of the client's IPC connection: it answers
// get_property requests with canned values and can push events.
type pipeIPC struct {
	conn net.Conn
}

// serve answers the client's requests until the connection dies. A
// property that is not in values is answered like mpv does: an error.
// values == nil means a WEDGED mpv: the requests are read and nothing
// ever comes back.
func (f *pipeIPC) serve(values map[string]any) {
	sc := bufio.NewScanner(f.conn)
	for sc.Scan() {
		if values == nil {
			continue // wedged: swallow the request, never answer
		}
		var req struct {
			Command   []any `json:"command"`
			RequestID int   `json:"request_id"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			continue
		}
		prop, _ := req.Command[1].(string)
		v, ok := values[prop]
		if !ok {
			fmt.Fprintf(f.conn, `{"error":"property not found","request_id":%d}`+"\n", req.RequestID)
			continue
		}
		b, _ := json.Marshal(v)
		fmt.Fprintf(f.conn, `{"error":"success","data":%s,"request_id":%d}`+"\n", b, req.RequestID)
	}
}

// push sends a raw notification line (mpv's event stream).
func (f *pipeIPC) push(line string) {
	fmt.Fprintf(f.conn, "%s\n", line)
}

// newFakePlayer wires a Player to a fake IPC over net.Pipe.
func newFakePlayer(t *testing.T, values map[string]any) (*Player, *pipeIPC) {
	t.Helper()
	client, server := net.Pipe()
	p := &Player{
		base:   "http://127.0.0.1:1",
		conn:   client,
		dead:   make(chan struct{}),
		events: make(chan Event, 64),
		pend:   map[int]chan ipcMsg{},
	}
	f := &pipeIPC{conn: server}
	go f.serve(values)
	go p.readLoop()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	return p, f
}

// One poll tick must cost ONE IPC round trip and every property must
// come back (the batch is the client's only way to read the player).
func TestPipeBatchGetRoundTrip(t *testing.T) {
	p, _ := newFakePlayer(t, map[string]any{"pause": true, "time-pos": 12.5})
	props, complete := p.batchGet([]string{"pause", "time-pos"})
	if !complete {
		t.Fatal("a served batch must be reported complete")
	}
	if !boolProp(props, "pause") {
		t.Fatalf("pause did not come back: %v", props)
	}
	if v, ok := numProp(props, "time-pos"); !ok || v != 12.5 {
		t.Fatalf("time-pos did not come back: %v", props)
	}
}

// A property mpv does not answer must not fail the whole batch.
func TestPipeBatchGetSkipsUnanswered(t *testing.T) {
	p, _ := newFakePlayer(t, map[string]any{"pause": false})
	props, complete := p.batchGet([]string{"pause", "track-list"})
	if !complete {
		t.Fatal("a served batch must be reported complete")
	}
	if _, ok := props["track-list"]; ok {
		t.Fatalf("an unanswered property must not appear: %v", props)
	}
}

// A wedged mpv (it never answers) must not hang the poll: the batch
// times out and the sample is marked incomplete.
func TestPipeBatchGetTimesOutOnAWedgedMPV(t *testing.T) {
	orig := cmdTimeout
	cmdTimeout = 200 * time.Millisecond
	defer func() { cmdTimeout = orig }()

	p, _ := newFakePlayer(t, nil) // the fake answers nothing
	start := time.Now()
	props, complete := p.batchGet([]string{"pause"})
	if complete {
		t.Fatal("a wedged mpv must not report a complete batch")
	}
	if len(props) != 0 {
		t.Fatalf("a timed-out batch must carry no values: %v", props)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("the batch did not time out promptly: %v", el)
	}
}

// The IPC dying under us must be reported: the client cannot drive mpv
// any more and has to say so instead of freezing on a stale frame.
func TestPipeDeadIPCIsReported(t *testing.T) {
	p, fake := newFakePlayer(t, nil)
	fake.conn.Close() // mpv hangs up
	select {
	case <-p.dead:
	case <-time.After(3 * time.Second):
		t.Fatal("the dead IPC was not reported")
	}
}

// A dropped event must never be silent: a silent drop hid user seeks
// for a whole day of soak flake.
func TestPipeDroppedEventsAreReported(t *testing.T) {
	p, fake := newFakePlayer(t, nil)
	logged := make(chan string, 8)
	p.SetLogf(func(f string, a ...any) { logged <- fmt.Sprintf(f, a...) })
	for i := 0; i < cap(p.events); i++ {
		p.events <- Event{Name: "pause"} // the engine is stalled
	}
	fake.push(`{"event":"property-change","name":"pause","data":true}`)
	select {
	case line := <-logged:
		if !strings.Contains(line, "event dropped") {
			t.Fatalf("the dropped event was not reported: %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the dropped event was not reported at all")
	}
}

// A pushed event that fits must reach the engine (the instant sync path).
func TestPipePushedEventsReachTheEngine(t *testing.T) {
	p, fake := newFakePlayer(t, nil)
	fake.push(`{"event":"property-change","name":"pause","data":true}`)
	select {
	case ev := <-p.Events():
		if ev.Name != "pause" {
			t.Fatalf("event %q, want pause", ev.Name)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the pushed event never arrived")
	}
}
