//go:build windows

package snap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// longPath returns p in the \\?\ form so Win32 calls accept paths longer
// than MAX_PATH. The os package does this itself; raw syscalls need it.
func longPath(p string) string {
	if strings.HasPrefix(p, `\\?\`) || !filepath.IsAbs(p) {
		return p
	}
	p = filepath.Clean(p)
	if strings.HasPrefix(p, `\\`) {
		return `\\?\UNC\` + p[2:]
	}
	return `\\?\` + p
}

func utf16Path(p string) (*uint16, error) { return windows.UTF16PtrFromString(longPath(p)) }

// openNoFollow opens p (file, directory or link itself) for metadata work.
func openNoFollow(p string, access uint32, follow bool) (windows.Handle, error) {
	u, err := utf16Path(p)
	if err != nil {
		return windows.InvalidHandle, err
	}
	flags := uint32(windows.FILE_FLAG_BACKUP_SEMANTICS)
	if !follow {
		flags |= windows.FILE_FLAG_OPEN_REPARSE_POINT
	}
	return windows.CreateFile(u, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, flags, 0)
}

// keptAttrs are the attributes a snapshot carries over. Compression, sparse
// and reparse bits belong to how the data is stored, not to the file.
const keptAttrs = windows.FILE_ATTRIBUTE_READONLY | windows.FILE_ATTRIBUTE_HIDDEN | windows.FILE_ATTRIBUTE_SYSTEM |
	windows.FILE_ATTRIBUTE_ARCHIVE | windows.FILE_ATTRIBUTE_NOT_CONTENT_INDEXED

func winData(fi os.FileInfo) *syscall.Win32FileAttributeData {
	d, _ := fi.Sys().(*syscall.Win32FileAttributeData)
	return d
}

// setMeta copies creation, access and write times and the file attributes.
// Owners and ACLs are left to inheritance from the destination folder.
func setMeta(p string, fi os.FileInfo, link bool) {
	d := winData(fi)
	if d == nil {
		_ = os.Chtimes(p, fi.ModTime(), fi.ModTime())
		return
	}
	ct := windows.Filetime(d.CreationTime)
	at := windows.Filetime(d.LastAccessTime)
	wt := windows.Filetime(d.LastWriteTime)
	setTimes(p, &ct, &at, &wt)
	setAttrs(p, d.FileAttributes)
}

func setTimes(p string, ct, at, wt *windows.Filetime) {
	h, err := openNoFollow(p, windows.FILE_WRITE_ATTRIBUTES, false)
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	_ = windows.SetFileTime(h, ct, at, wt)
}

// setAttrs applies the kept attribute bits of want to p.
func setAttrs(p string, want uint32) {
	u, err := utf16Path(p)
	if err != nil {
		return
	}
	cur, err := windows.GetFileAttributes(u)
	if err != nil || cur&keptAttrs == want&keptAttrs {
		return
	}
	v := want & keptAttrs
	if v == 0 {
		v = windows.FILE_ATTRIBUTE_NORMAL
	}
	_ = windows.SetFileAttributes(u, v)
}

func setMtime(p string, mtime int64) {
	wt := windows.NsecToFiletime(mtime)
	setTimes(p, nil, nil, &wt)
}

// volumeSerial identifies the volume p lives on.
func volumeSerial(p string, follow bool) (uint32, bool) {
	h, err := openNoFollow(p, 0, follow)
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return 0, false
	}
	return info.VolumeSerialNumber, true
}

// sameDevice reports whether two paths live on the same volume.
func sameDevice(a, b string) bool {
	va, ok1 := volumeSerial(a, false)
	vb, ok2 := volumeSerial(b, true)
	return ok1 && ok2 && va == vb
}

func kindOf(fi os.FileInfo) string {
	m := fi.Mode()
	if m&os.ModeSymlink != 0 {
		return kindSymlink
	}
	d := winData(fi)
	if d != nil && d.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 && !m.IsDir() &&
		d.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		// Go reports junctions (mount points) as irregular, not as
		// directories, so they are never walked into.
		return kindJunction
	}
	switch {
	case m.IsDir():
		return kindDir
	case m.IsRegular():
		return kindFile
	case m&os.ModeIrregular != 0 && m&(os.ModeDevice|os.ModeNamedPipe|os.ModeSocket) == 0:
		// Other reparse points, such as OneDrive placeholders, hold file data.
		return kindFile
	}
	return kindOther
}

// copyLink recreates the symlink or junction src at dst, pointing at the
// same target. Directory symlinks stay directory symlinks.
func copyLink(src, dst string, fi os.FileInfo) error {
	target, err := os.Readlink(src)
	if err != nil {
		return err
	}
	if kindOf(fi) == kindJunction {
		return createJunction(dst, target)
	}
	return makeSymlink(dst, target, winData(fi) != nil && winData(fi).FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0)
}

// symlinkAllowUnprivileged is SYMBOLIC_LINK_FLAG_ALLOW_UNPRIVILEGED_CREATE,
// which works when Developer Mode is on.
const symlinkAllowUnprivileged = 0x2

func makeSymlink(dst, target string, dir bool) error {
	u, err := utf16Path(dst)
	if err != nil {
		return err
	}
	t, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	flags := uint32(symlinkAllowUnprivileged)
	if dir {
		flags |= windows.SYMBOLIC_LINK_FLAG_DIRECTORY
	}
	if err := windows.CreateSymbolicLink(u, t, flags); err != nil {
		if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
			return errors.New("creating symlinks on Windows needs Developer Mode or an administrator")
		}
		return &os.LinkError{Op: "symlink", Old: target, New: dst, Err: err}
	}
	return nil
}

// createJunction makes dst a directory junction to target. Junctions need no
// special privilege, unlike symlinks.
func createJunction(dst, target string) error {
	abs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dst, 0o700); err != nil {
		return err
	}
	sub, _ := windows.UTF16FromString(`\??\` + abs)
	print, _ := windows.UTF16FromString(abs)
	subBytes := (len(sub) - 1) * 2
	printBytes := (len(print) - 1) * 2
	// REPARSE_DATA_BUFFER header (8 bytes) + mount point header (8 bytes)
	// + both names with their terminating NULs.
	pathBytes := subBytes + 2 + printBytes + 2
	buf := make([]byte, 16+pathBytes)
	le := func(off int, v uint32, size int) {
		for i := 0; i < size; i++ {
			buf[off+i] = byte(v >> (8 * i))
		}
	}
	le(0, windows.IO_REPARSE_TAG_MOUNT_POINT, 4)
	le(4, uint32(8+pathBytes), 2)
	le(8, 0, 2)                   // SubstituteNameOffset
	le(10, uint32(subBytes), 2)   // SubstituteNameLength
	le(12, uint32(subBytes+2), 2) // PrintNameOffset
	le(14, uint32(printBytes), 2) // PrintNameLength
	copy(buf[16:], unsafe.Slice((*byte)(unsafe.Pointer(&sub[0])), len(sub)*2))
	copy(buf[16+subBytes+2:], unsafe.Slice((*byte)(unsafe.Pointer(&print[0])), len(print)*2))

	h, err := openNoFollow(dst, windows.GENERIC_WRITE, false)
	if err != nil {
		os.Remove(dst)
		return err
	}
	var ret uint32
	err = windows.DeviceIoControl(h, windows.FSCTL_SET_REPARSE_POINT, &buf[0], uint32(len(buf)), nil, 0, &ret, nil)
	windows.CloseHandle(h)
	if err != nil {
		os.Remove(dst)
		return &os.LinkError{Op: "junction", Old: target, New: dst, Err: err}
	}
	return nil
}

// isLocked reports a file another program holds open without sharing.
func isLocked(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

const readOnlyAttr = windows.FILE_ATTRIBUTE_READONLY

// fragileAttrs returns the attributes worth recording for a hard linked file:
// Remove-Item -Force and del /f clear read only, hidden and system before
// deleting, and that change shows through the snapshot's link too.
func fragileAttrs(fi os.FileInfo) (uint32, bool) {
	d := winData(fi)
	if d == nil {
		return 0, false
	}
	const fragile = windows.FILE_ATTRIBUTE_READONLY | windows.FILE_ATTRIBUTE_HIDDEN | windows.FILE_ATTRIBUTE_SYSTEM
	if d.FileAttributes&fragile == 0 {
		return 0, false
	}
	return d.FileAttributes & keptAttrs, true
}

func applyAttrs(p string, a uint32) { setAttrs(p, a) }
