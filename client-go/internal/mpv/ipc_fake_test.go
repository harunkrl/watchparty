package mpv

// W6: the IPC layer must be pinned on EVERY platform. The unix-socket
// fakes are !windows-tagged, so the windows CI job executed none of it
// (A4's "the IPC layer finally executes" was wrong). These pins drive the
// IPC over net.Pipe: platform-neutral, no mpv, no real socket.
//
// This file holds the LOGIC of the IPC layer: the batch round trip, the
// poll, the timeouts, the dropped events, the dead connection, the
// bounded Load, the quiet Close. The transport itself (the real unix
// socket dial) lives in transport_unix_test.go; the named-pipe path has
// its own pins in mpv_windows_test.go. The unix-only fakes that merely
// repeated the logic below were retired (master-2, F3).

import (
	"bufio"
	"bytes"
	"context"
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
	// firstOnly makes the fake answer the FIRST request of a batch and
	// then go quiet: the batch comes back truncated (A3-MPV1).
	firstOnly bool
}

// serve answers the client's requests until the connection dies. A
// property that is not in values is answered like mpv does: an error.
// values == nil means a WEDGED mpv: the requests are read and nothing
// ever comes back.
func (f *pipeIPC) serve(values map[string]any) {
	sc := bufio.NewScanner(f.conn)
	answered := 0
	for sc.Scan() {
		if values == nil && !f.firstOnly {
			continue // wedged: swallow the request, never answer
		}
		var req struct {
			Command   []any `json:"command"`
			RequestID int   `json:"request_id"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			continue
		}
		answered++
		if f.firstOnly && answered > 1 {
			continue // only the first request of the batch is answered
		}
		prop, _ := req.Command[1].(string)
		v, ok := values[prop]
		if !ok && values == nil {
			v = false // the truncating fake answers anything — once
		}
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
	return newFake(t, values, nil)
}

// newFake is newFakePlayer plus an optional read counter: the poll pins
// must see how many lines arrived in ONE read (the batch travels as one
// write, not one write per property).
func newFake(t *testing.T, values map[string]any, lines chan<- int) (*Player, *pipeIPC) {
	return newFakeConf(t, values, lines, false)
}

// newFakeConf spells out the fake mpv's behaviour: a firstOnly mpv
// answers the FIRST request of a batch and then goes quiet.
func newFakeConf(t *testing.T, values map[string]any, lines chan<- int, firstOnly bool) (*Player, *pipeIPC) {
	t.Helper()
	client, server := net.Pipe()
	p := &Player{
		base:   "http://127.0.0.1:1",
		conn:   client,
		dead:   make(chan struct{}),
		events: make(chan Event, 64),
		pend:   map[int]chan ipcMsg{},
	}
	var far net.Conn = server
	if lines != nil {
		far = countReads{Conn: server, lines: lines}
	}
	f := &pipeIPC{conn: far, firstOnly: firstOnly}
	go f.serve(values)
	go p.readLoop()
	t.Cleanup(func() {
		client.Close()
		server.Close()
	})
	return p, f
}

// countReads counts the lines that arrived in one read.
type countReads struct {
	net.Conn
	lines chan<- int
}

func (c countReads) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		select {
		case c.lines <- bytes.Count(b[:n], []byte("\n")):
		default:
		}
	}
	return n, err
}

// pipeValues is what a real mpv answers for the poll properties: the
// poll must fill the whole state out of ONE batch.
func pipeValues() map[string]any {
	return map[string]any{
		"pause": false, "idle-active": false, "paused-for-cache": false,
		"time-pos": 12.5, "demuxer-cache-time": 3.5, "volume": 55.0,
		"mute": true, "duration": 90.0,
		"audio-params": map[string]any{"channels": "stereo", "channel-count": 2},
		"track-list": []any{
			map[string]any{"id": 1, "type": "audio", "selected": true},
			map[string]any{"id": 2, "type": "sub", "lang": "trg", "selected": true},
		},
	}
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
	// and the poll must say so too: the client shuts down instead of
	// sampling nothing forever
	out := make(chan State, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Poll(ctx, 50*time.Millisecond, out)
	select {
	case st := <-out:
		if !st.Exited {
			t.Fatalf("the dead IPC was reported as %+v, want Exited", st)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the poll never reported the dead IPC")
	}
}

// H2/H5: a property mpv does not answer must not break the sample — the
// layout and the subtitle stay EMPTY (the UI must not guess "stereo").
func TestMissingPropertyLeavesTheSampleHonest(t *testing.T) {
	props := map[string]any{"pause": true}
	if l := audioLayout(props); l != "" {
		t.Fatalf("layout without audio-params = %q, want empty", l)
	}
	if s := subLang(props); s != "" {
		t.Fatalf("subtitle without track-list = %q, want empty", s)
	}
	// a track list without an active subtitle stays empty too
	props = map[string]any{"track-list": []any{
		map[string]any{"type": "sub", "lang": "trg", "selected": false},
		map[string]any{"type": "audio", "selected": true},
	}}
	if s := subLang(props); s != "" {
		t.Fatalf("subtitle without a selected track = %q, want empty", s)
	}
	// an unknown language is not a language
	props = map[string]any{"track-list": []any{
		map[string]any{"type": "sub", "lang": "und", "selected": true},
	}}
	if s := subLang(props); s != "" {
		t.Fatalf("subtitle with an unknown language = %q, want empty", s)
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

// One poll tick must cost ONE IPC round trip and fill the whole state.
// (The unix-only pin, now on every platform: the windows CI job ran
// none of the IPC layer.)
func TestPipePollReadsBatchInOneRoundTrip(t *testing.T) {
	lines := make(chan int, 8)
	p, _ := newFake(t, pipeValues(), lines)
	out := make(chan State, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Poll(ctx, 50*time.Millisecond, out)

	select {
	case st := <-out:
		if st.Pos == nil || *st.Pos != 12.5 {
			t.Fatalf("pos = %v, want 12.5", st.Pos)
		}
		if st.Volume != 55 || st.Cache != 3.5 {
			t.Fatalf("volume/cache = %v/%v", st.Volume, st.Cache)
		}
		if !st.Muted {
			t.Fatal("the mute flag was not polled")
		}
		if st.Dur != 90 {
			t.Fatalf("duration = %v, want 90", st.Dur)
		}
		if st.Pause || st.Idle || st.Buffering {
			t.Fatalf("unexpected flags: %+v", st)
		}
		if st.Partial {
			t.Fatal("a complete batch must not be marked partial")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the poll never produced a state")
	}
	select {
	case n := <-lines:
		if n != len(pollProps) {
			t.Fatalf("the poll travelled in %d lines, want %d (one round trip)", n, len(pollProps))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the fake mpv never saw the poll batch")
	}
}

// The poll fills the audio layout and the active subtitle out of the
// same batch (H2/H5) — the UI must not guess them.
func TestPipePollReadsLayoutAndSubtitle(t *testing.T) {
	p, _ := newFake(t, pipeValues(), nil)
	out := make(chan State, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Poll(ctx, 50*time.Millisecond, out)

	select {
	case st := <-out:
		if st.Layout != "stereo" {
			t.Fatalf("layout = %q, want stereo", st.Layout)
		}
		if st.Sub != "trg" {
			t.Fatalf("subtitle = %q, want trg", st.Sub)
		}
		if st.Partial {
			t.Fatal("a complete batch must not be marked partial")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the poll never produced a state")
	}
}

// A wedged mpv must not pin the caller: Load waits for the file to go
// live and the engine holds its lock across that call (OnRoom -> load ->
// Player.Load), so the wait must be bounded.
func TestPipeLoadWaitIsBoundedWhenMpvIsWedged(t *testing.T) {
	origCmd, origWait := cmdTimeout, loadWait
	cmdTimeout, loadWait = 20*time.Millisecond, 200*time.Millisecond
	defer func() { cmdTimeout, loadWait = origCmd, origWait }()

	p, _ := newFake(t, nil, nil) // wedged: the requests are swallowed
	defer p.Close()

	start := time.Now()
	_ = p.Load("movie.mkv")
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("Load blocked %s on a wedged mpv (the wait must be bounded)", el)
	}
}

// Close is the normal end of life: the conn dies on purpose, which must
// not be reported as a crash.
func TestPipeCloseDoesNotReportDeadIPC(t *testing.T) {
	p, _ := newFake(t, nil, nil)
	p.Close()
	select {
	case <-p.dead:
		t.Fatal("Close reported a dead IPC")
	case <-time.After(300 * time.Millisecond):
	}
}

// A wedged mpv (a SIGSTOP'd player, say) must not block the pause: the
// command is written without waiting for mpv's reply (a tell), so a dead
// player cannot freeze the interface.
func TestPipeSetPauseDoesNotWaitForTheReply(t *testing.T) {
	p, _ := newFakePlayer(t, nil) // wedged: it never answers
	start := time.Now()
	if err := p.SetPause(true); err != nil {
		t.Fatalf("the pause command failed: %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("the pause waited %s for mpv's reply", el)
	}
}
