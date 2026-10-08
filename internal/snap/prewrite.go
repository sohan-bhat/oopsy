package snap

import (
	"fmt"
	"strings"
	"hash/fnv"
	"os"
	"path/filepath"
)

// On disks where the mirror uses hard links, a program that writes into a
// file in place changes the mirror's link too. Some hooks (the Linux
// preload library, oops watch) copy a file right before its first write and
// leave the copy in prewrite/<hash of the root>/<hash of the path>, with the
// original's size and time. A sync uses it when the stamps match.

// PrewriteDir is where pre write copies land.
func PrewriteDir() string { return filepath.Join(Dir(), "prewrite") }

// PrewriteName is the file name a pre write copy of path p gets: FNV 1a of
// the path, as 16 hex digits. The preload library computes the same.
func PrewriteName(p string) string {
	h := fnv.New64a()
	h.Write([]byte(p))
	return fmt.Sprintf("%016x", h.Sum64())
}

// listPrewriteRoot adds a hard link mirror's root to the list the preload
// library reads, so it only copies files Oopsy mirrors.
func listPrewriteRoot(root string) {
	p := filepath.Join(PrewriteDir(), "roots")
	b, _ := os.ReadFile(p)
	for _, line := range strings.Split(string(b), "\n") {
		if line == root {
			return
		}
	}
	if err := os.MkdirAll(PrewriteDir(), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(root + "\n")
}

// prewriteDir is where pre write copies of this mirror's files land.
func (m *Mirror) prewriteDir() string { return filepath.Join(PrewriteDir(), PrewriteName(m.Root)) }

// clearPrewrite drops pre write copies once a sync has used what it needs:
// whatever is left belongs to changes that are already accounted for.
func (m *Mirror) clearPrewrite() { _ = os.RemoveAll(m.prewriteDir()) }

// preWriteBackup returns a pre write copy of rel that holds the version
// the index knew, or "".
func (m *Mirror) preWriteBackup(rel string, old node) string {
	p := filepath.Join(m.prewriteDir(), PrewriteName(m.origPath(rel)))
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
