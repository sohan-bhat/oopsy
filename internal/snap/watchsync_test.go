package snap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Editor saves made while no command is pending become watch snapshots
// that give back the previous version, also on hard link mirrors where the
// save writes into the file in place.
func TestWatchSyncKeepsPreviousVersions(t *testing.T) {
	for _, m := range []string{"", "hardlink"} {
		t.Run("method="+m, func(t *testing.T) {
			if m != "" {
				withMethod(t, m)
			}
			work := mirrorProject(t)
			if _, err := BuildMirror(work); err != nil {
				t.Fatal(err)
			}
			max := int64(1 << 20)
			if _, err := UpdateWatchBase(work, max, true); err != nil {
				t.Fatal(err)
			}
			main := filepath.Join(work, "src", "main.go")
			cfg := filepath.Join(work, "config.json")

			// In place save: truncate and write.
			if err := os.WriteFile(main, []byte("package main // edited in place\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			res, err := WatchSync(work, max)
			if err != nil || res.Snapshot == nil {
				t.Fatalf("in place save: %v %+v", err, res)
			}
			s := res.Snapshot
			if s.Source != WatchSource || s.Command != "saved src/main.go" || !s.WatchEditsOnly() {
				t.Fatalf("snapshot %+v", s)
			}
			if m == "hardlink" && s.Entries[0].Method != "object" {
				t.Fatalf("hard link save not kept as an object: %+v", s.Entries[0])
			}
			if _, err := Restore(s); err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile(main); string(b) != "package main\n" {
				t.Fatalf("restored %q", b)
			}
			UpdateWatchBase(work, max, false)
			WatchSync(work, max) // the restore itself is a change

			// Atomic save: write a temp file and rename it over.
			tmp := cfg + ".swp"
			os.WriteFile(tmp, []byte(`{"v": 2, "saved": true}`+"\n"), 0o644)
			if err := os.Rename(tmp, cfg); err != nil {
				t.Fatal(err)
			}
			res, err = WatchSync(work, max)
			if err != nil || res.Snapshot == nil {
				t.Fatalf("atomic save: %v %+v", err, res)
			}
			if _, err := Restore(res.Snapshot); err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile(cfg); string(b) != `{"v": 1}`+"\n" {
				t.Fatalf("restored %q", b)
			}
			UpdateWatchBase(work, max, false)
			WatchSync(work, max)

			// A deletion counts as a command for plain oops.
			os.RemoveAll(filepath.Join(work, "out"))
			res, err = WatchSync(work, max)
			if err != nil || res.Snapshot == nil || res.Snapshot.WatchEditsOnly() {
				t.Fatalf("delete: %v %+v", err, res)
			}
			latest, _ := LatestUndo()
			if latest == nil || latest.ID != res.Snapshot.ID {
				t.Fatalf("LatestUndo = %+v, want %d", latest, res.Snapshot.ID)
			}
			if !strings.HasPrefix(res.Snapshot.Command, "deleted out") {
				t.Fatalf("command %q", res.Snapshot.Command)
			}
		})
	}
}

// Watch leaves changes alone while a command is pending, and skips files
// over the size limit.
func TestWatchSyncPendingAndLimit(t *testing.T) {
	work := mirrorProject(t)
	if _, err := MirrorBefore(work, Pending{Command: "./build.sh", Cwd: work, Source: "zsh"}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(work, "notes.txt.new"), nil, 0o644)
	os.Remove(filepath.Join(work, "src", "util", "u.go"))
	res, err := WatchSync(work, 1<<20)
	if err != nil || !res.Pending || res.Snapshot != nil {
		t.Fatalf("pending: %v %+v", err, res)
	}
	res2, err := MirrorAfter(work)
	if err != nil || res2.Snapshot == nil || res2.Snapshot.Command != "./build.sh" {
		t.Fatalf("the command lost its change: %v %+v", err, res2)
	}

	big := filepath.Join(work, "big.bin")
	os.WriteFile(big, make([]byte, 3000), 0o644)
	WatchSync(work, 1000)
	os.WriteFile(big, make([]byte, 4000), 0o644)
	os.Chtimes(big, time.Now().Add(time.Second), time.Now().Add(time.Second))
	os.WriteFile(filepath.Join(work, "config.json"), []byte("{}\n"), 0o644)
	res, err = WatchSync(work, 1000)
	if err != nil || res.Snapshot == nil {
		t.Fatalf("%v %+v", err, res)
	}
	s := res.Snapshot
	if len(s.Entries) != 1 || filepath.Base(s.Entries[0].Orig) != "config.json" || len(s.Skipped) != 1 {
		t.Fatalf("entries %+v skipped %+v", s.Entries, s.Skipped)
	}
}
