//go:build !windows

package snap

import (
	"io/fs"
	"path/filepath"
	"syscall"
)

// compressDir is Windows only; Unix filesystems with compression (btrfs,
// APFS) also have clones, which are cheaper still.
func compressDir(string) bool { return false }

func isCompressed(string) bool { return false }

// diskUsage adds up the blocks p's files occupy, counting each inode once.
func diskUsage(p string) int64 {
	var total int64
	seen := map[uint64]bool{}
	_ = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			if !seen[uint64(st.Ino)] {
				seen[uint64(st.Ino)] = true
				total += int64(st.Blocks) * 512
			}
		}
		return nil
	})
	return total
}
