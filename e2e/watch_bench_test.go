package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Measures the watch daemon with 3 projects of 10,000 files each: its
// resident memory, the disk the stored versions take on hard link disks,
// and the time from a save to its snapshot. Set OOPS_WATCH_BENCH_FILES to
// change the project size.
func TestWatchFootprint(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	files := 10000
	if v, err := strconv.Atoi(os.Getenv("OOPS_WATCH_BENCH_FILES")); err == nil && v > 0 {
		files = v
	}
	e := newEnv(t)
	var roots []string
	line := "export function handler(req, res) { return res.json({ ok: true, at: Date.now() }) }\n"
	for p := 0; p < 3; p++ {
		root := filepath.Join(e.work, fmt.Sprintf("proj%d", p))
		for i := 0; i < files; i++ {
			dir := filepath.Join(root, "src", fmt.Sprintf("m%03d", i/100))
			if i%100 == 0 {
				os.MkdirAll(dir, 0o755)
			}
			body := fmt.Sprintf("// file %d of project %d\n%s", i, p, strings.Repeat(line, 3+i%40))
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d.ts", i)), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		e.mustOops("mirror", "build", root)
		roots = append(roots, root)
	}
	start := time.Now()
	e.startWatch(t, roots...)
	ready := time.Since(start)
	time.Sleep(2500 * time.Millisecond) // a fresh status report
	st := e.readWatchStatus()
	t.Logf("%s, %d projects of %d files ready in %v; RSS %.1f MB, Go heap %.1f MB",
		st.Mechanism, len(st.Projects), files, ready.Round(time.Millisecond), mb(st.RSS), mb(st.Heap))
	if st.BaseFiles > 0 {
		t.Logf("stored versions (hard link disk): %d files, %.1f MB compressed, %.0f bytes per file",
			st.BaseFiles, float64(st.BaseBytes)/(1<<20), float64(st.BaseBytes)/float64(st.BaseFiles))
	} else {
		t.Logf("no stored versions needed: this disk mirrors with clones")
	}

	// Saves spread over the projects; time each to its snapshot.
	var total time.Duration
	var worst time.Duration
	const saves = 12
	for i := 0; i < saves; i++ {
		root := roots[i%3]
		p := filepath.Join(root, "src", fmt.Sprintf("m%03d", (i*7)%(files/100)), fmt.Sprintf("f%05d.ts", ((i*7)%(files/100))*100))
		before := e.lastSnapID()
		t0 := time.Now()
		if err := os.WriteFile(p, []byte(fmt.Sprintf("// edit %d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		e.waitWatchSnap(t, before, p)
		d := time.Since(t0)
		total += d
		if d > worst {
			worst = d
		}
		time.Sleep(400 * time.Millisecond)
	}
	time.Sleep(2500 * time.Millisecond)
	st = e.readWatchStatus()
	t.Logf("save to snapshot: mean %v, worst %v (300 ms of it is the debounce); daemon's own last latency %.0f ms",
		(total / saves).Round(time.Millisecond), worst.Round(time.Millisecond), st.LatencyMs)
	t.Logf("after %d saves: RSS %.1f MB, Go heap %.1f MB, %d events, %d syncs", saves, mb(st.RSS), mb(st.Heap), st.Events, st.Syncs)
	if st.RSS > 100<<20 {
		t.Errorf("the daemon uses %.1f MB resident", mb(st.RSS))
	}
}

func mb(b uint64) float64 { return float64(b) / (1 << 20) }
