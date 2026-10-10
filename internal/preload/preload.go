// Package preload installs Oopsy's Linux preload library, which copies a
// mirrored file right before a program first writes into it in place. See
// csrc/oopsy_preload.c for what it does and does not cover.
package preload

import (
	"bytes"
	"os"
	"path/filepath"

	"github.com/sohan-bhat/oopsy/internal/snap"
)

// Path is where the library is installed.
func Path() string { return filepath.Join(snap.Dir(), "lib", "liboopsy_preload.so") }

// Install writes the library for this machine when it is missing or out of
// date, and returns its path, or "" where there is none (not Linux, or an
// architecture without a build).
func Install() string {
	lib := library()
	if len(lib) == 0 || os.Getenv("OOPS_NO_PRELOAD") != "" || snap.LoadConfig().NoPreload {
		return ""
	}
	p := Path()
	if cur, err := os.ReadFile(p); err == nil && bytes.Equal(cur, lib) {
		return p
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return ""
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, lib, 0o755); err != nil {
		return ""
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return ""
	}
	return p
}
