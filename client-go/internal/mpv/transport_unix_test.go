//go:build !windows

package mpv

// The unix IPC transport: dialPipe must reach a real unix socket and
// carry mpv's line protocol.
//
// Everything ABOVE the socket (the batch round trip, the poll, the
// timeouts, the dropped events, the dead connection, the bounded Load,
// the quiet Close) is logic and is pinned platform-neutrally in
// ipc_fake_test.go. The named-pipe path has its own pins in
// mpv_windows_test.go. The unix-only fakes that merely repeated that
// logic were retired (master-2, F3) — this file keeps the transport.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestDialPipeCarriesTheLineProtocol(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "mpv.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		sc := bufio.NewScanner(c)
		if sc.Scan() {
			var req struct {
				RequestID int `json:"request_id"`
			}
			if json.Unmarshal(sc.Bytes(), &req) == nil {
				fmt.Fprintf(c, `{"error":"success","data":true,"request_id":%d}`+"\n", req.RequestID)
			}
		}
		c.Close() // mpv hangs up: the socket must die with it
	}()

	conn, err := dialPipe(sock)
	if err != nil {
		t.Fatalf("dialPipe failed: %v", err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, `{"command":["get_property","pause"],"request_id":7}`+"\n"); err != nil {
		t.Fatalf("the command did not travel: %v", err)
	}
	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("no answer came back over the socket: %v", err)
	}
	var resp struct {
		Error     string `json:"error"`
		RequestID int    `json:"request_id"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("the answer was not JSON: %v", err)
	}
	if resp.Error != "success" || resp.RequestID != 7 {
		t.Fatalf("the answer was wrong: %+v", resp)
	}
	// the socket's death must surface as a read error, not as silence
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := br.ReadBytes('\n'); err == nil {
		t.Fatal("the socket death did not surface as a read error")
	}
}
