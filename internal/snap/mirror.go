package snap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// A mirror is a standing copy of a project that costs almost no disk: every
// file is a copy on write clone (APFS, btrfs, XFS, ReFS) or a hard link
// (NTFS, ext4) of the original. Before a command runs, the mirror is brought
// up to date by a quick stat walk. After it runs, the walk is repeated, and
// every mirror entry the command changed or removed is moved into that
// command's snapshot. This catches programs that change files themselves,
// such as make clean, npm scripts and any script, without knowing them.

// Mirror is one project's mirror.
type Mirror struct {
	Root   string
	Dir    string
	Method string // clone, reflink, block clone or hardlink
	inVol  bool   // lives in a volume store, not the main store
}

type mirrorMeta struct {
	Root    string    `json:"root"`
	Method  string    `json:"method"`
	Created time.Time `json:"created"`
	Files   int       `json:"files"`
	Synced  time.Time `json:"synced"`
}

// Pending is a command that ran (or is running) since the mirror was last
// brought up to date. Its changes are attributed to it at the next sync.
type Pending struct {
	Command string    `json:"command"`
	Cwd     string    `json:"cwd"`
	Source  string    `json:"source"`
	Session string    `json:"session,omitempty"`
	SnapID  int       `json:"snap_id,omitempty"` // snapshot the command line layer already made
	Time    time.Time `json:"time"`
}

// ErrNoMirror means this volume supports neither clones nor hard links.
var ErrNoMirror = errors.New("this disk supports neither copy on write clones nor hard links")

func mirrorKey(root string) string {
	h := sha256.Sum256([]byte(pathKey(filepath.Clean(root))))
	return hex.EncodeToString(h[:8])
}

// mirrorParent picks where root's mirror lives: the main store when it is on
// the same volume, else the private store at the root of root's volume.
func mirrorParent(root string) (string, bool) {
	store := StoreDir()
	_ = os.MkdirAll(store, 0o700)
	if SecondStore() != "" || sameDevice(root, store) {
		// A second drive store holds the mirror itself, as real copies.
		return filepath.Join(store, "mirrors"), false
	}
	vr := volumeRoot(root)
	if vr == "" {
		return "", false
	}
	base := volumeBase(vr)
	if base == "" || ensureVolumeBase(base) != nil {
		return "", false
	}
	return filepath.Join(base, "mirrors"), true
}

// OpenMirror opens or creates the mirror for a project root.
func OpenMirror(root string) (*Mirror, error) {
	if method == "copy" {
		return nil, ErrNoMirror
	}
	parent, inVol := mirrorParent(root)
	if parent == "" {
		return nil, ErrNoMirror
	}
	m := &Mirror{Root: root, Dir: filepath.Join(parent, mirrorKey(root)), inVol: inVol}
	if err := os.MkdirAll(filepath.Join(m.Dir, "data"), 0o700); err != nil {
		return nil, err
	}
	meta, err := m.meta()
	if err == nil && meta.Method != "" {
		m.Method = meta.Method
		if m.Method == "hardlink" && runtime.GOOS == "linux" {
			listPrewriteRoot(root)
		}
		return m, nil
	}
	if SecondStore() != "" {
		m.Method = "zstd copy"
	} else {
		m.Method = probeMirrorMethod(m.Dir)
	}
	if m.Method == "" {
		return nil, ErrNoMirror
	}
	if m.Method == "hardlink" && runtime.GOOS == "linux" {
		listPrewriteRoot(root)
	}
	return m, m.saveMeta(mirrorMeta{Root: root, Method: m.Method, Created: time.Now()})
}

func probeMirrorMethod(dir string) string {
	if !forceCopy {
		if k := ProbeClone(dir); k != "copy" {
			return k
		}
	}
	a := filepath.Join(dir, ".probe-link")
	b := a + "2"
	defer os.Remove(a)
	defer os.Remove(b)
	if os.WriteFile(a, []byte("x"), 0o600) != nil || os.Link(a, b) != nil {
		return ""
	}
	return "hardlink"
}

func (m *Mirror) meta() (mirrorMeta, error) {
	var mm mirrorMeta
	b, err := os.ReadFile(filepath.Join(m.Dir, "meta.json"))
	if err != nil {
		return mm, err
	}
	return mm, json.Unmarshal(b, &mm)
}

func (m *Mirror) saveMeta(mm mirrorMeta) error {
	b, _ := json.MarshalIndent(mm, "", "  ")
	return os.WriteFile(filepath.Join(m.Dir, "meta.json"), b, 0o600)
}

func (m *Mirror) dataPath(rel string) string {
	return filepath.Join(m.Dir, "data", filepath.FromSlash(rel))
}

func (m *Mirror) origPath(rel string) string {
	return filepath.Join(m.Root, filepath.FromSlash(rel))
}

