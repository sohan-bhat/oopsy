//go:build !windows

package classify

import "os/exec"

func quoteForMSYS(*exec.Cmd) {}
