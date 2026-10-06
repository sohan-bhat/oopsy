//go:build windows

package classify

import (
	"os/exec"
	"strings"
	"syscall"
)

// quoteForMSYS gives an MSYS program (Git's find.exe) a command line with
// every argument in double quotes. Unquoted arguments such as *.log would
// otherwise be globbed by the MSYS runtime before find sees them.
func quoteForMSYS(cmd *exec.Cmd) {
	parts := make([]string, len(cmd.Args))
	for i, a := range cmd.Args {
		parts[i] = `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: strings.Join(parts, " ")}
}
