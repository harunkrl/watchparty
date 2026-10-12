// Package room speaks the watchparty server protocol: SSE events in,
// POST /cmd and GET /files out. Field shapes mirror server.js verbatim.
package room

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Event is one wire message, both directions; SSE payloads and POST /cmd
// bodies differ only in which fields are set. `p` is the paused flag
// (plan section 1, fixed wire name). `pos` is the hello-time media
// position the server sends only when it has a live snapshot.
type Event struct {
	Type  string   `json:"type"`
	Nick  string   `json:"nick,omitempty"`
	Text  string   `json:"text,omitempty"`
	Name  string   `json:"name,omitempty"`
	Now   string   `json:"now,omitempty"`
	Queue []string `json:"queue,omitempty"`
	T     float64  `json:"t,omitempty"`
	P     bool     `json:"p,omitempty"`
	From  string   `json:"from,omitempty"`
	Pos   *float64 `json:"pos,omitempty"`
}

// readIdle bounds the SSE read silence before we declare the connection
// dead and let the outer reconnect loop take over. 3× the server ping
// interval so a single missed ping does not flap.
var readIdle = 45 * time.Second

// Client is one room connection. The two http.Clients split timeouts:
// SSE must live forever (ctx cancels it), calls are capped like client.py.
type Client struct {
	Base   string
	ID     string
	stream *http.Client
	call   *http.Client
}

func New(base, id string) *Client {
	return &Client{
		Base:   strings.TrimRight(base, "/"),
		ID:     id,
		stream: &http.Client{},
		// DisableKeepAlives: the server (node) reaps idle connections after
		// ~5s; a rare POST reusing such a connection times out instead of
		// being processed. Heartbeat posts are 2s apart so a fresh
		// connection per call costs nothing measurable.
		call: &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{DisableKeepAlives: true},
		},
	}
}

// Stream connects to /events and forwards decoded events to out until ctx
// is cancelled. Own echoes (From == ID, except hello) are dropped and
// malformed lines skipped, matching client.py's on_event / sse_loop.
// onConnect (optional) fires once the HTTP stream is up — the reconnect
// loop resets its backoff counter there (client.py parity).
func (c *Client) Stream(ctx context.Context, nick string, out chan<- Event, onConnect ...func()) error {
	u := fmt.Sprintf("%s/events?id=%s&nick=%s", c.Base, url.QueryEscape(c.ID), url.QueryEscape(nick))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if len(onConnect) > 0 {
		onConnect[0]()
	}
	// Watchdog: closes the body if no scanner progress for readIdle, so
	// the scanner errors out and the outer reconnect backoff takes over.
	// Go's stdlib has no client-side per-read deadline API (NewResponseController
	// requires http.ResponseWriter, the server side), so we achieve the
	// same observable behavior with a timer + body close.
	wctx, wcancel := context.WithCancel(ctx)
	reset := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTimer(readIdle)
		defer t.Stop()
		for {
			select {
			case <-wctx.Done():
				return
			case <-reset:
				if !t.Stop() {
					select {
					case <-t.C:
					default:
					}
				}
				t.Reset(readIdle)
			case <-t.C:
				resp.Body.Close() // unblocks scanner; outer defer is idempotent
				return
			}
		}
	}()
	// cancel and reap the watchdog before returning: it must never outlive
	// Stream (and never touch package state after the call is done)
	defer func() {
		wcancel()
		<-done
	}()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		select {
		case reset <- struct{}{}: // any line (incl. `: ping` keepalives) is proof of life
		default:
		}
		line := sc.Text()
		if strings.HasPrefix(line, ":") { // SSE comment: server keepalive ping
			continue
		}
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue // blank separator or a field we do not use
		}
		var ev Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(payload, " ")), &ev); err != nil {
			continue
		}
		if ev.From == c.ID && ev.Type != "hello" {
			continue
		}
		select {
		case out <- ev:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return sc.Err()
}

// Post sends one command to /cmd with our id attached (client.py parity:
// every post carries `from` so the server can skip our echo).
func (c *Client) Post(ev Event) error {
	ev.From = c.ID
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	resp, err := c.call.Post(c.Base+"/cmd", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("server said %s", resp.Status)
	}
	return nil
}

// Files lists the video names the server exposes.
func (c *Client) Files() ([]string, error) {
	resp, err := c.call.Get(c.Base + "/files")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("server said %s", resp.Status)
	}
	var files []string
	return files, json.NewDecoder(resp.Body).Decode(&files)
}
