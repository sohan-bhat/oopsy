//go:build windows

package e2e

import (
	"syscall"

	"golang.org/x/sys/windows"
)

const attrMask = windows.FILE_ATTRIBUTE_READONLY | windows.FILE_ATTRIBUTE_HIDDEN | windows.FILE_ATTRIBUTE_SYSTEM | windows.FILE_ATTRIBUTE_ARCHIVE

// winMeta returns p's kept attributes and creation time, without following
// links.
func winMeta(p string) (uint32, int64) {
	var d syscall.Win32FileAttributeData
	u, _ := windows.UTF16PtrFromString(p)
	if err := syscall.GetFileAttributesEx(u, syscall.GetFileExInfoStandard, (*byte)(unsafePointer(&d))); err != nil {
		return 0, 0
	}
	return d.FileAttributes & attrMask, d.CreationTime.Nanoseconds()
}

func setWinAttrs(p string, a uint32) {
	u, _ := windows.UTF16PtrFromString(p)
	windows.SetFileAttributes(u, a)
}
