package snap

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sohan-bhat/oopsy/internal/classify"
)

// makeSparse writes a 1 GB file that holds 2 MB of data and holes.
func makeSparse(t *testing.T, p string) []byte {
	t.Helper()
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("docker disk image block "), 1<<16) // about 1.5 MB
	f.Write(data)
	f.WriteAt(data, 600<<20)
	f.Truncate(1 << 30)
	f.Close()
	if fi, _ := os.Stat(p); allocBytes(p, fi) > 100<<20 {
		t.Skipf("this filesystem does not keep holes (%d bytes allocated)", allocBytes(p, fi))
	}
	return data
}

func allocOf(t *testing.T, p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return allocBytes(p, fi)
}

// A sparse disk image counts what it takes on disk, not its length, and
// copies of it stay sparse both in the store and after a restore.
func TestSparseFilesStaySparse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows files are not sparse unless marked; covered by allocBytes")
	}
	for _, m := range []string{"", "copy"} {
		t.Run("method="+m, func(t *testing.T) {
			if m != "" {
				withMethod(t, m)
			}
			work := testEnv(t)
			img := filepath.Join(work, "Docker.raw")
			data := makeSparse(t, img)
			// A floor that a dense 1 GB copy would break, but the real 3 MB fits.
			sp, err := SpaceAt(work, Config{})
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("OOPS_FREE_FLOOR_BYTES", itoa(sp.Free-(512<<20)))
			s := create(t, work, classify.Action{Program: "rm", Paths: []string{img}, Delete: m == ""})
			if len(s.Skipped) > 0 {
				t.Fatalf("skipped: %+v", s.Skipped)
			}
			if s.Size > 50<<20 {
				t.Fatalf("a 1 GB sparse file with 3 MB of data counted as %d bytes", s.Size)
			}
			if a := allocOf(t, s.EntryPath(s.Entries[0])); a > 50<<20 {
				t.Fatalf("the stored copy takes %d bytes", a)
			}
			os.Remove(img)
			if _, err := Restore(s); err != nil {
				t.Fatal(err)
			}
			fi, _ := os.Stat(img)
			if fi.Size() != 1<<30 || allocOf(t, img) > 50<<20 {
				t.Fatalf("restored size %d, allocated %d", fi.Size(), allocOf(t, img))
			}
			f, _ := os.Open(img)
			got := make([]byte, len(data))
			f.ReadAt(got, 600<<20)
			hole := make([]byte, 4096)
			f.ReadAt(hole, 900<<20)
			f.Close()
			if !bytes.Equal(got, data) || !bytes.Equal(hole, make([]byte, 4096)) {
				t.Fatal("restored content differs")
			}
		})
	}
}

func itoa(n int64) string {
	b := []byte{}
	if n == 0 {
		return "0"
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
