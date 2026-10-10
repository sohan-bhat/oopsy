package e2e

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sohan-bhat/oopsy/internal/snap"
)

func writePseudoRandom(t *testing.T, p string, size int64) {
	t.Helper()
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriterSize(f, 4<<20)
	r := rand.NewChaCha8([32]byte{1, 2, 3})
	buf := make([]byte, 1<<20)
	for left := size; left > 0; left -= int64(len(buf)) {
		r.Read(buf)
		if _, err := w.Write(buf[:min(int64(len(buf)), left)]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func sha(t *testing.T, p string) string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	io.Copy(h, f)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// A 3 GB file overwritten in place on a disk without copy on write (ext4,
// NTFS) is copied before the overwrite, since there is room above the free
// space floor, and comes back byte for byte. With the floor raised so the
// copy would break it, Oopsy skips it and says so right away.
// Runs where OOPS_TEST_BIG=1 (Linux and Windows CI).
func TestBigOverwriteUnderFreeSpaceRule(t *testing.T) {
	if os.Getenv("OOPS_TEST_BIG") == "" {
		t.Skip("set OOPS_TEST_BIG=1 to write a 3 GB file")
	}
	e := newEnv(t)
	big := filepath.Join(e.work, "big.bin")
	const size = 3 << 30
	start := time.Now()
	writePseudoRandom(t, big, size)
	want := sha(t, big)
	t.Logf("wrote 3 GB in %s", time.Since(start).Round(time.Millisecond))

	start = time.Now()
	msg := e.guarded("echo overwritten > big.bin")
	t.Logf("snapshot and overwrite took %s: %s", time.Since(start).Round(time.Millisecond), strings.TrimSpace(msg))
	if fi, _ := os.Stat(big); fi.Size() > 100 {
		t.Fatal("the overwrite did not happen")
	}
	if strings.Contains(msg, "not saved") {
		t.Fatalf("the copy was skipped: %s", msg)
	}
	list := e.mustOops("show", "1")
	t.Logf("%s", list)
	os.Remove(big) // undo then puts the saved copy back in its place
	start = time.Now()
	e.mustOops("-y")
	t.Logf("restore took %s", time.Since(start).Round(time.Millisecond))
	if got := sha(t, big); got != want {
		t.Fatalf("restored file differs: %s, want %s", got, want)
	}

	// Now raise the floor so the copy would break it.
	free := diskFree(t, e.work)
	c := exec.Command(bin, "hook", "zsh", "--", "echo again > big.bin")
	c.Dir = e.work
	c.Env = append(e.environ(), fmt.Sprintf("OOPS_FREE_FLOOR_BYTES=%d", free-(1<<30)))
	out, _ := c.CombinedOutput()
	if !strings.Contains(string(out), "not saved") || !strings.Contains(string(out), "floor") {
		t.Fatalf("the skip was not reported right away: %s", out)
	}
	t.Logf("with the floor raised: %s", strings.TrimSpace(string(out)))
}

func diskFree(t *testing.T, p string) int64 {
	t.Helper()
	sp, err := snap.SpaceAt(p, snap.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return sp.Free
}
