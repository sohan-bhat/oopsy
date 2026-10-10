//go:build !windows

package hooks

import "errors"

// cmd.exe only exists on Windows.
type noAutoRun struct{}

var autoRun autoRunStore = noAutoRun{}

var errNoCmd = errors.New("cmd.exe is only on Windows")

func (noAutoRun) get() (autoRunValue, error) { return autoRunValue{}, errNoCmd }
func (noAutoRun) set(autoRunValue) error     { return errNoCmd }

func findClink() string { return "" }

func clinkScripts(string, bool, string) error { return errNoCmd }
