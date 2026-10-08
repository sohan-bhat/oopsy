//go:build windows

package snap

import (
	"golang.org/x/sys/windows"
)

// volumeRoots lists the fixed drives that may hold a volume store.
func volumeRoots() []string {
	var out []string
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		r, _ := windows.UTF16PtrFromString(root)
		if windows.GetDriveType(r) == windows.DRIVE_FIXED {
			out = append(out, root)
		}
	}
	return out
}
