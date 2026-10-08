package snap

import (
	"os"
	"path/filepath"
)

// WatchRoots lists the project roots of every mirror, in the main store and
// in the stores at the root of other volumes.
func WatchRoots() []string {
	seen := map[string]bool{}
	var out []string
	add := func(dir string) {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			m := &Mirror{Dir: filepath.Join(dir, e.Name())}
			mm, err := m.meta()
			if err != nil || mm.Root == "" || seen[pathKey(mm.Root)] {
				continue
			}
			if _, err := os.Stat(m.indexPath()); err != nil {
				continue
			}
			seen[pathKey(mm.Root)] = true
			out = append(out, mm.Root)
		}
	}
	add(filepath.Join(Dir(), "mirrors"))
	for _, vr := range volumeRoots() {
		if base := volumeBase(vr); base != "" {
			add(filepath.Join(base, "mirrors"))
		}
	}
	return out
}
