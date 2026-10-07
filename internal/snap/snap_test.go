package snap

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sohan-bhat/oopsy/internal/classify"
)

// withMethod forces a snapshot method for one test.
func withMethod(t *testing.T, m string) {
	t.Helper()
	oldMethod, oldForce := method, forceCopy
	method, forceCopy = m, m == "copy" || m == "hardlink"
	t.Cleanup(func() { method, forceCopy = oldMethod, oldForce })
}

// testEnv gives each test its own store and work directory on one volume.
func testEnv(t *testing.T) string {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("OOPS_HOME", filepath.Join(root, "store"))
	work := filepath.Join(root, "work")
	writeTree(t, work, map[string]string{
		"a.txt":         "alpha\n",
		"sub/b.txt":     "bravo\n",
		"sub/deep/c.go": "package c\n",
	})
	return work
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	for name, data := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, old, old)
	}
}

func create(t *testing.T, cwd string, actions ...classify.Action) *Snapshot {
	t.Helper()
	s, err := Create("test", cwd, "manual", actions)
	if err != nil || s == nil {
		t.Fatalf("Create: %v %v", s, err)
	}
	return s
}

func storeFile(s *Snapshot, e Entry, rel string) string {
	return filepath.Join(s.EntryPath(e), filepath.FromSlash(rel))
}

func TestHardLinksForDeletes(t *testing.T) {
	withMethod(t, "hardlink")
	work := testEnv(t)
	tree := filepath.Join(work, "sub")
	s := create(t, work, classify.Action{Program: "rm", Paths: []string{tree}, Delete: true})
	if len(s.Entries) != 1 {
		t.Fatalf("entries: %+v skipped: %+v", s.Entries, s.Skipped)
	}
	e := s.Entries[0]
	if e.Method != "hardlink" || e.Disk != 0 {
		t.Fatalf("method %q disk %d, want hardlink and 0", e.Method, e.Disk)
	}
	orig, _ := os.Stat(filepath.Join(tree, "b.txt"))
	saved, _ := os.Stat(storeFile(s, e, "b.txt"))
	if !os.SameFile(orig, saved) {
		t.Fatal("snapshot file is not a hard link to the original")
	}
	if err := os.RemoveAll(tree); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(s); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(tree, "deep", "c.go"))
	if err != nil || string(b) != "package c\n" {
		t.Fatalf("restore: %q %v", b, err)
	}
	fi, _ := os.Stat(filepath.Join(tree, "b.txt"))
	if !fi.ModTime().Equal(time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("mtime %v not kept", fi.ModTime())
	}
	if saved, _ := os.Stat(storeFile(s, e, "b.txt")); !os.SameFile(fi, saved) {
		t.Error("restore copied a hard link snapshot instead of linking it back")
	}
}

func TestOverwritesNeverUseHardLinks(t *testing.T) {
	withMethod(t, "hardlink")
	work := testEnv(t)
	f := filepath.Join(work, "a.txt")
	for _, actions := range [][]classify.Action{
		{{Program: ">", Paths: []string{f}}},
		// One command that deletes the folder and overwrites a file in it.
		{{Program: "rm", Paths: []string{work}, Delete: true}, {Program: "cp", Paths: []string{f}}},
	} {
		s := create(t, filepath.Dir(work), actions...)
		e := s.Entries[0]
		if e.Method != "copy" && e.Method != "compressed copy" {
			t.Fatalf("%v: method %q, want copy", actions, e.Method)
		}
		orig, _ := os.Stat(f)
		saved, _ := os.Stat(storeFile(s, e, relKey(e.Orig, f)))
		if os.SameFile(orig, saved) {
			t.Fatal("an overwrite was snapshotted with a hard link")
		}
		// Writing into the original must not touch the snapshot.
		os.WriteFile(f, []byte("changed\n"), 0o644)
		if b, _ := os.ReadFile(storeFile(s, e, relKey(e.Orig, f))); !bytes.Equal(b, []byte("alpha\n")) {
			t.Fatalf("snapshot changed with the original: %q", b)
		}
		os.WriteFile(f, []byte("alpha\n"), 0o644)
	}
}

func TestCopyKeepsGoingPastUnreadableFiles(t *testing.T) {
	withMethod(t, "copy")
	work := testEnv(t)
	locked := filepath.Join(work, "sub", "b.txt")
	release := lockForTest(t, locked)
	defer release()
	s := create(t, work, classify.Action{Program: "rm", Paths: []string{filepath.Join(work, "sub")}, Delete: true})
	if len(s.Entries) != 1 {
		t.Fatalf("the folder was dropped: %+v", s.Skipped)
	}
	if len(s.Skipped) != 1 || s.Skipped[0].Path != locked {
		t.Fatalf("skipped %+v, want just %s", s.Skipped, locked)
	}
	if _, err := os.Stat(storeFile(s, s.Entries[0], "deep/c.go")); err != nil {
		t.Fatal("other files were not saved")
	}
}

func TestMinimalPaths(t *testing.T) {
	sep := string(filepath.Separator)
	root := filepath.Join(sep+"x", "y")
	got := minimalPaths([]string{filepath.Join(root, "a", "b"), filepath.Join(root, "a"), filepath.Join(root, "ab"), root + sep + "a" + sep})
	want := []string{filepath.Join(root, "a"), filepath.Join(root, "ab")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("minimalPaths = %v, want %v", got, want)
	}
}

// TestHardLinkKeepsReadOnly mimics Remove-Item -Force, which makes a read
// only file writable before deleting it. Through a hard link that change
// reaches the snapshot, so the original mode must come back on restore.
func TestHardLinkKeepsReadOnly(t *testing.T) {
	withMethod(t, "hardlink")
	work := testEnv(t)
	f := filepath.Join(work, "sub", "b.txt")
	os.Chmod(f, 0o444)
	s := create(t, work, classify.Action{Program: "Remove-Item", Paths: []string{filepath.Join(work, "sub")}, Delete: true})
	if s.Entries[0].Method != "hardlink" {
		t.Fatalf("method %s", s.Entries[0].Method)
	}
	os.Chmod(f, 0o644)
	os.RemoveAll(filepath.Join(work, "sub"))
	if _, err := Restore(s); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(f)
	if err != nil || fi.Mode().Perm()&0o200 != 0 {
		t.Fatalf("read only not restored: %v %v", fi.Mode(), err)
	}
	if saved, _ := os.Stat(storeFile(s, s.Entries[0], "b.txt")); os.SameFile(fi, saved) {
		t.Error("a read only file was linked back; clearing it later would change the snapshot")
	}
}
