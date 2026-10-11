//go:build !windows

package mpv

import (
	"net"
	"os"
	"syscall"
)

func dialPipe(path string) (net.Conn, error) {
	return net.Dial("unix", path)
}

// terminate asks politely first; Close escalates to Kill after 2s.
func terminate(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}
