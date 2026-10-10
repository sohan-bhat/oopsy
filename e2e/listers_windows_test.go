package e2e

import (
	"path/filepath"
	"testing"

	"github.com/sohan-bhat/oopsy/internal/snap"
)

// Logs how fast each Windows folder lister walks a 10,000 file repository.
func TestWindowsListerSpeed(t *testing.T) {
	e := newEnv(t)
	root := filepath.Join(e.work, "repo")
	makeRepo(t, e, root, 10000)
	for i := 0; i < 3; i++ {
		rd, fd, a, b := snap.CompareListers(root)
		t.Logf("single thread walk: ReadDir %s for %d entries, FindFirstFileEx %s for %d entries", rd, a, fd, b)
	}
}
