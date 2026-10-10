package snap

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// forceCopy disables copy on write clones (used to measure the fallback).
var forceCopy = os.Getenv("OOPS_FORCE_COPY") != ""

// errNoClone means the filesystem cannot make copy on write clones here.
var errNoClone = errors.New("copy on write clones are not supported here")

// treeSize walks p and returns the apparent size and the number of entries.
func treeSize(p string) (int64, int, error) {
	var size int64
	var n int
	err := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == p {
				return err
			}
			return nil
		}
		n++
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				size += fi.Size()
			}
		}
		return nil
	})
	return size, n, err
}

// copyTree copies src to dst keeping modes, owners (when allowed), symlinks
// and timestamps. dst must not exist.
func copyTree(src, dst string, fileCopy func(src, dst string, fi os.FileInfo) error) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
		setMeta(dst, fi, true)
		return nil
	case fi.IsDir():
		if err := os.Mkdir(dst, 0o700); err != nil {
			return err
		}
		ents, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		var firstErr error
		for _, e := range ents {
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), fileCopy); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		setMeta(dst, fi, false)
		return firstErr
	case fi.Mode().IsRegular():
		if err := fileCopy(src, dst, fi); err != nil {
			return err
		}
		setMeta(dst, fi, false)
		return nil
	default:
		// Sockets, fifos and devices are skipped: there is no data to save.
		return nil
	}
}

func plainCopy(src, dst string, fi os.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// SameDevice reports whether two paths are on the same filesystem.
func SameDevice(a, b string) bool { return sameDevice(a, b) }

// ProbeClone reports which snapshot method works in dir: clone, reflink or copy.
func ProbeClone(dir string) string {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "copy"
	}
	src, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return "copy"
	}
	src.WriteString("oopsy")
	src.Close()
	defer os.Remove(src.Name())
	dst := src.Name() + ".clone"
	defer os.Remove(dst)
	if err := cloneTree(src.Name(), dst); err != nil {
		return "copy"
	}
	return cloneMethod
}
