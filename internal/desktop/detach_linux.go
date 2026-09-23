package desktop

import "syscall"

// detachedAttr puts a detached child into its own session so it cannot be
// taken down with the daemon's process group.
func detachedAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
