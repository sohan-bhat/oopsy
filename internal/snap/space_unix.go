//go:build !windows

package snap

import "golang.org/x/sys/unix"

// diskSpace returns the bytes available to this user and the volume size.
func diskSpace(p string) (int64, int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(p, &st); err != nil {
		return 0, 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), int64(st.Blocks) * int64(st.Bsize), nil
}
