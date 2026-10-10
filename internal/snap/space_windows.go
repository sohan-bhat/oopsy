//go:build windows

package snap

import "golang.org/x/sys/windows"

// diskSpace returns the bytes available to this user and the volume size.
func diskSpace(p string) (int64, int64, error) {
	u, err := utf16Path(p)
	if err != nil {
		return 0, 0, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(u, &avail, &total, &free); err != nil {
		return 0, 0, err
	}
	return int64(avail), int64(total), nil
}
