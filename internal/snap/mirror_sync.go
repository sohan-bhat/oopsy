package snap

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultMirrorMaxFiles caps how big a project the mirror covers. Past it,
// the stat walk before each command would get slow, so only the command
// line layer protects the project.
const DefaultMirrorMaxFiles = 50000

// sync walks the project, attributes what changed since the last sync to
// attr (when there is a pending command), and brings the mirror up to date.
// The caller holds the mirror lock.
func (m *Mirror) sync(attr *Pending, limit int) (*SyncResult, error) {
	start := time.Now()
	usn, haveUSN := m.usnStart()
	if haveUSN && m.Exists() && m.usnClean() {
		// The change journal says nothing in the project changed.
		m.usnAdvance(usn)
		Trace("change journal: nothing changed")
		mm, _ := m.meta()
		return &SyncResult{Files: mm.Files, Millis: float64(time.Since(start).Microseconds()) / 1000}, nil
	}
	old, _ := readIndex(m.indexPath())
	Trace("read index")
	w, err := walkProject(m.Root, limit)
	if err != nil {
		return nil, err
	}
	Trace("walk")
	res := &SyncResult{Files: w.files}
	cur := w.nodes

	// Merge the two sorted lists.
	var removed, changed, added []node
	i, j := 0, 0
	for i < len(old) || j < len(cur) {
		switch {
		case j >= len(cur) || (i < len(old) && old[i].rel < cur[j].rel):
			removed = append(removed, old[i])
			i++
		case i >= len(old) || cur[j].rel < old[i].rel:
			added = append(added, cur[j])
			j++
		default:
			o, c := old[i], cur[j]
			i++
			j++
			switch {
			case o.kind != c.kind:
				removed = append(removed, o)
				added = append(added, c)
			case c.kind == 'f' && (o.size != c.size || o.mtime != c.mtime):
				changed = append(changed, c)
				oldStamps[c.rel] = o
			case c.kind == 'l' && o.mtime != c.mtime:
				removed = append(removed, o)
				added = append(added, c)
			case c.kind == 'r' && o.mtime != c.mtime && attr != nil:
				res.Notes = append(res.Notes, rebuildNote(m.origPath(c.rel), "changed"))
			}
		}
	}
	res.Changed = len(changed)
	res.Removed = len(removed)
	res.Added = len(added)

	var snap *Snapshot
	if attr != nil && (len(removed) > 0 || len(changed) > 0 || len(res.Notes) > 0) {
		snap, err = m.attribute(attr, old, removed, changed, res)
		if err != nil {
			return nil, err
		}
	} else {
		for _, n := range minimalNodes(removed) {
			if p := m.dataPath(n.rel); exists(p) {
				m.trash(p)
			}
		}
	}
	for _, n := range changed {
		m.put(n)
	}
	m.addAll(added)
	Trace("diff and update")
	m.clearPrewrite()
	if len(removed) > 0 || len(changed) > 0 || len(added) > 0 || indexChanged(old, cur) {
		if err := writeIndex(m.indexPath(), cur); err != nil {
			return nil, err
		}
	}
	Trace("write index")
	mm, _ := m.meta()
	mm.Root, mm.Method, mm.Files, mm.Synced = m.Root, m.Method, w.files, time.Now()
	if mm.Created.IsZero() {
		mm.Created = mm.Synced
	}
	_ = m.saveMeta(mm)
	for k := range oldStamps {
		delete(oldStamps, k)
	}
	if haveUSN {
		m.usnSave(usn)
	}
	res.Snapshot = snap
	res.Millis = float64(time.Since(start).Microseconds()) / 1000
	return res, nil
}

// indexChanged reports differences the diff does not count, such as a
// folder's time or a file's mode.
func indexChanged(old, cur []node) bool {
	if len(old) != len(cur) {
		return true
	}
	for i := range old {
		if old[i] != cur[i] {
			return true
		}
	}
	return false
}

// Trace reports progress through a sync when hooks are tracing.
var Trace = func(string) {}

// oldStamps carries the pre change node of each changed file from the merge
// to attribute. Syncs never run concurrently in one process.
var oldStamps = map[string]node{}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func rebuildNote(p, what string) string {
	return fmt.Sprintf("%s was %s. Oopsy does not keep rebuildable folders unless the command names them, so rebuild it if you need it.", p, what)
}

