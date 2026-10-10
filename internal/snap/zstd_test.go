package snap

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sohan-bhat/oopsy/internal/classify"
)

// Copies on filesystems without clones are zstd compressed, except files
// that are already compressed, and restore byte for byte.
func TestZstdCopiesRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows copies use NTFS compression")
	}
	withMethod(t, "copy")
	work := testEnv(t)
	text := bytes.Repeat([]byte("the same line of source code, again and again\n"), 50000)
	noise := make([]byte, 1<<20)
	rand.Read(noise)
	magic := append(append([]byte{}, zstdMagic...), []byte("not really a zstd frame")...)
	files := map[string][]byte{"big.txt": text, "photo.jpg": noise, "fake.zst": magic, "empty": nil}
	for name, b := range files {
		os.WriteFile(filepath.Join(work, name), b, 0o644)
	}
	s := create(t, work, classify.Action{Program: ">", Paths: []string{work}})
	e := s.Entries[0]
	if e.Method != "zstd copy" {
		t.Fatalf("method %q", e.Method)
	}
	if fi, _ := os.Stat(storeFile(s, e, "big.txt")); fi.Size() > int64(len(text))/20 {
		t.Errorf("text was not compressed: %d of %d bytes", fi.Size(), len(text))
	}
	if b, _ := os.ReadFile(storeFile(s, e, "photo.jpg")); !bytes.Equal(b, noise) {
		t.Error("an already compressed file should be stored as is")
	}
	os.RemoveAll(work)
	if _, err := Restore(s); err != nil {
		t.Fatal(err)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(work, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: restored %d bytes, want %d (%v)", name, len(got), len(want), err)
		}
	}
}
