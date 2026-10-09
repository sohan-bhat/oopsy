package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A store on a second drive holds real copies of everything, so a restore
// works without the original disk's blocks. On Windows CI it is a real
// second volume (D:); elsewhere a folder stands in for it.
func TestSecondDriveStore(t *testing.T) {
	e := newEnv(t)
	store := filepath.Join(filepath.Dir(e.work), "second-drive")
	if onWindows {
		if _, err := os.Stat(`D:\`); err == nil {
			store = filepath.Join(`D:\`, "oops-second-store-"+filepath.Base(filepath.Dir(e.work)))
			t.Cleanup(func() { forceRemove(store) })
		}
	}
	out := e.mustOops("config", "store", store)
	if !strings.Contains(out, "full compressed copy") {
		t.Fatalf("config did not explain the cost: %s", out)
	}
	tree := filepath.Join(e.work, "project")
	makeTree(t, tree)
	before := snapshotTree(t, tree, true)
	e.guarded("rm -rf project")
	show := e.mustOops("show", "1")
	if strings.Contains(show, "hardlink") || strings.Contains(show, "clone") {
		t.Fatalf("a second drive store must hold copies:\n%s", show)
	}
	if ents, _ := os.ReadDir(filepath.Join(store, "snapshots")); len(ents) == 0 {
		t.Fatal("nothing was written to the second drive")
	}
	e.mustOops("-y")
	compareTrees(t, before, snapshotTree(t, tree, true))
}
