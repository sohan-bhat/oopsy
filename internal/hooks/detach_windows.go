//go:build windows

package hooks

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detach makes c outlive the hook without a console window.
func detach(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP, HideWindow: true}
}
