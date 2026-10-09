package snap

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// A snapshot taken before a command records, once the command is done,
// what it left at each overwritten file (see Entry.After). Hooks register
// the snapshot here before the command runs, and the step after it, or the
// next oops, fills the stamps in.

func afterDir() string { return filepath.Join(Dir(), "after") }

func sessionFile(session string) string {
	h := sha256.Sum256([]byte(session))
	return filepath.Join(afterDir(), hex.EncodeToString(h[:6]))
}

// AwaitAfter remembers that snapshot id belongs to a command now running in
// session.
func AwaitAfter(session string, id int) {
	if err := os.MkdirAll(afterDir(), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(sessionFile(session), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(strconv.Itoa(id) + "\n")
}

// FinishAfter fills in what the finished commands of a session left, or of
// every session when session is "*".
func FinishAfter(session string) {
	var files []string
	if session == "*" {
		ents, _ := os.ReadDir(afterDir())
		for _, e := range ents {
			files = append(files, filepath.Join(afterDir(), e.Name()))
		}
	} else {
		files = []string{sessionFile(session)}
	}
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		_ = os.Remove(p)
		for _, line := range strings.Fields(string(b)) {
			if id, err := strconv.Atoi(line); err == nil {
				_ = SetAfter(id)
			}
		}
	}
}
