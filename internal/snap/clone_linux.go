package snap

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// cloneMethod names the copy on write mechanism on this platform.
const cloneMethod = "reflink"

func reflinkFile(src, dst string, fi os.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		out.Close()
		os.Remove(dst)
		return errNoClone
	}
	return out.Close()
}

// cloneTree reflinks every file in src into dst (btrfs, xfs, bcachefs).
// It returns errNoClone before writing anything if reflinks do not work.
func cloneTree(src, dst string) error {
	probe := ""
	_ = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			probe = p
			return filepath.SkipAll
		}
		return nil
	})
	if probe != "" {
		tmp := dst + ".probe"
		fi, err := os.Lstat(probe)
		if err != nil {
			return err
		}
		if err := reflinkFile(probe, tmp, fi); err != nil {
			return errNoClone
		}
		os.Remove(tmp)
	}
	err := copyTree(src, dst, reflinkFile)
	if errors.Is(err, errNoClone) {
		_ = forceRemove(dst)
	}
	return err
}

func statAtime(st *syscall.Stat_t) time.Time {
	return time.Unix(st.Atim.Sec, st.Atim.Nsec)
}

func cloneFile(src, dst string, fi os.FileInfo) error {
	if err := reflinkFile(src, dst, fi); err == nil {
		return nil
	}
	return plainCopy(src, dst, fi)
}
