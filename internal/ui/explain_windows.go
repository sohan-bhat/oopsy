//go:build windows

package ui

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// ownerName is left empty on Windows: access there is decided by ACLs, so
// the owner rarely explains a denied write.
func ownerName(string) string { return "" }

func currentUser() string { return os.Getenv("USERNAME") }

func diskFull(err error) bool {
	return errors.Is(err, windows.ERROR_DISK_FULL) || errors.Is(err, windows.ERROR_HANDLE_DISK_FULL)
}

func readOnly(err error) bool { return errors.Is(err, windows.ERROR_WRITE_PROTECT) }
