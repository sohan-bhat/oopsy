//go:build !windows

package e2e

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func utimesNoFollow(p string, ts []syscall.Timespec) error {
	u := []unix.Timespec{{Sec: ts[0].Sec, Nsec: ts[0].Nsec}, {Sec: ts[1].Sec, Nsec: ts[1].Nsec}}
	return unix.UtimesNanoAt(unix.AT_FDCWD, p, u, unix.AT_SYMLINK_NOFOLLOW)
}
