package snap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Watch mode keeps project mirrors current between commands. Changes made
// while no command is pending (an editor save, an agent's edit tool, a file
// deleted from a file manager or from cmd.exe) become watch snapshots,
// which hold the previous version of every changed and removed file.

// WatchSource is the Source of snapshots watch mode makes.
const WatchSource = "watch"

// DefaultWatchMaxFileMB is the largest file whose previous version watch
// mode keeps when it changes.
const DefaultWatchMaxFileMB = 50

// WatchMaxFile returns the configured per file limit in bytes.
func WatchMaxFile() int64 {
	mb := LoadConfig().WatchMaxFileMB
	if mb <= 0 {
		mb = DefaultWatchMaxFileMB
	}
	return int64(mb) << 20
}

// WatchEditsOnly reports a watch snapshot that only holds modified files.
// Plain oops skips those, so editor saves never get in the way of undoing
// the last command; oops undo <id> restores them.
func (s *Snapshot) WatchEditsOnly() bool {
	if s.Source != WatchSource {
		return false
	}
	for _, e := range s.Entries {
		if e.After == nil {
			return false
		}
	}
	return true
}

// WatchIgnored reports paths whose changes the mirror does not track one by
// one: git's own folder, rebuildable folders and the store.
func WatchIgnored(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	if rel == "." {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if parts[0] == ".git" {
		return true
	}
	for _, part := range parts[:len(parts)-1] {
		if rebuildableNames[part] {
			return true
		}
	}
	return rebuildableNames[parts[len(parts)-1]] || within(p, Dir()) || within(p, Dir())
}

// WatchResult reports one watch sync.
type WatchResult struct {
	Snapshot *Snapshot
	Pending  bool // a command is running; its own after step attributes
	Prepared int  // previous versions put in place from the watch base
	Millis   float64
}

// WatchSync brings root's mirror up to date after a change seen by watch
// mode. When a command is pending, it only puts stored previous versions
// in the pre write folder for the command's after step to pick up.
// Otherwise the changes become a watch snapshot. Changed files bigger than
// maxFile are not kept.
func WatchSync(root string, maxFile int64) (*WatchResult, error) {
	start := time.Now()
	m, err := OpenMirror(root)
	if err != nil {
		return nil, err
	}
	if !m.Exists() {
		return &WatchResult{}, nil
	}
	l, err := m.lock()
	if err != nil {
		return nil, err
	}
	defer l.release()
	old, _ := readIndex(m.indexPath())
	prepared := m.prepPrewrite(old)
	res := &WatchResult{Prepared: len(prepared)}
	if m.pending() != nil {
		res.Pending = true
		res.Millis = float64(time.Since(start).Microseconds()) / 1000
		return res, nil
	}
	attr := &Pending{Command: "watch", Cwd: root, Source: WatchSource, Time: time.Now()}
	sr, err := m.sync(attr, mirrorLimit())
	if err != nil {
		return nil, err
	}
	m.EmptyTrash()
	if sr.Snapshot != nil {
		res.Snapshot = m.finishWatch(sr.Snapshot, prepared, maxFile)
	}
	res.Millis = float64(time.Since(start).Microseconds()) / 1000
	return res, nil
}

// finishWatch trims a fresh watch snapshot: git internals and changed
// files over the limit are dropped, pre write copies made from the watch
// base point at their object instead of a second copy, and the command
// line describes the files. It returns nil when nothing is left.
func (m *Mirror) finishWatch(s *Snapshot, prepared map[string]baseRec, maxFile int64) *Snapshot {
	var keep []Entry
	var skipped []Skip
	drop := func(e Entry) {
		if e.Method != "object" {
			_ = forceRemove(s.EntryPath(e))
		}
	}
	for _, e := range s.Entries {
		switch {
		case within(e.Orig, filepath.Join(m.Root, ".git")):
			drop(e)
			continue
		case e.After != nil && e.Type == "file" && logicalSize(s, e) > maxFile:
			drop(e)
			skipped = append(skipped, Skip{Path: e.Orig, Reason: fmt.Sprintf("bigger than the watch limit of %d MB (oops config watch_max_file_mb)", maxFile>>20)})
			continue
		}
		if b, ok := prepared[e.Orig]; ok && e.Method == "copy" && e.Type == "file" {
			drop(e)
			e.Method, e.Store, e.Disk, e.Base = "object", b.hex(), 0, ""
			e.Mode, e.Mtime = b.mode, b.mtime
		}
		keep = append(keep, e)
	}
	if len(keep) == 0 {
		_ = Remove(s.ID)
		return nil
	}
	s.Entries, s.Skipped, s.Notes = keep, skipped, nil
	s.Size, s.Disk, s.Files = 0, 0, 0
	for _, e := range keep {
		s.Size += e.Size
		s.Disk += e.Disk
		s.Files += e.Files
	}
	s.Command = describeWatch(m.Root, keep)
	s.Cwd = m.Root
	_ = s.save()
	return s
}

// describeWatch names what a watch snapshot holds, such as
// "saved src/app.ts" or "saved 3 files, deleted docs".
func describeWatch(root string, entries []Entry) string {
	var saved, deleted []string
	for _, e := range entries {
		rel, err := filepath.Rel(root, e.Orig)
		if err != nil {
			rel = e.Orig
		}
		rel = filepath.ToSlash(rel)
		if e.After != nil {
			saved = append(saved, rel)
		} else {
			deleted = append(deleted, rel)
		}
	}
	part := func(verb string, names []string) string {
		switch len(names) {
		case 0:
			return ""
		case 1:
			return verb + " " + names[0]
		case 2:
			return verb + " " + names[0] + " and " + names[1]
		}
		return fmt.Sprintf("%s %s and %d more", verb, names[0], len(names)-1)
	}
	var parts []string
	for _, p := range []string{part("saved", saved), part("deleted", deleted)} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ", ")
}

// PruneWatch keeps the number of watch snapshots made only of edits under
// max, deleting the oldest first.
func PruneWatch(max int) int {
	list, err := List()
	if err != nil {
		return 0
	}
	n, removed := 0, 0
	for _, s := range list { // newest first
		if !s.WatchEditsOnly() {
			continue
		}
		n++
		if n > max && Remove(s.ID) == nil {
			removed++
		}
	}
	return removed
}

// logicalSize is a saved file's length, which the watch limit is about.
func logicalSize(s *Snapshot, e Entry) int64 {
	if fi, err := os.Lstat(s.EntryPath(e)); err == nil {
		return fi.Size()
	}
	return e.Size
}
