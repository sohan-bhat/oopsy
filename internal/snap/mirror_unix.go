//go:build !windows

package snap

import (
	"io/fs"
	"os"
)

// entryInfo stats a directory entry without following links.
func entryInfo(full string, _ fs.DirEntry) (os.FileInfo, error) { return os.Lstat(full) }

// nodeMode is what the mirror records about a file beyond size and time:
// the permission bits.
func nodeMode(fi os.FileInfo) uint32 {
	return uint32(fi.Mode().Perm() | fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky))
}

// fragileMode reports a recorded mode that a delete may clear through a
// hard link (rm -f and friends make read only files writable first).
func fragileMode(m uint32) bool { return m&0o200 == 0 }

// applyDirMode puts a folder's recorded permissions back.
func applyDirMode(p string, m uint32) { _ = os.Chmod(p, os.FileMode(m)) }
