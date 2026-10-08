package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/sohan-bhat/oopsy/internal/snap"
	"github.com/sohan-bhat/oopsy/internal/ui"
)

// Cursor has no hook before a file edit, only afterFileEdit, which names the
// file and the replacements made. The version before the edit is recovered
// after the fact, from the project mirror when it still holds exactly that
// version, else by undoing the replacements on the current text.

func cursorAfterEdit(in editInput, cwd, session string, stderr io.Writer) {
	path := in.FilePath
	if path == "" || len(in.Edits) == 0 {
		return
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	cur, err := os.ReadFile(path)
	if err != nil {
		return
	}
	prev, mtime, ok := fromMirror(path, cur, in.Edits)
	if !ok {
		prev, ok = reverseEdits(cur, in.Edits)
		mtime = fi.ModTime()
	}
	if !ok {
		fmt.Fprintf(stderr, "%s could not recover %s before this edit, so it is not saved\n", ui.ErrWarn("oops:"), ui.HomeRel(path))
		return
	}
	if cwd == "" {
		cwd = filepath.Dir(path)
	}
	if err := saveRecovered(path, prev, fi.Mode().Perm(), mtime, editLabel("Edit", []string{path}, cwd), cwd); err != nil {
		fmt.Fprintf(stderr, "%s could not save the file before the edit: %v\n", ui.ErrWarn("oops:"), err)
	}
}

// fromMirror returns the mirror's copy of path when applying the edits to
// it gives exactly the current text, which proves it is the version the
// edit started from. Copy on write mirrors keep that version; a hard link
// mirror shares the file, so it only does when Cursor replaced the file
// instead of writing into it.
func fromMirror(path string, cur []byte, edits []edit) ([]byte, time.Time, bool) {
	root := snap.MirrorRoot(filepath.Dir(path))
	if root == "" {
		return nil, time.Time{}, false
	}
	found := false
	for _, m := range snap.Mirrors() {
		if m.Root == root {
			found = true
			break
		}
	}
	if !found {
		return nil, time.Time{}, false
	}
	m, err := snap.OpenMirror(root)
	if err != nil {
		return nil, time.Time{}, false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, time.Time{}, false
	}
	data := filepath.Join(m.Dir, "data", rel)
	fi, err := os.Lstat(data)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, time.Time{}, false
	}
	b, err := os.ReadFile(data)
	if err != nil || bytes.Equal(b, cur) {
		return nil, time.Time{}, false
	}
	if out, ok := applyEdits(b, edits); !ok || !bytes.Equal(out, cur) {
		return nil, time.Time{}, false
	}
	return b, fi.ModTime(), true
}

// applyEdits replays the edits forward, each on its first match.
func applyEdits(b []byte, edits []edit) ([]byte, bool) {
	for _, e := range edits {
		if e.OldString == "" || !bytes.Contains(b, []byte(e.OldString)) {
			return nil, false
		}
		b = bytes.Replace(b, []byte(e.OldString), []byte(e.NewString), 1)
	}
	return b, true
}

// reverseEdits undoes the edits on the current text, last edit first. Each
// replacement text must occur exactly once, or where it came from is not
// known and nothing is guessed.
func reverseEdits(cur []byte, edits []edit) ([]byte, bool) {
	b := cur
	for i := len(edits) - 1; i >= 0; i-- {
		e := edits[i]
		if e.NewString == "" || bytes.Count(b, []byte(e.NewString)) != 1 {
			return nil, false
		}
		b = bytes.Replace(b, []byte(e.NewString), []byte(e.OldString), 1)
	}
	return b, !bytes.Equal(b, cur)
}

// saveRecovered stores a recovered version of path as an object snapshot.
// The object store saves files, so the version is written to a temporary
// file first, and the entry is then pointed at path.
func saveRecovered(path string, prev []byte, mode os.FileMode, mtime time.Time, label, cwd string) error {
	dir, err := os.MkdirTemp("", "oopsy-edit-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, filepath.Base(path))
	if err := os.WriteFile(tmp, prev, 0o600); err != nil {
		return err
	}
	_ = os.Chmod(tmp, mode)
	_ = os.Chtimes(tmp, mtime, mtime)
	s, err := snap.SaveVersions(label, cwd, "cursor", []string{tmp})
	if err != nil || s == nil {
		return err
	}
	for i := range s.Entries {
		s.Entries[i].Orig = path
		s.Entries[i].After = snap.StampOf(path)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	meta := filepath.Join(snap.SnapDir(s.ID), "meta.json")
	if err := os.WriteFile(meta+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(meta+".tmp", meta)
}
