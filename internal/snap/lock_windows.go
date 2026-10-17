//go:build windows

package snap

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

type lock struct{ f *os.File }

func acquire() (*lock, error) {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return nil, err
	}
	return lockAt(filepath.Join(Dir(), "lock"))
}

// lockAt takes an exclusive lock on the file at p, waiting for it.
func lockAt(p string) (*lock, error) {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ol := new(windows.Overlapped)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, ol); err != nil {
		f.Close()
		return nil, err
	}
	return &lock{f}, nil
}

// tryLockAt takes the lock at p only when nobody holds it, and returns nil
// when it is busy.
func tryLockAt(p string) *lock {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil
	}
	ol := new(windows.Overlapped)
	if windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol) != nil {
		f.Close()
		return nil
	}
	return &lock{f}
}

func (l *lock) release() {
	if l.f == nil {
		return
	}
	ol := new(windows.Overlapped)
	_ = windows.UnlockFileEx(windows.Handle(l.f.Fd()), 0, 1, 0, ol)
	l.f.Close()
	l.f = nil
}
