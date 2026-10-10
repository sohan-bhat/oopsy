//go:build windows

package snap

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// allocBytes is what a file takes on disk: its compressed or allocated
// size, which is less than its length for sparse and compressed files.
func allocBytes(p string, fi os.FileInfo) int64 {
	d := winData(fi)
	if d == nil || d.FileAttributes&(windows.FILE_ATTRIBUTE_SPARSE_FILE|windows.FILE_ATTRIBUTE_COMPRESSED) == 0 {
		return fi.Size()
	}
	u, err := utf16Path(p)
	if err != nil {
		return fi.Size()
	}
	var high uint32
	low, _, callErr := procGetCompressedFileSize.Call(uintptr(unsafe.Pointer(u)), uintptr(unsafe.Pointer(&high)))
	if uint32(low) == 0xFFFFFFFF && callErr != windows.ERROR_SUCCESS {
		return fi.Size()
	}
	return int64(high)<<32 | int64(uint32(low))
}

func inodeOf(os.FileInfo) uint64 { return 0 }

// dataRegions is left to reading on Windows; zero runs still become holes.
func dataRegions(*os.File, int64) ([][2]int64, bool) { return nil, false }

// markSparse makes out sparse when in is, so skipped zero runs stay holes.
func markSparse(in, out *os.File) {
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(windows.Handle(in.Fd()), &info) != nil ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_SPARSE_FILE == 0 {
		return
	}
	var ret uint32
	_ = windows.DeviceIoControl(windows.Handle(out.Fd()), windows.FSCTL_SET_SPARSE, nil, 0, nil, 0, &ret, nil)
}
