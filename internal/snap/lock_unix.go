//go:build !windows

package snap

import (
	"os"
	"path/filepath"
	"syscall"
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
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return &lock{f}, nil
}

func (l *lock) release() {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	l.f.Close()
}
