package journal

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// openFileNoFollow opens path without following a final reparse point
// (symlink or junction) and rejects one. Windows has no O_NOFOLLOW, and
// os.OpenFile only passes FILE_FLAG_OPEN_REPARSE_POINT through from Go 1.26,
// so this calls CreateFile itself. It supports the flags the journal uses:
// O_RDONLY or O_RDWR, optionally with O_CREATE|O_EXCL. O_APPEND is not
// honored: an append-only handle can't truncate a torn tail, so appendLine
// seeks to the end itself.
func openFileNoFollow(path string, flags int, _ fs.FileMode, _ bool) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	access := uint32(windows.GENERIC_READ)
	if flags&os.O_RDWR != 0 {
		access |= windows.GENERIC_WRITE
	}
	create := uint32(windows.OPEN_EXISTING)
	if flags&(os.O_CREATE|os.O_EXCL) == os.O_CREATE|os.O_EXCL {
		create = windows.CREATE_NEW
	}
	h, err := windows.CreateFile(name, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, create,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		windows.CloseHandle(h)
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("%s is a symbolic link", path)
	}
	return os.NewFile(uintptr(h), path), nil
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
