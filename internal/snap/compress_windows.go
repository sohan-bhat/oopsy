//go:build windows

package snap

import (
	"io/fs"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

const compressionFormatDefault = 1 // COMPRESSION_FORMAT_DEFAULT, LZNT1 on NTFS

// compressDir turns on NTFS compression for dir, so every file and folder
// created inside it afterwards is stored compressed. It reports false where
// the volume cannot compress (ReFS, FAT, clusters over 4 KB).
func compressDir(dir string) bool {
	h, err := openNoFollow(dir, windows.FILE_READ_DATA|windows.FILE_WRITE_DATA|windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES, true)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	format := uint16(compressionFormatDefault)
	var ret uint32
	return windows.DeviceIoControl(h, windows.FSCTL_SET_COMPRESSION, (*byte)(unsafe.Pointer(&format)), 2, nil, 0, &ret, nil) == nil
}

// isCompressed reports whether p is stored NTFS compressed.
func isCompressed(p string) bool {
	u, err := utf16Path(p)
	if err != nil {
		return false
	}
	a, err := windows.GetFileAttributes(u)
	return err == nil && a&windows.FILE_ATTRIBUTE_COMPRESSED != 0
}

var procGetCompressedFileSize = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetCompressedFileSizeW")

// diskUsage adds up the bytes p's files actually occupy, which for
// compressed files is their compressed size.
func diskUsage(p string) int64 {
	var total int64
	_ = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		u, err := utf16Path(path)
		if err != nil {
			return nil
		}
		var high uint32
		low, _, callErr := procGetCompressedFileSize.Call(uintptr(unsafe.Pointer(u)), uintptr(unsafe.Pointer(&high)))
		if uint32(low) == 0xFFFFFFFF && callErr != windows.ERROR_SUCCESS {
			return nil
		}
		total += int64(high)<<32 | int64(uint32(low))
		return nil
	})
	return total
}
