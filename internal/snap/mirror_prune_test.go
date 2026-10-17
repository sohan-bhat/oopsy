package snap

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// buildMirror mirrors a small project named name under the test's root and
// backdates its last sync by age.
func buildMirror(t *testing.T, root, name string, age time.Duration) *Mirror {
	t.Helper()
	dir := filepath.Join(root, name)
	writeTree(t, dir, map[string]string{"main.go": "package main\n", "sub/a.txt": "a\n"})
	if _, err := BuildMirror(dir); err != nil {
		t.Fatal(err)
	}
	m, err := OpenMirror(dir)
	if err != nil {
		t.Fatal(err)
	}
	mm, _ := m.meta()
	mm.Synced = time.Now().Add(-age)
	mm.Created = mm.Synced
	if err := m.saveMeta(mm); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPruneMirrorsDropsUnusedAndKeepsUndoWindow(t *testing.T) {
	work := testEnv(t)
	root := filepath.Dir(work)
	day := 24 * time.Hour
	live := buildMirror(t, root, "live", time.Minute)
	old := buildMirror(t, root, "old", 10*day)
	gone := buildMirror(t, root, "gone", time.Hour)
	goneOld := buildMirror(t, root, "gone-old", 8*day)
	os.RemoveAll(gone.Root)
	os.RemoveAll(goneOld.Root)

	res := PruneMirrors(LoadConfig(), MirrorsStale)
	if res.Removed != 2 {
		t.Errorf("removed %d mirrors, want 2", res.Removed)
	}
	if !exists(live.Dir) || !exists(gone.Dir) {
		t.Error("a mirror in use or inside the undo window was removed")
	}
	if exists(old.Dir) || exists(goneOld.Dir) {
		t.Error("a mirror unused for keep_days was kept")
	}

	PruneMirrors(LoadConfig(), MirrorsGone)
	if exists(gone.Dir) {
		t.Error("--gone kept the mirror of a deleted folder")
	}
	if !exists(live.Dir) {
		t.Error("--gone removed the mirror of a live folder")
	}
}

func TestPruneMirrorsSkipsBusyAndPending(t *testing.T) {
	work := testEnv(t)
	root := filepath.Dir(work)
	busy := buildMirror(t, root, "busy", time.Hour)
	pend := buildMirror(t, root, "pending", time.Hour)
	l, err := busy.lock()
	if err != nil {
		t.Fatal(err)
	}
	defer l.release()
	if err := pend.setPending(&Pending{Command: "make", Cwd: pend.Root, Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
	PruneMirrors(LoadConfig(), MirrorsAll)
	if !exists(busy.Dir) || !exists(pend.Dir) {
		t.Error("a busy mirror or one with a pending command was removed")
	}
}

func TestPruneMirrorsUnderFloorGoesOldestFirst(t *testing.T) {
	work := testEnv(t)
	root := filepath.Dir(work)
	keep := buildMirror(t, root, "keep", time.Minute)
	gone := buildMirror(t, root, "gone", time.Hour)
	os.RemoveAll(gone.Root)
	// A floor no disk can meet makes every idle mirror go, the gone one too.
	t.Setenv("OOPS_FREE_FLOOR_BYTES", "9223372036854775807")
	PruneMirrors(LoadConfig(), MirrorsStale)
	if exists(gone.Dir) || exists(keep.Dir) {
		t.Error("mirrors were kept while the disk was under its floor")
	}
}

func TestTooBroadCoversTempAndAppFolders(t *testing.T) {
	home, _ := os.UserHomeDir()
	tmp := os.TempDir()
	for _, p := range []string{tmp, filepath.Dir(filepath.Clean(tmp)), filepath.Join(home, "Library", "Caches"), filepath.Join(home, ".cache", "uv")} {
		if !tooBroad(p) {
			t.Errorf("%s should be too broad to mirror", p)
		}
	}
	if p := filepath.Join(tmp, "project"); tooBroad(p) {
		t.Errorf("%s is a project inside the temp folder and should be mirrored", p)
	}
}

func TestMirrorSweepDueWaitsAnHour(t *testing.T) {
	work := testEnv(t)
	buildMirror(t, filepath.Dir(work), "p", time.Minute)
	cfg := LoadConfig()
	if MirrorSweepDue(cfg) {
		t.Error("a store that never swept should start its clock, not sweep")
	}
	if MirrorSweepDue(cfg) {
		t.Error("the sweep ran again within the hour")
	}
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(sweepStamp(), old, old)
	if !MirrorSweepDue(cfg) {
		t.Error("the sweep did not come due after an hour")
	}
}

// On Windows the temp folder lives inside AppData\Local. A project in it is
// mirrored, while the rest of AppData\Local is not.
func TestTempInsideAppDataStillMirrors(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	home := filepath.Join(root, "home")
	tmp := filepath.Join(home, "AppData", "Local", "Temp")
	os.MkdirAll(tmp, 0o700)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	if p := filepath.Join(tmp, "project"); tooBroad(p) {
		t.Errorf("%s is a project in the temp folder and should be mirrored", p)
	}
	if p := filepath.Join(home, "AppData", "Local", "Programs"); !tooBroad(p) {
		t.Errorf("%s should be too broad to mirror", p)
	}
}
