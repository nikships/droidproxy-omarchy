package journal

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// openFileNoFollow opens path and rejects it when the opened handle is a
// reparse point. Windows has no O_NOFOLLOW, so the check runs on the handle
// right after open; callers fingerprint the path before and after, which
// bounds the swap window the same way the unix checks do.
func openFileNoFollow(path string, flags int, perm fs.FileMode, _ bool) (*os.File, error) {
	f, err := os.OpenFile(path, flags, perm)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err == nil &&
		info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		f.Close()
		return nil, fmt.Errorf("%s is a symbolic link", path)
	}
	return f, nil
}

// openFilePlain opens path, following symlinks.
func openFilePlain(path string, flags int, perm fs.FileMode, _ bool) (*os.File, error) {
	return os.OpenFile(path, flags, perm)
}

func tryFlock(f *os.File) error {
	overlapped := &windows.Overlapped{}
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, overlapped,
	)
}

func lockBusy(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

// fileInode has no Lstat equivalent on Windows; fingerprints fall back to
// size and mtime there.
func fileInode(_ fs.FileInfo) uint64 { return 0 }

func ownedByMe(path string, _ fs.FileInfo) bool {
	sd, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil || sd == nil {
		return false
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return false
	}
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false
	}
	return owner.Equals(tokenUser.User.Sid)
}

func checkOwner(path string, info fs.FileInfo) error {
	if !ownedByMe(path, info) {
		return fmt.Errorf("%s is not owned by this user", info.Name())
	}
	return nil
}
