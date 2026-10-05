package snap

import (
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// cloneMethod names the copy on write mechanism on this platform.
const cloneMethod = "clone"

// cloneTree makes an APFS copy on write clone of src at dst. Directories are
// cloned recursively by the kernel in one call.
func cloneTree(src, dst string) ([]Skip, error) {
	if forceCopy {
		return nil, errNoClone
	}
	err := unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
	if err == nil {
		fixDirTimes(src, dst)
		return nil, nil
	}
	// Anything else (such as an unreadable file inside the tree) falls back
	// to the copy path, which saves what it can and reports the rest.
	_ = forceRemove(dst)
	return nil, errNoClone
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
