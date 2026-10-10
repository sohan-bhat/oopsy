//go:build !windows

package snap

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func setMeta(p string, fi os.FileInfo, link bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if ok {
		_ = os.Lchown(p, int(st.Uid), int(st.Gid))
	}
	if !link {
		_ = os.Chmod(p, fi.Mode().Perm()|fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky))
	}
	atime := fi.ModTime()
	if ok {
		atime = statAtime(st)
	}
	ts := []unix.Timespec{unix.NsecToTimespec(atime.UnixNano()), unix.NsecToTimespec(fi.ModTime().UnixNano())}
	_ = unix.UtimesNanoAt(unix.AT_FDCWD, p, ts, unix.AT_SYMLINK_NOFOLLOW)
}

func setMtime(p string, mtime int64) {
	t := time.Unix(0, mtime)
	ts := []unix.Timespec{unix.NsecToTimespec(t.UnixNano()), unix.NsecToTimespec(t.UnixNano())}
	_ = unix.UtimesNanoAt(unix.AT_FDCWD, p, ts, unix.AT_SYMLINK_NOFOLLOW)
}

// sameDevice reports whether two paths live on the same filesystem.
func sameDevice(a, b string) bool {
	var sa, sb unix.Stat_t
	if unix.Lstat(a, &sa) != nil || unix.Stat(b, &sb) != nil {
		return false
	}
	return sa.Dev == sb.Dev
}

// fixDirTimes copies directory timestamps from src to dst. Clones and
// copies create directories fresh, which would otherwise stamp them "now".
func fixDirTimes(src, dst string) {
	_ = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		atime := fi.ModTime()
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			atime = statAtime(st)
		}
		ts := []unix.Timespec{unix.NsecToTimespec(atime.UnixNano()), unix.NsecToTimespec(fi.ModTime().UnixNano())}
		_ = unix.UtimesNanoAt(unix.AT_FDCWD, target, ts, unix.AT_SYMLINK_NOFOLLOW)
		return nil
	})
}

func kindOf(fi os.FileInfo) string {
	switch m := fi.Mode(); {
	case m&os.ModeSymlink != 0:
		return kindSymlink
	case m.IsDir():
		return kindDir
	case m.IsRegular():
		return kindFile
	}
	return kindOther
}

// copyLink recreates the symlink src at dst.
func copyLink(src, dst string, fi os.FileInfo) error {
	target, err := os.Readlink(src)
	if err != nil {
		return err
	}
	return os.Symlink(target, dst)
}

// isLocked reports a file another program holds exclusively. Unix has no
// mandatory locks that stop reading, so this never happens there.
func isLocked(error) bool { return false }

// readOnlyAttr and fragileAttrs only matter on Windows, where deleting a
// read only file clears attributes on every hard link to it.
const readOnlyAttr = 0

func fragileAttrs(os.FileInfo) (uint32, bool) { return 0, false }

func applyAttrs(string, uint32) {}
