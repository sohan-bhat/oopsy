package snap

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// A mirror costs nothing while its files match the project, since they share
// the same disk blocks. It starts costing real space once the project moves
// on without it: files changed by programs no hook saw, or the whole project
// deleted. PruneMirrors drops the mirrors that only hold such leftovers, so
// the store cannot grow without bound.
//
// A mirror is dropped when
//   - it has not synced for keep_days. Its project is not in use, or is gone
//     and past the undo window. A live project's mirror is rebuilt the next
//     time a command runs there.
//   - it covers a folder Oopsy no longer mirrors whole, such as a temp, cache
//     or system folder.
//   - the disk is under its free space floor. Mirrors whose folder is gone
//     go first, then the least recently used, until the floor is met.
//
// A mirror that is busy is never touched, nor one with a command pending,
// unless that command is over a day old, its folder is gone, or the folder
// is one Oopsy no longer mirrors. Such a command was cut off before its
// after hook ran, or has nothing left to be attributed to.

// MirrorPruneResult reports what PruneMirrors removed.
type MirrorPruneResult struct {
	Removed int
	Freed   int64 // free space gained on the store's disk
}

type mirrorEntry struct {
	m     *Mirror
	meta  mirrorMeta
	gone  bool
	broad bool // a folder Oopsy no longer mirrors whole
}

// mirrorSweepEvery throttles the sweep that runs after each snapshot.
const mirrorSweepEvery = time.Hour

// MirrorPrune says how far PruneMirrors goes.
type MirrorPrune int

const (
	MirrorsStale MirrorPrune = iota // the rules above
	MirrorsGone                     // also every mirror whose folder is gone, at any age
	MirrorsAll                      // every idle mirror
)

// PruneMirrors removes stale mirrors from the main store.
func PruneMirrors(cfg Config, mode MirrorPrune) MirrorPruneResult {
	var res MirrorPruneResult
	parent := filepath.Join(StoreDir(), "mirrors")
	_ = os.WriteFile(sweepStamp(), nil, 0o600)
	before, spErr := SpaceAt(parent, cfg)
	list := listMirrors(parent)
	cutoff := time.Now().Add(-time.Duration(cfg.KeepDays) * 24 * time.Hour)
	var rest []mirrorEntry
	for _, e := range list {
		stale := lastUse(e.meta).Before(cutoff)
		if mode == MirrorsAll || stale || e.gone && mode == MirrorsGone || e.broad {
			if removeMirror(e) {
				res.Removed++
			}
			continue
		}
		rest = append(rest, e)
	}
	if spErr == nil {
		// Under the floor, give back the oldest mirrors until it is met.
		sort.SliceStable(rest, func(i, j int) bool {
			if rest[i].gone != rest[j].gone {
				return rest[i].gone
			}
			return lastUse(rest[i].meta).Before(lastUse(rest[j].meta))
		})
		for _, e := range rest {
			sp, err := SpaceAt(parent, cfg)
			if err != nil || sp.Free >= sp.Floor {
				break
			}
			if removeMirror(e) {
				res.Removed++
			}
		}
		if after, err := SpaceAt(parent, cfg); err == nil && after.Free > before.Free {
			res.Freed = after.Free - before.Free
		}
	}
	return res
}

// MirrorSweepDue reports whether the mirror sweep should run again: an hour
// after the last one, or right away when the disk is under its floor. Hooks
// check it after each command and run the sweep in the background.
func MirrorSweepDue(cfg Config) bool {
	fi, err := os.Stat(sweepStamp())
	if err != nil {
		// A store that never swept starts its clock now.
		if _, err := os.Stat(filepath.Join(StoreDir(), "mirrors")); err == nil {
			_ = os.WriteFile(sweepStamp(), nil, 0o600)
		}
		return false
	}
	if time.Since(fi.ModTime()) >= mirrorSweepEvery {
		return true
	}
	sp, err := SpaceAt(StoreDir(), cfg)
	return err == nil && sp.Free < sp.Floor && time.Since(fi.ModTime()) >= time.Minute
}

func sweepStamp() string { return filepath.Join(StoreDir(), "mirrors", ".swept") }

func listMirrors(parent string) []mirrorEntry {
	ents, _ := os.ReadDir(parent)
	var out []mirrorEntry
	for _, d := range ents {
		if !d.IsDir() {
			continue
		}
		if strings.HasSuffix(d.Name(), ".pruning") {
			// Left behind by a sweep that was interrupted.
			_ = forceRemove(filepath.Join(parent, d.Name()))
			continue
		}
		m := &Mirror{Dir: filepath.Join(parent, d.Name())}
		mm, err := m.meta()
		if err != nil || mm.Root == "" {
			continue
		}
		m.Root = mm.Root
		_, statErr := os.Lstat(mm.Root)
		out = append(out, mirrorEntry{m: m, meta: mm, gone: os.IsNotExist(statErr), broad: tooBroad(mm.Root)})
	}
	return out
}

func lastUse(mm mirrorMeta) time.Time {
	if mm.Synced.After(mm.Created) {
		return mm.Synced
	}
	return mm.Created
}

// abandonAfter is how old a pending command is before it no longer keeps
// its mirror.
const abandonAfter = 24 * time.Hour

// removeMirror deletes an idle mirror and reports whether it did.
func removeMirror(e mirrorEntry) bool {
	m := e.m
	l := tryLockAt(filepath.Join(m.Dir, "lock"))
	if l == nil {
		return false
	}
	if p := m.pending(); p != nil && !e.gone && !e.broad && time.Since(p.Time) < abandonAfter {
		l.release()
		return false
	}
	// Move it aside first, so a hook that opens this mirror while the files
	// are being deleted starts a fresh one instead of reading a half deleted
	// copy. Windows cannot rename a folder with an open file in it, so the
	// lock is let go first there.
	gone := m.Dir + ".pruning"
	if runtime.GOOS == "windows" {
		l.release()
	}
	err := os.Rename(m.Dir, gone)
	if runtime.GOOS != "windows" {
		l.release()
	}
	if err != nil {
		return false
	}
	return forceRemove(gone) == nil
}
