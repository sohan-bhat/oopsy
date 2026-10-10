//go:build windows

package snap

import (
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// findData is WIN32_FIND_DATAW as the system really lays it out.
type findData struct {
	FileAttributes    uint32
	CreationTime      windows.Filetime
	LastAccessTime    windows.Filetime
	LastWriteTime     windows.Filetime
	FileSizeHigh      uint32
	FileSizeLow       uint32
	Reserved0         uint32
	Reserved1         uint32
	FileName          [windows.MAX_PATH]uint16
	AlternateFileName [14]uint16
	_                 [3]uint32 // dwFileType, dwCreatorType, wFinderFlags on some systems
}

var (
	kernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procFindFirstEx   = kernel32.NewProc("FindFirstFileExW")
	procFindNextFileW = kernel32.NewProc("FindNextFileW")
)

const (
	findExInfoBasic       = 1
	findExSearchNameMatch = 0
	findFirstExLargeFetch = 2
)

// listDirFind lists a folder with FindFirstFileEx in its fastest mode: no
// short names, and large fetches.
func listDirFind(dir string) ([]dirEntry, error) {
	pattern, err := windows.UTF16PtrFromString(longPath(filepath.Join(dir, "*")))
	if err != nil {
		return nil, err
	}
	var fd findData
	h, _, callErr := procFindFirstEx.Call(uintptr(unsafe.Pointer(pattern)), findExInfoBasic, uintptr(unsafe.Pointer(&fd)),
		findExSearchNameMatch, 0, findFirstExLargeFetch)
	if windows.Handle(h) == windows.InvalidHandle {
		return nil, callErr
	}
	defer windows.FindClose(windows.Handle(h))
	var out []dirEntry
	for {
		name := windows.UTF16ToString(fd.FileName[:])
		if name != "." && name != ".." {
			e := dirEntry{name: name, mode: fd.FileAttributes & keptAttrs,
				mtime: fd.LastWriteTime.Nanoseconds()}
			switch {
			case fd.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
				e.kind = 'l'
			case fd.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0:
				e.kind = 'd'
				e.size = fd.CreationTime.Nanoseconds()
			default:
				e.kind = 'f'
				e.size = int64(fd.FileSizeHigh)<<32 | int64(fd.FileSizeLow)
			}
			out = append(out, e)
		}
		r, _, _ := procFindNextFileW.Call(h, uintptr(unsafe.Pointer(&fd)))
		if r == 0 {
			break
		}
	}
	return out, nil
}

// CompareListers walks root with both listers and reports their times and
// entry counts, for measuring on Windows CI.
func CompareListers(root string) (readDir, find time.Duration, a, b int) {
	walk := func(f func(string) ([]dirEntry, error)) (int, time.Duration) {
		start := time.Now()
		n := 0
		var rec func(string)
		rec = func(d string) {
			ents, _ := f(d)
			for _, e := range ents {
				n++
				if e.kind == 'd' {
					rec(filepath.Join(d, e.name))
				}
			}
		}
		rec(root)
		return n, time.Since(start)
	}
	a, readDir = walk(listDir)
	b, find = walk(listDirFind)
	return
}
