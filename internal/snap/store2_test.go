package snap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sohan-bhat/oopsy/internal/classify"
)

// With a store on a second drive, both layers copy into it, so the saved
// data does not depend on the first disk. A temp folder stands in for the
// other drive here; the Windows CI test uses a real second volume.
func TestSecondStoreCopiesEverything(t *testing.T) {
	work := mirrorProject(t)
	second := filepath.Join(filepath.Dir(work), "second-drive")
	cfg := LoadConfig()
	cfg.Store = second
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	before := readState(t, work)
	s := create(t, work, classify.Action{Program: "rm", Paths: []string{filepath.Join(work, "src")}, Delete: true})
	if e := s.Entries[0]; e.Method == "hardlink" || e.Method == "clone" || e.Base != "" || !within(s.EntryPath(e), second) {
		t.Fatalf("classifier entry not copied into the second store: %+v", e)
	}
	if _, err := MirrorBefore(work, Pending{Command: "./cleanup.sh", Cwd: work, Source: "zsh"}); err != nil {
		t.Fatal(err)
	}
	m, _ := OpenMirror(work)
	if !within(m.Dir, second) {
		t.Fatalf("mirror at %s, not in the second store", m.Dir)
	}
	os.RemoveAll(filepath.Join(work, "src"))
	os.WriteFile(filepath.Join(work, "config.json"), []byte("{}"), 0o644)
	res, err := MirrorAfter(work)
	if err != nil || res.Snapshot == nil {
		t.Fatalf("%v %+v", err, res)
	}
	if _, err := Restore(res.Snapshot); err != nil {
		t.Fatal(err)
	}
	diffStates(t, before, readState(t, work))
}
