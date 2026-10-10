//go:build !windows

package watch

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// singleInstance takes the daemon lock, so only one runs per store.
func singleInstance() (func(), error) {
	f, err := os.OpenFile(filepath.Join(Dir(), "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("oops watch is already running")
	}
	return func() { f.Close() }, nil
}

// alive reports whether a process with this pid exists.
func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// stopPID asks a daemon started without a service manager to stop.
func stopPID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGTERM)
}

func hideConsole() {}
