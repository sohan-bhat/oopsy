//go:build !windows

package snap

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// volumeRoots lists the mount points that may hold a volume store: the
// volumes under /Volumes on macOS, and real disks from /proc/mounts on Linux.
func volumeRoots() []string {
	var out []string
	if runtime.GOOS == "darwin" {
		ents, _ := os.ReadDir("/Volumes")
		for _, e := range ents {
			out = append(out, filepath.Join("/Volumes", e.Name()))
		}
		return out
	}
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || !strings.HasPrefix(fields[0], "/dev/") || fields[1] == "/" {
			continue
		}
		mp := strings.ReplaceAll(fields[1], `\040`, " ")
		if _, err := os.Stat(volumeTop(mp)); err == nil {
			out = append(out, mp)
		}
	}
	return out
}
