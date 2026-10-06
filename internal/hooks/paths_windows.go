//go:build windows

package hooks

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// documentsDir is the real Documents folder, which OneDrive often moves.
// PowerShell keeps its profiles there.
func documentsDir() string {
	if d := os.Getenv("OOPS_DOCUMENTS_DIR"); d != "" {
		return d
	}
	if d, err := windows.KnownFolderPath(windows.FOLDERID_Documents, 0); err == nil && d != "" {
		return d
	}
	return filepath.Join(home(), "Documents")
}

// shortPath returns the 8.3 form of p, which has no spaces, so a hook
// command line works in cmd, PowerShell and Git Bash without quoting.
func shortPath(p string) string {
	u, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return p
	}
	buf := make([]uint16, 1024)
	n, err := windows.GetShortPathName(u, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 || int(n) > len(buf) {
		return p
	}
	s := windows.UTF16ToString(buf[:n])
	if strings.Contains(s, " ") {
		return p
	}
	return s
}
