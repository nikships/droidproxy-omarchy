//go:build !windows

package backend

import (
	"os"
	"syscall"
)

// terminate asks p to exit gracefully.
func terminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
