package ui

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExplainNeverShowsARawGoError(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.txt")
	_, err := os.Open(missing)
	if got := Explain(err); !strings.Contains(got, "does not exist") || strings.Contains(got, "open ") {
		t.Errorf("not found: %q", got)
	}
	perm := &fs.PathError{Op: "open", Path: missing, Err: fs.ErrPermission}
	if got := Explain(perm); !strings.HasPrefix(got, "could not read") {
		t.Errorf("permission: %q", got)
	}
	if got := Explain(errors.New("plain words")); got != "plain words" {
		t.Errorf("plain: %q", got)
	}
}
