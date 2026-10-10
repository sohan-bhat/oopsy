package snap

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MirrorRoot picks the scope the generic layer protects for a command run
// in cwd: the git work tree around it, or cwd itself. It returns "" when that
// would be too broad to mirror (the home folder, a volume root, the store).
func MirrorRoot(cwd string) string {
	cwd = filepath.Clean(cwd)
	root := cwd
	for d := cwd; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			root = d
			break
		}
		parent := filepath.Dir(d)
		if parent == d || tooBroad(parent) {
			break
		}
		d = parent
	}
	if tooBroad(root) {
		return ""
	}
	return root
}

// tooBroad reports folders the mirror never covers whole.
func tooBroad(p string) bool {
	p = filepath.Clean(p)
	if filepath.Dir(p) == p || refuse(p) != "" {
		return true
	}
	home, _ := os.UserHomeDir()
	if home != "" && (pathKey(p) == pathKey(filepath.Clean(home)) || within(home, p)) {
		return true
	}
	for _, sys := range systemDirs() {
		if within(p, sys) {
			return true
		}
	}
	return false
}

func systemDirs() []string {
	if os.PathSeparator == '\\' {
		win := os.Getenv("SystemRoot")
		if win == "" {
			win = `C:\Windows`
		}
		return []string{win, os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramData")}
	}
	return []string{"/usr", "/bin", "/sbin", "/etc", "/System", "/Library", "/opt/homebrew", "/proc", "/sys", "/dev"}
}

func mirrorLimit() int {
	if n := LoadConfig().MirrorMaxFiles; n > 0 {
		return n
	}
	return DefaultMirrorMaxFiles
}

// MirrorBefore runs before a command that is not read only: changes since
// the last sync are attributed to any command still pending, the mirror is
// brought up to date, and p becomes the pending command. A project seen for
// the first time is mirrored now when small, else in the background.
func MirrorBefore(root string, p Pending) (*SyncResult, error) {
	m, err := OpenMirror(root)
	if err != nil {
		return nil, err
	}
	Trace("open mirror")
	l, err := m.lock()
	if err != nil {
		return nil, err
	}
	defer l.release()
	Trace("lock mirror")
	var res *SyncResult
	if !m.Exists() {
		w, err := walkProject(root, mirrorLimit())
		if err != nil {
			return nil, err
		}
		if w.files > firstBuildInline && os.Getenv("OOPS_MIRROR_INLINE") == "" {
			return nil, ErrBuilding{Files: w.files}
		}
	}
	res, err = m.sync(m.pending(), mirrorLimit())
	if err != nil {
		return nil, err
	}
	p.Time = time.Now()
	err = m.setPending(&p)
	Trace("mark pending")
	return res, err
}

// firstBuildInline is the largest project mirrored while a command waits.
// Bigger ones are built by oops mirror build in the background.
const firstBuildInline = 3000

// ErrBuilding means the project is new and big, so its mirror is being built
// in the background and this command is covered by the command line layer
// only.
type ErrBuilding struct{ Files int }

func (e ErrBuilding) Error() string { return "the project mirror is being built" }

// BuildMirror builds or refreshes a mirror without attributing anything.
func BuildMirror(root string) (*SyncResult, error) {
	m, err := OpenMirror(root)
	if err != nil {
		return nil, err
	}
	l, err := m.lock()
	if err != nil {
		return nil, err
	}
	defer l.release()
	res, err := m.sync(m.pending(), mirrorLimit())
	if err == nil {
		_ = m.setPending(nil)
	}
	return res, err
}

// MirrorAfter attributes what the pending command changed and clears it.
func MirrorAfter(root string) (*SyncResult, error) {
	m, err := OpenMirror(root)
	if err != nil {
		return nil, err
	}
	l, err := m.lock()
	if err != nil {
		return nil, err
	}
	defer l.release()
	p := m.pending()
	if p == nil {
		return &SyncResult{}, nil
	}
	res, err := m.sync(p, mirrorLimit())
	if err != nil {
		return nil, err
	}
	m.EmptyTrash()
	return res, m.setPending(nil)
}

// FinishPending attributes changes for every pending command, or only for
// those of one session when session is not empty. oops runs it before
// listing or undoing, so a command's snapshot is complete even when no hook
// ran after it.
func FinishPending(session string) []*SyncResult {
	if session == "" {
		defer FinishAfter("*")
	} else {
		defer FinishAfter(session)
	}
	ents, err := os.ReadDir(pendingDir())
	if err != nil {
		return nil
	}
	var out []*SyncResult
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(pendingDir(), e.Name()))
		if err != nil || len(b) == 0 {
			continue
		}
		var rec struct{ Root, Session string }
		if json.Unmarshal(b, &rec) != nil || rec.Root == "" {
			continue
		}
		if session != "" && rec.Session != session {
			continue
		}
		res, err := MirrorAfter(rec.Root)
		if err != nil {
			var tb errTooBig
			if errors.As(err, &tb) || errors.Is(err, os.ErrNotExist) {
				_ = os.Remove(filepath.Join(pendingDir(), e.Name()))
			}
			continue
		}
		out = append(out, res)
	}
	return out
}

// MirrorStatus describes one mirror for oops status.
type MirrorStatus struct {
	Root   string
	Method string
	Files  int
	Synced time.Time
	Bytes  int64 // metadata the mirror itself takes on disk
}

// Mirrors lists the mirrors in the main store.
func Mirrors() []MirrorStatus {
	var out []MirrorStatus
	ents, _ := os.ReadDir(filepath.Join(StoreDir(), "mirrors"))
	for _, e := range ents {
		m := &Mirror{Dir: filepath.Join(StoreDir(), "mirrors", e.Name())}
		mm, err := m.meta()
		if err != nil {
			continue
		}
		var idx int64
		if fi, err := os.Stat(m.indexPath()); err == nil {
			idx = fi.Size()
		}
		out = append(out, MirrorStatus{Root: mm.Root, Method: mm.Method, Files: mm.Files, Synced: mm.Synced, Bytes: idx})
	}
	return out
}

// isOopsCommand reports lines that only run oops itself.
func isOopsCommand(cmd string) bool {
	f := strings.Fields(cmd)
	return len(f) > 0 && (f[0] == "oops" || f[0] == "oops-db" || strings.HasSuffix(f[0], "/oops"))
}
