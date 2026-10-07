package snap

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
)

// On disks where the mirror uses hard links, a program that writes into a
// file in place changes the mirror's link too. Some hooks (the Linux
// preload library, oops watch) copy a file right before its first write and
// leave the copy in the prewrite folder, named by a hash of the path, with
// the original's size and time. A sync uses it when the stamps match.

// PrewriteDir is where pre write copies land.
func PrewriteDir() string { return filepath.Join(Dir(), "prewrite") }

// PrewriteName is the file name a pre write copy of path p gets: FNV 1a of
// the path, as 16 hex digits. The preload library computes the same.
func PrewriteName(p string) string {
	h := fnv.New64a()
	h.Write([]byte(p))
	return fmt.Sprintf("%016x", h.Sum64())
}

// preWriteBackup returns a pre write copy of rel that holds the version
// the index knew, or "".
func (m *Mirror) preWriteBackup(rel string, old node) string {
	p := filepath.Join(PrewriteDir(), PrewriteName(m.origPath(rel)))
	st := StampOf(p)
	if st == nil || *st != old.stamp() {
		return ""
	}
	return p
}

// addBackupEntry moves a pre write copy into the snapshot.
func (m *Mirror) addBackupEntry(s *Snapshot, c node, backup string, k *int) {
	orig := m.origPath(c.rel)
	e := Entry{Orig: orig, Store: filepath.Join("files", fmt.Sprintf("m%d", *k)), Type: "file", Method: "copy", Files: 1, Generic: true, After: &Stamp{Size: c.size, Mtime: c.mtime}}
	target := s.EntryPath(e)
	if err := os.Rename(backup, target); err != nil {
		if _, err := copyTree(backup, target, plainCopy); err != nil {
			s.Skipped = append(s.Skipped, Skip{Path: orig, Reason: "could not keep the pre write copy: " + skipReason(err)})
			return
		}
		_ = os.Remove(backup)
	}
	*k++
	if fi, err := os.Stat(target); err == nil {
		e.Size = fi.Size()
		e.Disk = diskUsage(target)
	}
	s.Entries = append(s.Entries, e)
	s.Size += e.Size
	s.Disk += e.Disk
	s.Files++
}
