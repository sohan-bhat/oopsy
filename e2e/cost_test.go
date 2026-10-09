package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// makeRepo builds a source tree of n small files in a git work tree.
func makeRepo(t *testing.T, e *env, root string, n int) {
	t.Helper()
	body := []byte("package x\n\n// Some source code that is about this long on average.\nfunc F() int { return 42 }\n")
	for i := 0; i < n; i++ {
		p := filepath.Join(root, fmt.Sprintf("pkg%02d", i%50), fmt.Sprintf("sub%02d", (i/50)%10), fmt.Sprintf("file%05d.go", i))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e.git("init", "-q", root)
}

// TestGenericCost measures what the generic layer costs on a 10,000 file
// repository: the time a hook adds before a command, the after step, and the
// disk the mirror takes per file. It always runs and logs its numbers; it
// fails only on a gross regression.
func TestGenericCost(t *testing.T) {
	e := newEnv(t)
	root := filepath.Join(e.work, "repo")
	const n = 10000
	makeRepo(t, e, root, n)

	free0 := diskFree(t, e.work)
	start := time.Now()
	out := e.mustOops("mirror", "build", root)
	build := time.Since(start)
	time.Sleep(500 * time.Millisecond)
	free1 := diskFree(t, e.work)
	idx := int64(0)
	if fi, err := os.Stat(mirrorIndex(t, e)); err == nil {
		idx = fi.Size()
	}
	mirrors, _, _ := e.oopsCmd("", "mirror")
	t.Logf("first mirror build: %s (%s) %s", build.Round(time.Millisecond), trim(out), trim(mirrors))
	t.Logf("mirror disk: %d bytes free space used for %d files (%.0f bytes per file, noisy), index %d bytes (%.0f bytes per file)",
		free0-free1, n, float64(free0-free1)/n, idx, float64(idx)/n)

	hook := func(args ...string) time.Duration {
		c := exec.Command(bin, args...)
		c.Dir = root
		c.Env = append(e.environ(), "OOPSY_SESSION=bench")
		s := time.Now()
		if b, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v %s", args, err, b)
		}
		return time.Since(s)
	}
	var pre, post, ro []time.Duration
	for i := 0; i < 30; i++ {
		pre = append(pre, hook("hook", "zsh", "--", "./deploy.sh --fast"))
		post = append(post, hook("hook", "zsh-post"))
		ro = append(ro, hook("hook", "zsh", "--", "git status"))
	}
	pm, p95 := stats(pre)
	am, a95 := stats(post)
	rm, _ := stats(ro)
	t.Logf("before a command (sync walk of %d files): median %s, p95 %s", n, ms(pm), ms(p95))
	t.Logf("after a command (runs in the background in shells): median %s, p95 %s", ms(am), ms(a95))
	t.Logf("read only line that reaches the hook process anyway: median %s (shells skip these without a process)", ms(rm))
	empty := baselineProcess(t, e)
	t.Logf("baseline: starting an empty Go program takes %s here, so the added time per command is about %s", ms(empty), ms(pm))
	if pm > 400*time.Millisecond {
		t.Errorf("the hook before a command got slow: %s", ms(pm))
	}
	_ = runtime.GOOS
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func mirrorIndex(t *testing.T, e *env) string {
	ents, _ := os.ReadDir(filepath.Join(e.oops, "mirrors"))
	for _, d := range ents {
		return filepath.Join(e.oops, "mirrors", d.Name(), "index")
	}
	return ""
}

func baselineProcess(t *testing.T, e *env) time.Duration {
	src := filepath.Join(e.home, "empty.go")
	exe := filepath.Join(e.home, "empty"+map[bool]string{true: ".exe"}[onWindows])
	os.WriteFile(src, []byte("package main\nfunc main() {}\n"), 0o644)
	if out, err := exec.Command("go", "build", "-o", exe, src).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	var ds []time.Duration
	for i := 0; i < 30; i++ {
		s := time.Now()
		exec.Command(exe).Run()
		ds = append(ds, time.Since(s))
	}
	m, _ := stats(ds)
	return m
}
