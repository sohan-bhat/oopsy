//go:build windows

package snap

import (
	"testing"

	"golang.org/x/sys/windows"
)

// lockForTest opens p with no sharing, the way some editors and databases
// hold their files, so nobody else can read it until the handle closes.
func lockForTest(t *testing.T, p string) func() {
	t.Helper()
	u, _ := windows.UTF16PtrFromString(p)
	h, err := windows.CreateFile(u, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return func() { windows.CloseHandle(h) }
}
