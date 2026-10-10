//go:build !windows

package snap

import (
	"os"

	"golang.org/x/sys/unix"
)

// listDirStat reads a folder and stats each entry relative to the open
// folder, which saves the kernel walking the full path for every file.
func listDirStat(dir string) ([]dirEntry, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil && len(names) == 0 {
		return nil, err
	}
	fd := int(f.Fd())
	out := make([]dirEntry, 0, len(names))
	var st unix.Stat_t
	for _, name := range names {
		if unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
			continue
		}
		e := dirEntry{name: name, size: st.Size, mode: unixMode(uint32(st.Mode))}
		sec, nsec := statMtime(&st)
		e.mtime = sec*1e9 + nsec
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFREG:
			e.kind = 'f'
		case unix.S_IFDIR:
			e.kind = 'd'
		case unix.S_IFLNK:
			e.kind = 'l'
		}
		out = append(out, e)
	}
	return out, nil
}

// unixMode turns raw st_mode permission bits into the FileMode form the
// rest of Oopsy records.
func unixMode(m uint32) uint32 {
	fm := os.FileMode(m & 0o777)
	if m&unix.S_ISUID != 0 {
		fm |= os.ModeSetuid
	}
	if m&unix.S_ISGID != 0 {
		fm |= os.ModeSetgid
	}
	if m&unix.S_ISVTX != 0 {
		fm |= os.ModeSticky
	}
	return uint32(fm)
}

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
