//go:build !windows

package snap

import (
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// allocBytes is what a file takes on disk.
func allocBytes(_ string, fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int64(st.Blocks) * 512
	}
	return fi.Size()
}

// inodeOf identifies a file so hard links are counted once.
func inodeOf(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

// dataRegions lists where in becomes data, with SEEK_DATA and SEEK_HOLE.
func dataRegions(in *os.File, size int64) ([][2]int64, bool) {
	var out [][2]int64
	fd := int(in.Fd())
	off := int64(0)
	for off < size {
		data, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if err != nil {
			if err == unix.ENXIO {
				break // only a hole is left
			}
			return nil, false
		}
		hole, err := unix.Seek(fd, data, unix.SEEK_HOLE)
		if err != nil {
			return nil, false
		}
		if hole > size {
			hole = size
		}
		out = append(out, [2]int64{data, hole})
		off = hole
	}
	_, _ = in.Seek(0, io.SeekStart)
	return out, true
}

func markSparse(_, _ *os.File) {}
