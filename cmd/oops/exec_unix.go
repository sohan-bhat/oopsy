//go:build !windows

package main

import (
	"os"
	"syscall"
)

// execDB replaces this process with oops-db.
func execDB(bin string, args []string) error {
	return syscall.Exec(bin, append([]string{bin}, args...), os.Environ())
}
