//go:build windows

package snap

import (
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// listDir uses the data the directory listing already returned, so a walk
// costs one call per folder instead of one per file.
func listDir(dir string) ([]dirEntry, error) {
	ents, err := os.ReadDir(dir)
	if err != nil && len(ents) == 0 {
		return nil, err
	}
	out := make([]dirEntry, 0, len(ents))
	for _, d := range ents {
		fi, err := d.Info()
		if err != nil {
			continue
		}
		if fi.Mode()&fs.ModeIrregular != 0 {
			if fi, err = os.Lstat(filepath.Join(dir, d.Name())); err != nil {
				continue
			}
		}
		e := dirEntry{name: d.Name(), size: fi.Size(), mtime: fi.ModTime().UnixNano(), mode: nodeMode(fi)}
		switch kindOf(fi) {
		case kindFile:
			e.kind = 'f'
		case kindDir:
			e.kind = 'd'
			// Folders have no size; keep their creation time instead, so a
			// folder Oopsy brings back has the one it had.
			if d := winData(fi); d != nil {
				e.size = d.CreationTime.Nanoseconds()
			}
		case kindSymlink, kindJunction:
			e.kind = 'l'
		}
		out = append(out, e)
	}
	return out, nil
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

// setDirCreated puts back a folder's creation time recorded by listDir.
func setDirCreated(p string, created int64) {
	if created == 0 {
		return
	}
	ct := windows.NsecToFiletime(created)
	setTimes(p, &ct, nil, nil)
}
