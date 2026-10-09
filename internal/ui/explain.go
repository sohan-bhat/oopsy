package ui

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Explain turns an error from the operating system into a sentence a person
// can act on. Errors that are already plain text pass through unchanged.
func Explain(err error) string {
	if err == nil {
		return ""
	}
	op, path := pathOf(err)
	if path == "" {
		return err.Error()
	}
	shown := HomeRel(path)
	switch {
	case errors.Is(err, fs.ErrPermission):
		return explainPermission(op, path)
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Sprintf("%s does not exist", shown)
	case errors.Is(err, fs.ErrExist):
		return fmt.Sprintf("%s already exists", shown)
	case diskFull(err):
		return fmt.Sprintf("the disk holding %s is full; free up some space and run it again", shown)
	case readOnly(err):
		return fmt.Sprintf("%s is on a read-only disk, so it cannot be changed", shown)
	}
	return err.Error()
}

// pathOf digs the operation and path out of the error types os returns.
func pathOf(err error) (string, string) {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Op, pe.Path
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Op, le.New
	}
	return "", ""
}

func explainPermission(op, path string) string {
	verb := "change"
	switch op {
	case "open", "read", "stat", "lstat", "readdir":
		verb = "read"
	case "mkdir":
		verb = "create"
	}
	// The path itself often does not exist yet: mkdir fails because a folder
	// above it is locked. Blame the folder that is actually in the way.
	blocker := path
	for {
		if _, err := os.Lstat(blocker); err == nil {
			break
		}
		parent := filepath.Dir(blocker)
		if parent == blocker {
			break
		}
		blocker = parent
	}
	msg := fmt.Sprintf("could not %s %s", verb, HomeRel(path))
	if blocker != path {
		msg += fmt.Sprintf(" because %s is not writable by you", HomeRel(blocker))
	} else {
		msg += " because you do not have permission"
	}
	if owner := ownerName(blocker); owner == "root" || owner == "SYSTEM" {
		msg += fmt.Sprintf(". It belongs to %s, probably because it was created with sudo. Fix it with: sudo chown -R %s %s",
			owner, currentUser(), shellPath(blocker))
	} else if owner != "" && owner != currentUser() {
		msg += fmt.Sprintf(". It belongs to %s, not you", owner)
	}
	return msg
}

// shellPath writes p the way you would type it, with ~ for home and quotes
// when it has spaces.
func shellPath(p string) string {
	s := HomeRel(p)
	if strings.ContainsAny(s, " '\"$") {
		rest := strings.TrimPrefix(s, "~/")
		q := "'" + strings.ReplaceAll(rest, "'", `'\''`) + "'"
		if rest != s {
			return "~/" + q
		}
		return q
	}
	return s
}
