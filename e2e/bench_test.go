package e2e

import (
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func stats(ds []time.Duration) (median, p95 time.Duration) {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2], ds[len(ds)*95/100]
}

func ms(d time.Duration) string { return fmt.Sprintf("%.2f ms", float64(d.Microseconds())/1000) }

// TestHookOverhead measures the full hook process (start, parse, classify,
// exit) for commands that pass the shell prefilter but are harmless.
// Run with: OOPS_BENCH=1 go test -run TestHookOverhead -v ./e2e/
func TestHookOverhead(t *testing.T) {
	if os.Getenv("OOPS_BENCH") == "" {
		t.Skip("set OOPS_BENCH=1")
	}
	e := newEnv(t)
	os.WriteFile(filepath.Join(e.work, "a.txt"), []byte("x"), 0o644)
	for _, cmd := range []string{"git status", "ls -la > /dev/null", "cp -n a.txt b.txt", "git log --oneline | head", "find . -name '*.go'"} {
		var ds []time.Duration
		for i := 0; i < 200; i++ {
			c := exec.Command(bin, "hook", "zsh", "--", cmd)
			c.Dir = e.work
			c.Env = e.environ()
			start := time.Now()
			if err := c.Run(); err != nil {
				t.Fatal(err)
			}
			ds = append(ds, time.Since(start))
		}
		med, p95 := stats(ds)
		t.Logf("hook %-28q median %s  p95 %s", cmd, ms(med), ms(p95))
	}
	// Baselines: the floor for any process based hook on this machine.
	empty := filepath.Join(e.home, "empty")
	src := filepath.Join(e.home, "empty.go")
	os.WriteFile(src, []byte("package main\nfunc main() {}\n"), 0o644)
	if out, err := exec.Command("go", "build", "-ldflags", "-s -w", "-o", empty, src).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	for _, b := range []string{"/usr/bin/true", empty} {
		var ds []time.Duration
		for i := 0; i < 200; i++ {
			start := time.Now()
			exec.Command(b).Run()
			ds = append(ds, time.Since(start))
		}
		med, p95 := stats(ds)
		t.Logf("baseline %-28s median %s  p95 %s", filepath.Base(b)+" (empty program)", ms(med), ms(p95))
	}
	// The shell prefilter: commands that do not mention a risky program never
	// start a process at all. Time the zsh regex test itself.
	if _, err := exec.LookPath("zsh"); err == nil {
		init, _, _ := e.oopsCmd("", "shell-init", "zsh")
		script := init + `
zmodload zsh/datetime
local s=$EPOCHREALTIME
for i in {1..10000}; do [[ "npm run test -- --watch" =~ $__OOPSY_RE ]]; done
print $(( (EPOCHREALTIME - s) * 1000000 / 10000 ))`
		out, err := exec.Command("zsh", "-f", "-c", script).Output()
		if err == nil {
			t.Logf("zsh prefilter for a harmless command: %s microseconds, no process started", strings.TrimSpace(string(out)))
		}
	}
}

func makeBigTree(t *testing.T, root string, totalMB, fileMB int) {
	t.Helper()
	buf := make([]byte, fileMB<<20)
	for i := 0; i < totalMB/fileMB; i++ {
		rand.Read(buf[:4096]) // vary each file a little; the rest repeats
		p := filepath.Join(root, fmt.Sprintf("d%02d", i%20), fmt.Sprintf("f%04d.bin", i))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, buf, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Plus a few thousand small files, like a source tree.
	for i := 0; i < 3000; i++ {
		p := filepath.Join(root, "src", fmt.Sprintf("pkg%02d", i%50), fmt.Sprintf("file%04d.go", i))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("package x\n// some source\n"), 0o644)
	}
}

// TestSnapshotSpeed1GB times snapshotting and restoring a 1 GB tree.
// Run with: OOPS_BENCH=1 go test -run TestSnapshotSpeed1GB -v ./e2e/
func TestSnapshotSpeed1GB(t *testing.T) {
	if os.Getenv("OOPS_BENCH") == "" {
		t.Skip("set OOPS_BENCH=1")
	}
	e := newEnv(t)
	tree := filepath.Join(e.work, "big")
	makeBigTree(t, tree, 1024, 1)
	for _, mode := range []string{"clone", "copy"} {
		env := e.environ()
		if mode == "copy" {
			env = append(env, "OOPS_FORCE_COPY=1")
		}
		c := exec.Command(bin, "hook", "zsh", "--", "rm -rf big")
		c.Dir = e.work
		c.Env = env
		start := time.Now()
		out, err := c.CombinedOutput()
		snapTime := time.Since(start)
		if err != nil {
			t.Fatalf("%v %s", err, out)
		}
		e.sh("rm -rf big")
		c = exec.Command(bin, "-y")
		c.Dir = e.work
		c.Env = env
		start = time.Now()
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		restoreTime := time.Since(start)
		t.Logf("%-5s snapshot of 1 GB, 4,044 files: %s   restore: %s   (%s)", mode, ms(snapTime), ms(restoreTime), string(out[:len(out)-1]))
	}
	df, _ := exec.Command("du", "-sh", e.oops).Output()
	t.Logf("snapshot store on disk after both runs: %s", df)
}
