package snap

import (
	"fmt"
	"path/filepath"
)

// Copies are the only snapshots that cost real disk: clones and hard links
// share the original's data. Instead of a fixed size cap, a copy may use any
// amount of space as long as the disk keeps a safe floor free afterwards.
//
// The floor is 5% of the volume, at least 2 GB and at most 20 GB:
//   - 5% matches the share ext4 keeps back for root by default, and is
//     roughly where macOS and Windows start warning about low space.
//   - 2 GB keeps small disks able to install updates, swap and build.
//   - 20 GB stops the percentage from wasting hundreds of gigabytes on
//     large disks, where 20 GB is plenty of headroom.
const (
	floorPercent = 5
	floorMin     = 2 << 30
	floorMax     = 20 << 30
)

// FreeFloor returns the bytes that must stay free on a volume of the given
// total size. A configured floor wins when set.
func FreeFloor(total int64, cfg Config) int64 {
	if cfg.FreeFloorBytes > 0 {
		return cfg.FreeFloorBytes
	}
	f := total / 100 * floorPercent
	if f < floorMin {
		f = floorMin
	}
	if f > floorMax {
		f = floorMax
	}
	return f
}

// Space describes free space on the volume holding a path.
type Space struct {
	Free  int64
	Total int64
	Floor int64
}

// SpaceAt reports free space for the volume holding p (or its nearest
// existing parent).
func SpaceAt(p string, cfg Config) (Space, error) {
	for {
		free, total, err := diskSpace(p)
		if err == nil {
			return Space{Free: free, Total: total, Floor: FreeFloor(total, cfg)}, nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return Space{}, err
		}
		p = parent
	}
}

// Allows reports whether n more bytes fit above the floor.
func (s Space) Allows(n int64) bool { return s.Free-n >= s.Floor }

// errNoSpace explains a copy that would break the free space floor.
func errNoSpace(size int64, sp Space) error {
	return fmt.Errorf("a %s copy would leave %s free, under the %s floor Oopsy keeps free on this disk (set free_floor_gb with oops config)",
		human(size), human(max(sp.Free-size, 0)), human(sp.Floor))
}
