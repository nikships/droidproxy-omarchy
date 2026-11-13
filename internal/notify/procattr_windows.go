package notify

import "syscall"

// detachedAttr starts a detached child with no console window so it cannot
// be taken down with the daemon and never flashes a console on screen.
func detachedAttr() *syscall.SysProcAttr {
	const (
		createNoWindow     = 0x08000000
		detachedProcess    = 0x00000008
		createNewProcGroup = 0x00000200
	)
	return &syscall.SysProcAttr{
		CreationFlags: createNoWindow | detachedProcess | createNewProcGroup,
	}
}
