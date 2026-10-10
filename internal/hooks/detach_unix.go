//go:build !windows

package hooks

import (
	"os/exec"
	"syscall"
)

// detach makes c outlive the hook in its own session.
func detach(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
