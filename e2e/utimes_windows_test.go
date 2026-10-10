//go:build windows

package e2e

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// utimesNoFollow sets access and write times on p itself, even when p is a
// symlink or junction.
func utimesNoFollow(p string, ts []syscall.Timespec) error {
	u, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(u, windows.FILE_WRITE_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	at := windows.NsecToFiletime(syscall.TimespecToNsec(ts[0]))
	wt := windows.NsecToFiletime(syscall.TimespecToNsec(ts[1]))
	return windows.SetFileTime(h, nil, &at, &wt)
}
