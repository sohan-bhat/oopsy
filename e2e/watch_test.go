package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// watchStatus mirrors the fields of the daemon's status file the tests use.
type watchStatus struct {
	PID          int      `json:"pid"`
	Mechanism    string   `json:"mechanism"`
	Projects     []string `json:"projects"`
	Ready        bool     `json:"ready"`
	Events       uint64   `json:"events"`
	Syncs        uint64   `json:"syncs"`
	Snapshots    uint64   `json:"snapshots"`
	LastSnapshot int      `json:"last_snapshot"`
	LatencyMs    float64  `json:"latency_ms"`
	RSS          uint64   `json:"rss"`
	Heap         uint64   `json:"heap"`
	BaseFiles    int      `json:"base_files"`
	BaseBytes    int64    `json:"base_bytes"`
}

func (e *env) readWatchStatus() *watchStatus {
	b, err := os.ReadFile(filepath.Join(e.oops, "watch", "status.json"))
	if err != nil {
		return nil
	}
	var st watchStatus
	if json.Unmarshal(b, &st) != nil {
		return nil
	}
	return &st
}

// startWatch runs oops watch run as a child process (never a real
// service) and waits until it has synced every project in roots.
func (e *env) startWatch(t *testing.T, roots ...string) {
	t.Helper()
	c := exec.Command(bin, "watch", "run")
	c.Dir = e.work
	c.Env = append(e.environ(), "OOPS_WATCH_RESCAN=300ms")
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { c.Wait(); close(done) }()
	t.Cleanup(func() {
		if onWindows {
			c.Process.Kill()
		} else {
			c.Process.Signal(os.Interrupt)
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			c.Process.Kill()
			<-done
		}
		if t.Failed() {
			t.Logf("watch daemon output:\n%s", out.String())
		}
	})
	end := time.Now().Add(2 * time.Minute)
	for time.Now().Before(end) {
		if st := e.readWatchStatus(); st != nil && st.Ready && len(st.Projects) >= len(roots) {
			have := map[string]bool{}
			for _, p := range st.Projects {
				have[p] = true
			}
			all := true
			for _, r := range roots {
				all = all && have[r]
			}
			if all {
				return
			}
		}
		select {
		case <-done:
			t.Fatalf("watch daemon exited: %s", out.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("watch daemon never became ready: %+v\n%s", e.readWatchStatus(), out.String())
}

// lastSnapID is the newest snapshot id so far.
func (e *env) lastSnapID() int {
	ents, _ := os.ReadDir(filepath.Join(e.oops, "snapshots"))
	max := 0
	for _, d := range ents {
		if n, err := strconv.Atoi(d.Name()); err == nil && n > max {
			max = n
		}
	}
	return max
}

type snapMeta struct {
	ID      int    `json:"id"`
	Source  string `json:"source"`
	Command string `json:"command"`
	Entries []struct {
		Orig   string          `json:"orig"`
		Method string          `json:"method"`
		After  json.RawMessage `json:"after"`
	} `json:"entries"`
}

// waitWatchSnap waits for a watch snapshot newer than after that holds
// path, and returns it.
func (e *env) waitWatchSnap(t *testing.T, after int, path string) snapMeta {
	t.Helper()
	end := time.Now().Add(30 * time.Second)
	for time.Now().Before(end) {
		for id := after + 1; id <= e.lastSnapID(); id++ {
			b, err := os.ReadFile(filepath.Join(e.oops, "snapshots", strconv.Itoa(id), "meta.json"))
			if err != nil {
				continue
			}
			var s snapMeta
			if json.Unmarshal(b, &s) != nil || s.Source != "watch" {
				continue
			}
			for _, en := range s.Entries {
				if en.Orig == path {
					return s
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, _, _ := e.oopsCmd("", "list", "--watch")
	t.Fatalf("no watch snapshot of %s after %d\n%s\nstatus %+v", path, after, out, e.readWatchStatus())
	return snapMeta{}
}

// settle gives the daemon time to finish the sync it is on, including
// storing the new version of a changed file on hard link disks.
func (e *env) settle(t *testing.T) {
	t.Helper()
	time.Sleep(1200 * time.Millisecond)
}

func watchProject(t *testing.T, e *env) string {
	t.Helper()
	root := filepath.Join(e.work, "app")
	files := map[string]string{
		"src/main.c":  "int main(void) { return 0; }\n",
		"src/util.c":  "int util(void) { return 1; }\n",
		"README.md":   "# app\n",
		"docs/a.md":   "first doc\n",
		"bin/data.db": strings.Repeat("\x00\x01binary", 500),
	}
	for name, data := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e.git("init", "-q", root)
	return root
}

// Editor style saves with watch on: an atomic save (write a temp file and
// rename it over) and an in place save (truncate and write) each become a
// watch snapshot that oops undo restores byte for byte. A deletion becomes
// a snapshot plain oops undoes.
func TestWatchEditorSaves(t *testing.T) {
	e := newEnv(t)
	root := watchProject(t, e)
	e.mustOops("mirror", "build", root)
	e.startWatch(t, root)
	main := filepath.Join(root, "src", "main.c")
	original, _ := os.ReadFile(main)

	// Atomic save.
	before := e.lastSnapID()
	start := time.Now()
	tmp := filepath.Join(root, "src", ".main.c.swp")
	os.WriteFile(tmp, []byte("int main(void) { return 1; } // saved atomically\n"), 0o644)
	if err := os.Rename(tmp, main); err != nil {
		t.Fatal(err)
	}
	s := e.waitWatchSnap(t, before, main)
	t.Logf("atomic save: snapshot %d (%s) %v after the save", s.ID, s.Command, time.Since(start))
	if s.Command != "saved src/main.c" {
		t.Errorf("command %q", s.Command)
	}
	e.settle(t)
	out, _, _ := e.inDir(root).oopsCmd("", "-y")
	if strings.Contains(out, fmt.Sprintf("Snapshot %d ", s.ID)) {
		t.Fatalf("plain oops undid a watch snapshot of an edit:\n%s", out)
	}
	before = e.lastSnapID()
	e.mustOops("undo", strconv.Itoa(s.ID), "-y")
	if got, _ := os.ReadFile(main); !bytes.Equal(got, original) {
		t.Fatalf("atomic save not undone: %q", got)
	}
	e.waitWatchSnap(t, before, main) // the restore is a change too
	e.settle(t)

	// In place save.
	before = e.lastSnapID()
	start = time.Now()
	f, err := os.OpenFile(main, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("int main(void) { return 2; } /* written in place, longer */\n"))
	f.Close()
	s = e.waitWatchSnap(t, before, main)
	t.Logf("in place save: snapshot %d (%s, %s) %v after the save", s.ID, s.Command, s.Entries[0].Method, time.Since(start))
	e.mustOops("undo", strconv.Itoa(s.ID), "-y")
	if got, _ := os.ReadFile(main); !bytes.Equal(got, original) {
		t.Fatalf("in place save not undone: got %q want %q", got, original)
	}
	e.waitWatchSnap(t, s.ID, main)
	e.settle(t)

	// The list hides edits but shows deletions, which plain oops undoes.
	util := filepath.Join(root, "src", "util.c")
	utilData, _ := os.ReadFile(util)
	before = e.lastSnapID()
	if err := os.Remove(util); err != nil {
		t.Fatal(err)
	}
	s = e.waitWatchSnap(t, before, util)
	list := e.mustOops("list")
	if !strings.Contains(list, "deleted src/util.c") || strings.Contains(list, "saved src/main.c") || !strings.Contains(list, "hidden") {
		t.Errorf("oops list:\n%s", list)
	}
	if all := e.mustOops("list", "--watch"); !strings.Contains(all, "saved src/main.c") {
		t.Errorf("oops list --watch:\n%s", all)
	}
	out = e.inDir(root).mustOops("-y")
	if !strings.Contains(out, "deleted src/util.c") {
		t.Fatalf("plain oops undid something else:\n%s", out)
	}
	if got, _ := os.ReadFile(util); !bytes.Equal(got, utilData) {
		t.Fatalf("deleted file not restored: %q", got)
	}

	st := e.readWatchStatus()
	if st == nil || st.RSS == 0 || st.Snapshots == 0 {
		t.Fatalf("status: %+v", st)
	}
	status := e.mustOops("watch", "status")
	if !strings.Contains(status, "running") || !strings.Contains(status, "resident") {
		t.Errorf("oops watch status:\n%s", status)
	}
	t.Logf("daemon: %s, %d events, %d snapshots, RSS %.1f MB, heap %.1f MB, last latency %.0f ms",
		st.Mechanism, st.Events, st.Snapshots, float64(st.RSS)/(1<<20), float64(st.Heap)/(1<<20), st.LatencyMs)
}