// parentMtime returns the time the folder holding rel had before the
// command, from the old index (or the root's current time at the top).
func parentMtime(old []node, rel, root string) int64 {
	p := relParent(rel)
	if p == "" {
		if fi, err := os.Stat(root); err == nil {
			return fi.ModTime().UnixNano()
		}
		return 0
	}
	lo, hi := 0, len(old)
	for lo < hi {
		mid := (lo + hi) / 2
		if old[mid].rel < p {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(old) && old[lo].rel == p {
		return old[lo].mtime
	}
	return 0
}

// minimalNodes drops nodes inside other nodes of the (sorted) list.
func minimalNodes(nodes []node) []node {
	var out []node
	for _, n := range nodes {
		if len(out) > 0 {
			last := out[len(out)-1].rel
			if len(n.rel) > len(last) && n.rel[:len(last)] == last && n.rel[len(last)] == '/' {
				continue
			}
		}
		out = append(out, n)
	}
	return out
}

// addAll mirrors new entries. On APFS a new folder is cloned whole in one
// call when nothing inside it is excluded.
func (m *Mirror) addAll(added []node) {
	excluded := map[string]bool{}
	for _, n := range added {
		if n.kind == 'r' {
			for p := relParent(n.rel); p != ""; p = relParent(p) {
				excluded[p] = true
			}
		}
	}
	store := Dir()
	skipUnder := ""
	for _, n := range added {
		if skipUnder != "" && len(n.rel) > len(skipUnder) && n.rel[:len(skipUnder)] == skipUnder && n.rel[len(skipUnder)] == '/' {
			continue
		}
		skipUnder = ""
		if m.Method == "clone" && n.kind == 'd' && !excluded[n.rel] && !within(store, m.origPath(n.rel)) {
			dst := m.dataPath(n.rel)
			if !exists(dst) && cloneDir(m.origPath(n.rel), dst) {
				skipUnder = n.rel
				continue
			}
		}
		m.put(n)
	}
}

// attribute moves the mirror's copies of everything the pending command
// removed or changed into its snapshot.
func (m *Mirror) attribute(p *Pending, old, removed, changed []node, res *SyncResult) (*Snapshot, error) {
	l, err := acquire()
	if err != nil {
		return nil, err
	}
	defer l.release()
	var s *Snapshot
	if p.SnapID > 0 {
		s, _ = Load(p.SnapID)
	}
	fresh := s == nil
	if fresh {
		id, err := nextID()
		if err != nil {
			return nil, err
		}
		s = &Snapshot{ID: id, Time: p.Time, Command: p.Command, Cwd: p.Cwd, Source: p.Source}
	}
	if err := os.MkdirAll(filepath.Join(s.Dir(), "files"), 0o700); err != nil {
		return nil, err
	}
	covered := func(path string) bool {
		for _, e := range s.Entries {
			if !e.Generic && within(path, e.Orig) {
				return true
			}
		}
		return false
	}
	dest := func(k int) (string, string) {
		store := filepath.Join("files", fmt.Sprintf("m%d", k))
		if m.inVol {
			base := s.volumeDir(m.Root)
			return store, base
		}
		return store, ""
	}
	k := len(s.Entries)
	addEntry := func(n node, after *Stamp) {
		orig := m.origPath(n.rel)
		data := m.dataPath(n.rel)
		if covered(orig) {
			if exists(data) {
				m.trash(data)
			}
			return
		}
		if !exists(data) {
			return
		}
		store, base := dest(k)
		e := Entry{Orig: orig, Store: store, Base: base, Method: m.Method, After: after, Generic: true}
		target := s.EntryPath(e)
		_ = os.MkdirAll(filepath.Dir(target), 0o700)
		if err := os.Rename(data, target); err != nil {
			s.Skipped = append(s.Skipped, Skip{Path: orig, Reason: "could not move the saved copy: " + skipReason(err)})
			return
		}
		k++
		switch n.kind {
		case 'd':
			e.Type = "dir"
			e.Files = 1
			inside := under(old, n.rel)
			for _, c := range inside {
				e.Files++
				rel := c.rel[len(n.rel)+1:]
				switch {
				case c.kind == 'd':
					applyDirMode(filepath.Join(target, filepath.FromSlash(rel)), c.mode)
					setDirCreated(filepath.Join(target, filepath.FromSlash(rel)), c.size)
				case c.kind == 'f' && m.Method == "hardlink" && fragileMode(c.mode):
					if e.Attrs == nil {
						e.Attrs = map[string]uint32{}
					}
					e.Attrs[rel] = c.mode
				}
			}
			// Folder times last, since filling a folder changes its time.
			for x := len(inside) - 1; x >= 0; x-- {
				if c := inside[x]; c.kind == 'd' {
					setMtime(filepath.Join(target, filepath.FromSlash(c.rel[len(n.rel)+1:])), c.mtime)
				}
			}
			applyDirMode(target, n.mode)
			setDirCreated(target, n.size)
			setMtime(target, n.mtime)
		case 'l':
			e.Type = "symlink"
			e.Files = 1
		default:
			e.Type = "file"
			e.Files = 1
			if m.Method == "hardlink" && fragileMode(n.mode) {
				e.Attrs = map[string]uint32{".": n.mode}
			}
		}
		e.ParentMtime = parentMtime(old, n.rel, m.Root)
		e.Size, _, _ = treeSize(target) // what the saved data takes on disk
		s.Entries = append(s.Entries, e)
		s.Size += e.Size
		s.Files += e.Files
	}
	for _, n := range minimalNodes(removed) {
		if n.kind == 'r' {
			if !covered(m.origPath(n.rel)) {
				res.Notes = append(res.Notes, rebuildNote(m.origPath(n.rel), "removed"))
			}
			continue
		}
		addEntry(n, nil)
	}
	for _, c := range changed {
		o := oldStamps[c.rel]
		if m.Method == "hardlink" {
			// A hard link shares the file. If the mirror's link shows the
			// new content, the command wrote into the file in place and the
			// old version is gone; if it still shows the old stamp, the
			// command replaced the file and the link kept the old one.
			if st := StampOf(m.dataPath(c.rel)); st == nil || *st != o.stamp() {
				if bk := m.preWriteBackup(c.rel, o); bk != "" {
					m.addBackupEntry(s, c, o, parentMtime(old, c.rel, m.Root), bk, &k)
					continue
				}
				if !covered(m.origPath(c.rel)) {
					s.Skipped = append(s.Skipped, Skip{Path: m.origPath(c.rel),
						Reason: "changed in place by a program Oopsy could not see; on this disk the mirror uses hard links, which share the file, so the old version is gone (oops watch keeps versions)"})
				}
				continue
			}
		}
		after := c.stamp()
		addEntry(o, &after)
	}
	s.Notes = append(s.Notes, res.Notes...)
	if fresh && len(s.Entries) == 0 && len(s.Skipped) == 0 && len(s.Notes) == 0 {
		_ = forceRemove(s.Dir())
		return nil, nil
	}
	s.dropEmptyVolumeDirs()
	if fresh {
		s.Millis = res.Millis
		appendJournal(s)
	}
	return s, s.save()
}
