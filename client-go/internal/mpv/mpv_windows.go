//go:build windows

package mpv

import (
	"net"
	"os"
	"time"

	"github.com/Microsoft/go-winio"
)

func dialPipe(path string) (net.Conn, error) {
	to := 200 * time.Millisecond
	return winio.DialPipe(path, &to)
}

// Windows has no SIGTERM; python's terminate() is TerminateProcess there
// too, so we go straight to Kill (Close's 2s grace is unix-only).
func terminate(p *os.Process) error {
	return p.Kill()
}
