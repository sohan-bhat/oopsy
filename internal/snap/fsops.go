package snap

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// method picks snapshot methods for tests and benchmarks: OOPS_METHOD=copy
// skips clones and hard links, OOPS_METHOD=hardlink skips clones.
// OOPS_FORCE_COPY=1 is the older spelling of OOPS_METHOD=copy.
var method = func() string {
	if os.Getenv("OOPS_FORCE_COPY") != "" {
		return "copy"
	}
	return os.Getenv("OOPS_METHOD")
}()

// forceCopy disables copy on write clones.
var forceCopy = method == "copy" || method == "hardlink"

// errNoClone means the filesystem cannot make copy on write clones here.
var errNoClone = errors.New("copy on write clones are not supported here")

// Entry kinds returned by kindOf.
const (
	kindFile     = "file"
	kindDir      = "dir"
	kindSymlink  = "symlink"
	kindJunction = "junction" // Windows directory junction (mount point)
	kindOther    = "other"    // sockets, fifos, devices: no data to save
)

// treeSize walks p and returns the apparent size and the number of entries.
// Links and junctions are counted but never followed.
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
		if d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil && kindOf(fi) == kindFile {
			size += fi.Size()
		}
		return nil
	})
	return size, n, err
}

// treeCopy copies a tree and keeps going past files it cannot read, such as
// files another program holds open without sharing. Those are recorded in
// skipped instead of failing the whole snapshot.
type treeCopy struct {
	file         func(src, dst string, fi os.FileInfo) error
	skipFileMeta bool // file sets metadata itself (hard links share it)
	skipped      []Skip
}

// copyTree copies src to dst keeping modes, owners (when allowed), links and
// timestamps. dst must not exist. The error is non nil only when nothing
// usable was written (or errNoClone, so the caller can pick another method).
func copyTree(src, dst string, fileCopy func(src, dst string, fi os.FileInfo) error) ([]Skip, error) {
	c := &treeCopy{file: fileCopy}
	err := c.walk(src, dst, true)
	return c.skipped, err
}

func (c *treeCopy) skip(p string, err error) {
	c.skipped = append(c.skipped, Skip{Path: p, Reason: skipReason(err)})
}

func (c *treeCopy) walk(src, dst string, top bool) error {
	fi, err := os.Lstat(src)
	if err != nil {
		if top {
			return err
		}
		c.skip(src, err)
		return nil
	}
	switch kindOf(fi) {
	case kindSymlink, kindJunction:
		if err := copyLink(src, dst, fi); err != nil {
			if top {
				return err
			}
			c.skip(src, err)
			return nil
		}
		setMeta(dst, fi, true)
	case kindDir:
		if err := os.Mkdir(dst, 0o700); err != nil {
			return err
		}
		ents, err := os.ReadDir(src)
		if err != nil {
			c.skip(src, err)
		}
		for _, e := range ents {
			if err := c.walk(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), false); err != nil {
				return err
			}
		}
		setMeta(dst, fi, false)
	case kindFile:
		if err := c.file(src, dst, fi); err != nil {
			_ = os.Remove(dst)
			if top || errors.Is(err, errNoClone) {
				return err
			}
			c.skip(src, err)
			return nil
		}
		if !c.skipFileMeta {
			setMeta(dst, fi, false)
		}
	}
	return nil
}

// skipReason turns a file error into a short explanation.
func skipReason(err error) string {
	switch {
	case isLocked(err):
		return "in use by another program, so it could not be read"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// skipError folds skipped paths into one error for callers that only report
// success or failure, such as restores.
func skipError(skipped []Skip) error {
	switch len(skipped) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("%s: %s", skipped[0].Path, skipped[0].Reason)
	}
	return fmt.Errorf("%s: %s (and %d more)", skipped[0].Path, skipped[0].Reason, len(skipped)-1)
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

// ProbeClone reports which copy on write method works in dir (clone,
// reflink or block clone), or copy when there is none.
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
	if _, err := cloneTree(src.Name(), dst); err != nil {
		return "copy"
	}
	return cloneMethod
}
