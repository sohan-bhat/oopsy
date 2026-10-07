package snap

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"
)

type fileState struct {
	data  string
	mode  os.FileMode
	mtime time.Time
	link  string
}

func readState(t *testing.T, root string) map[string]fileState {
	t.Helper()
	out := map[string]fileState{}
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		st := fileState{mode: fi.Mode()}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			st.link, _ = os.Readlink(p)
		case fi.IsDir():
			st.mtime = fi.ModTime()
		default:
			b, _ := os.ReadFile(p)
			st.data, st.mtime = string(b), fi.ModTime()
		}
		out[filepath.ToSlash(rel)] = st
		return nil
	})
	return out
}

func diffStates(t *testing.T, want, got map[string]fileState) {
	t.Helper()
	var keys []string
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w, g := want[k], got[k]
		if _, ok := got[k]; !ok {
			t.Errorf("%s missing after restore", k)
			continue
		}
		if runtime.GOOS == "windows" {
			w.mode, g.mode = 0, 0
		}
		if w != g {
			t.Errorf("%s: got %+v, want %+v", k, g, w)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s exists after restore but did not before", k)
		}
	}
}

func mirrorProject(t *testing.T) string {
	work := testEnv(t)
	writeTree(t, work, map[string]string{
		"src/main.go":       "package main\n",
		"src/util/u.go":     "package util\n",
		"out/gen/a.o":       "object a\n",
		"out/gen/b.o":       "object b\n",
		"config.json":       `{"v": 1}` + "\n",
		"notes.txt":         "keep me\n",
		"node_modules/x.js": "module.exports = 1\n",
	})
	os.Symlink("src/main.go", filepath.Join(work, "link"))
	os.Chmod(filepath.Join(work, "notes.txt"), 0o444)
	os.MkdirAll(filepath.Join(work, "out", "empty"), 0o755)
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, d := range []string{"out/gen", "out/empty", "out", "src/util", "src"} {
		os.Chtimes(filepath.Join(work, d), old, old)
	}
	return work
}

// An unknown program deletes a folder, rewrites a file in place, replaces
// another and deletes a read only one. The mirror gives all of it back.
func TestMirrorCatchesUnknownCommands(t *testing.T) {
	for _, m := range []string{"", "hardlink"} {
		t.Run("method="+m, func(t *testing.T) {
			if m != "" {
				withMethod(t, m)
			}
			work := mirrorProject(t)
			before := readState(t, work)
			if _, err := MirrorBefore(work, Pending{Command: "./cleanup.sh", Cwd: work, Source: "zsh"}); err != nil {
				t.Fatal(err)
			}
			os.RemoveAll(filepath.Join(work, "out"))
			os.WriteFile(filepath.Join(work, "src", "main.go"), []byte("package main // rewritten\n"), 0o644)
			tmp := filepath.Join(work, "config.json.tmp")
			os.WriteFile(tmp, []byte(`{"v": 2}`), 0o644)
			os.Rename(tmp, filepath.Join(work, "config.json"))
			os.Chmod(filepath.Join(work, "notes.txt"), 0o644)
			os.Remove(filepath.Join(work, "notes.txt"))
			os.Remove(filepath.Join(work, "link"))
			res, err := MirrorAfter(work)
			if err != nil || res.Snapshot == nil {
				t.Fatalf("MirrorAfter: %v %+v", err, res)
			}
			s := res.Snapshot
			if m == "hardlink" {
				// The in place rewrite cannot be kept with hard links.
				if len(s.Skipped) != 1 || filepath.Base(s.Skipped[0].Path) != "main.go" {
					for _, e := range s.Entries {
						t.Logf("%+v", e)
					}
					t.Fatalf("skipped: %+v", s.Skipped)
				}
				os.WriteFile(filepath.Join(work, "src", "main.go"), []byte("package main\n"), 0o644)
				st := before["src/main.go"]
				os.Chtimes(filepath.Join(work, "src", "main.go"), st.mtime, st.mtime)
			}
			if _, err := Restore(s); err != nil {
				t.Fatal(err)
			}
			diffStates(t, before, readState(t, work))
			if b, _ := os.ReadFile(filepath.Join(work, "config.json")); !bytes.Contains(b, []byte(`"v": 1`)) {
				t.Errorf("config.json not swapped back: %s", b)
			}
		})
	}
}

// Changes made while no command was pending only refresh the mirror.
func TestMirrorIdleChangesAreNotAttributed(t *testing.T) {
	work := mirrorProject(t)
	if _, err := BuildMirror(work); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(work, "src", "main.go"), []byte("edited in an editor\n"), 0o644)
	res, err := MirrorBefore(work, Pending{Command: "ls", Cwd: work, Source: "zsh"})
	if err != nil || res.Snapshot != nil {
		t.Fatalf("idle edit was attributed: %v %+v", err, res)
	}
	os.Remove(filepath.Join(work, "src", "main.go"))
	res, err = MirrorAfter(work)
	if err != nil || res.Snapshot == nil {
		t.Fatalf("%v %+v", err, res)
	}
	os.WriteFile(filepath.Join(work, "x"), nil, 0o644)
	Restore(res.Snapshot)
	if b, _ := os.ReadFile(filepath.Join(work, "src", "main.go")); string(b) != "edited in an editor\n" {
		t.Fatalf("restored %q, want the editor's version", b)
	}
}
