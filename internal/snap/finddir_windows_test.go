//go:build windows

package snap

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Compares the two ways to list folders on Windows on a 10,000 file tree,
// and checks they agree.
func TestWindowsListersAgree(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 10000; i++ {
		p := filepath.Join(root, fmt.Sprintf("p%02d", i%50), fmt.Sprintf("s%02d", (i/50)%10), fmt.Sprintf("f%05d.go", i))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("package x\n"), 0o644)
	}
	walk := func(f func(string) ([]dirEntry, error)) (int, time.Duration) {
		start := time.Now()
		n := 0
		var rec func(string)
		rec = func(d string) {
			ents, _ := f(d)
			for _, e := range ents {
				n++
				if e.kind == 'd' {
					rec(filepath.Join(d, e.name))
				}
			}
		}
		rec(root)
		return n, time.Since(start)
	}
	for i := 0; i < 3; i++ {
		a, ta := walk(listDir)
		b, tb := walk(listDirFind)
		t.Logf("ReadDir: %d entries in %s, FindFirstFileEx: %d entries in %s", a, ta, b, tb)
		if a != b {
			t.Fatalf("listers disagree: %d vs %d", a, b)
		}
	}
	x, _ := listDir(filepath.Join(root, "p00", "s00"))
	y, _ := listDirFind(filepath.Join(root, "p00", "s00"))
	m := map[string]dirEntry{}
	for _, e := range x {
		m[e.name] = e
	}
	for _, e := range y {
		if m[e.name] != e {
			t.Errorf("%s: ReadDir %+v, FindFirstFileEx %+v", e.name, m[e.name], e)
		}
	}
}
