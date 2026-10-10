package snap

import (
	"errors"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// cloneMethod names the copy on write mechanism on this platform.
const cloneMethod = "clone"

// cloneTree makes an APFS copy on write clone of src at dst. Directories are
// cloned recursively by the kernel in one call.
func cloneTree(src, dst string) error {
	err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
	if err == nil {
		fixDirTimes(src, dst)
		return nil
	}
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EINVAL) {
		_ = forceRemove(dst)
		return errNoClone
	}
	return err
}

func statAtime(st *syscall.Stat_t) time.Time {
	return time.Unix(st.Atimespec.Sec, st.Atimespec.Nsec)
}

// cloneFile clones a single regular file (used inside copy fallbacks).
func cloneFile(src, dst string, fi os.FileInfo) error {
	if err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW); err == nil {
		return nil
	}
	return plainCopy(src, dst, fi)
}
