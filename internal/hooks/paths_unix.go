//go:build !windows

package hooks

import "path/filepath"

// documentsDir is where PowerShell keeps profiles on Windows; on macOS and
// Linux pwsh uses ~/.config/powershell instead (see Targets).
func documentsDir() string { return filepath.Join(home(), "Documents") }

func shortPath(p string) string { return p }
