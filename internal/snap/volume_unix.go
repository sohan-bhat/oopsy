//go:build !windows

package snap

import (
	"fmt"
	"os"
	"path/filepath"
)

func volumeRoot(p string) string {
	root := volumeRootByDevice(p)
	if root == "/" {
		return "" // the system volume: no private store at the root of /
	}
	return root
}

// volumeTop is the store folder at a volume's root, one per user.
func volumeTop(root string) string {
	return filepath.Join(root, fmt.Sprintf(".oops-%d", os.Getuid()))
}

func volumeBase(root string) string { return volumeTop(root) }

func ensureVolumeBase(base string) error {
	if err := os.Mkdir(base, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	fi, err := os.Lstat(base)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is not a private folder", base)
	}
	return nil
}
