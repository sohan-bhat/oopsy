package snap

import (
	"os"
	"path/filepath"
	"strconv"
)

// Hard links and clones only work within one volume. When a path lives on a
// different volume than ~/.oops (a D: drive on Windows, a second disk on
// Linux, an external APFS drive), its snapshot goes into a small private
// store at that volume's root instead: <root>\.oops\<user SID> on Windows,
// <root>/.oops-<uid> elsewhere. Copies always go to the main store.

// volumeDir returns this snapshot's directory in the store on p's volume,
// creating it, or "" when there is no usable one.
func (s *Snapshot) volumeDir(p string) string {
	root := volumeRoot(p)
	if root == "" {
		return ""
	}
	if d, ok := s.vols[root]; ok {
		return d
	}
	if s.vols == nil {
		s.vols = map[string]string{}
	}
	d := ""
	if base := volumeBase(root); base != "" && ensureVolumeBase(base) == nil {
		d = filepath.Join(base, "snapshots", strconv.Itoa(s.ID))
		if err := os.MkdirAll(filepath.Join(d, "files"), 0o700); err != nil {
			d = ""
		}
	}
	s.vols[root] = d
	return d
}

// dropEmptyVolumeDirs removes volume store directories no entry ended up
// using.
func (s *Snapshot) dropEmptyVolumeDirs() {
	used := map[string]bool{}
	for _, e := range s.Entries {
		used[e.Base] = true
	}
	for _, d := range s.vols {
		if d != "" && !used[d] {
			_ = os.Remove(filepath.Join(d, "files"))
			_ = os.Remove(d)
		}
	}
}

// inVolumeStore reports whether p is inside a volume store.
func inVolumeStore(p string) bool {
	root := volumeRoot(p)
	if root == "" {
		return false
	}
	top := volumeTop(root)
	return top != "" && within(p, top)
}

// volumeRootByDevice finds the mount point holding p by walking up until the
// device changes.
func volumeRootByDevice(p string) string {
	cur := filepath.Clean(p)
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return cur
		}
		if !sameDevice(cur, parent) {
			return cur
		}
		cur = parent
	}
}
