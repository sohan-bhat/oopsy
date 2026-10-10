//go:build !windows

package watch

import (
	"os/exec"
	"syscall"
)

// detach makes c outlive this process in its own session.
func detach(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