func (m *Mirror) indexPath() string { return filepath.Join(m.Dir, "index") }

func (m *Mirror) lock() (*lock, error) { return lockAt(filepath.Join(m.Dir, "lock")) }

// Exists reports whether the mirror has been built.
func (m *Mirror) Exists() bool {
	_, err := os.Stat(m.indexPath())
	return err == nil
}

func (m *Mirror) pending() *Pending {
	b, err := os.ReadFile(filepath.Join(m.Dir, "pending.json"))
	if err != nil || len(b) == 0 {
		return nil
	}
	var p Pending
	if json.Unmarshal(b, &p) != nil {
		return nil
	}
	return &p
}

// setPending records or clears the pending command. The files are kept and
// emptied rather than deleted, since creating files is what costs most on
// Windows, where antivirus scanning looks at every new file.
func (m *Mirror) setPending(p *Pending) error {
	marker := filepath.Join(pendingDir(), mirrorKey(m.Root))
	if p == nil {
		_ = rewrite(marker, nil)
		return rewrite(filepath.Join(m.Dir, "pending.json"), nil)
	}
	b, _ := json.Marshal(p)
	if err := rewrite(filepath.Join(m.Dir, "pending.json"), b); err != nil {
		return err
	}
	rec, _ := json.Marshal(map[string]string{"root": m.Root, "session": p.Session})
	if err := rewrite(marker, rec); err != nil {
		_ = os.MkdirAll(pendingDir(), 0o700)
		return rewrite(marker, rec)
	}
	return nil
}

// rewrite replaces a small file's content in place.
func rewrite(p string, b []byte) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	if err := f.Close(); werr == nil {
		werr = err
	}
	return werr
}

func pendingDir() string { return filepath.Join(Dir(), "pending") }

// put brings one entry of the project into the mirror, replacing what was
// there. It returns false when the entry could not be mirrored.
func (m *Mirror) put(n node) bool {
	src, dst := m.origPath(n.rel), m.dataPath(n.rel)
	switch n.kind {
	case 'd':
		if fi, err := os.Lstat(dst); err == nil && fi.IsDir() {
			return true
		}
		_ = forceRemove(dst)
		return os.MkdirAll(dst, 0o700) == nil
	case 'r':
		_ = forceRemove(dst)
		return true
	}
	tmp := dst + ".oops-new"
	_ = os.Remove(tmp)
	fi, err := os.Lstat(src)
	if err != nil {
		return false
	}
	switch n.kind {
	case 'l':
		err = copyLink(src, tmp, fi)
	default:
		err = m.fileInto(src, tmp, fi)
	}
	if err != nil && errors.Is(err, os.ErrNotExist) && os.MkdirAll(filepath.Dir(dst), 0o700) == nil {
		_ = os.Remove(tmp)
		if n.kind == 'l' {
			err = copyLink(src, tmp, fi)
		} else {
			err = m.fileInto(src, tmp, fi)
		}
	}
	if err != nil {
		_ = os.Remove(tmp)
		return false
	}
	_ = forceRemove(dst)
	return os.Rename(tmp, dst) == nil
}

// fileInto clones or links one file.
func (m *Mirror) fileInto(src, dst string, fi os.FileInfo) error {
	switch m.Method {
	case "hardlink":
		return os.Link(src, dst)
	case "zstd copy":
		if err := zcopyFile(src, dst, fi); err != nil {
			return err
		}
		setMeta(dst, fi, false)
		return nil
	}
	if err := cloneOnly(src, dst, fi); err != nil {
		return err
	}
	setMeta(dst, fi, false) // reflinks and block clones start with fresh times
	return nil
}

// mirrorStats reports what a sync did.
type SyncResult struct {
	Snapshot *Snapshot
	Added    int
	Changed  int
	Removed  int
	Files    int
	Millis   float64
	Notes    []string
}

// trash moves a mirror path out of the way so it can be deleted later,
// away from the hook's critical path.
func (m *Mirror) trash(p string) {
	dir := filepath.Join(m.Dir, "trash")
	_ = os.MkdirAll(dir, 0o700)
	if os.Rename(p, filepath.Join(dir, fmt.Sprint(time.Now().UnixNano()))) != nil {
		_ = forceRemove(p)
	}
}

// EmptyTrash deletes trashed mirror data.
func (m *Mirror) EmptyTrash() { _ = forceRemove(filepath.Join(m.Dir, "trash")) }

// under returns the nodes inside rel (not rel itself) from a sorted list.
func under(nodes []node, rel string) []node {
	prefix := rel + "/"
	lo, hi := 0, len(nodes)
	for lo < hi {
		mid := (lo + hi) / 2
		if nodes[mid].rel < prefix {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	end := lo
	for end < len(nodes) && strings.HasPrefix(nodes[end].rel, prefix) {
		end++
	}
	return nodes[lo:end]
}
