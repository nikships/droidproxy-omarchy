//go:build !windows

package journal

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// openFileNoFollow opens path without following a final symlink.
func openFileNoFollow(path string, flags int, perm fs.FileMode, nonblock bool) (*os.File, error) {
	flags |= syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	if nonblock {
		flags |= syscall.O_NONBLOCK
	}
	return os.OpenFile(path, flags, perm)
}

// openFilePlain opens path, following symlinks.
func openFilePlain(path string, flags int, perm fs.FileMode, nonblock bool) (*os.File, error) {
	flags |= syscall.O_CLOEXEC
	if nonblock {
		flags |= syscall.O_NONBLOCK
	}
	return os.OpenFile(path, flags, perm)
}

func tryFlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func lockBusy(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR)
}

func fileInode(info fs.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

func ownedByMe(_ string, info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

func checkOwner(path string, info fs.FileInfo) error {
	if !ownedByMe(path, info) {
		return fmt.Errorf("%s is not owned by this user", info.Name())
	}
	return nil
}
