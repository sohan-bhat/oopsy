//go:build windows

package snap

import (
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// entryInfo uses the data the directory listing already returned, so a
// walk costs one call per folder instead of one per file.
func entryInfo(full string, e fs.DirEntry) (os.FileInfo, error) {
	fi, err := e.Info()
	if err != nil {
		return nil, err
	}
	if fi.Mode()&fs.ModeIrregular != 0 {
		return os.Lstat(full)
	}
	return fi, nil
}

// nodeMode records the file attributes Oopsy carries over on Windows.
func nodeMode(fi os.FileInfo) uint32 {
	if d := winData(fi); d != nil {
		return d.FileAttributes & keptAttrs
	}
	return 0
}

// fragileMode reports attributes del /f and Remove-Item -Force clear
// through a hard link before deleting.
func fragileMode(m uint32) bool {
	return m&(windows.FILE_ATTRIBUTE_READONLY|windows.FILE_ATTRIBUTE_HIDDEN|windows.FILE_ATTRIBUTE_SYSTEM) != 0
}

// applyDirMode puts a folder's recorded attributes back.
func applyDirMode(p string, m uint32) { setAttrs(p, m) }
