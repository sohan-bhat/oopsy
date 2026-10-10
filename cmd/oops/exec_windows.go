//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
)

// execDB runs oops-db as a child, since Windows cannot replace a running
// process, and exits with its status. Ctrl+C reaches both processes through
// the console, so this one just waits for the child to finish.
func execDB(bin string, args []string) error {
	signal.Ignore(os.Interrupt)
	c := exec.Command(bin, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := c.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.ExitCode())
	}
	if err != nil {
		return err
	}
	os.Exit(0)
	return nil
}
