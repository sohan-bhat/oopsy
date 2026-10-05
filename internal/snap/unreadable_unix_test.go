//go:build !windows

package snap

import (
	"os"
	"testing"
)

// lockForTest makes p unreadable, the closest Unix gets to a file another
// program holds open without sharing.
func lockForTest(t *testing.T, p string) func() {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root can read anything")
	}
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	return func() { os.Chmod(p, 0o644) }
}
