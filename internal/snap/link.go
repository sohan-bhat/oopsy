package snap

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Hard link snapshots share the file's data, so they cost no disk and no
// time. They are safe exactly when the command only unlinks the original:
// the data then lives on through the snapshot's link. Commands that write
// into files (redirects, cp, sed -i, git reset) must never use them, since
// the snapshot would change along with the original.

// linker hard links files into a snapshot, or back out of one on restore.
type linker struct {
	root    string            // tree root, so files have stable relative names
	record  bool              // true when taking a snapshot, false on restore
	attrs   map[string]uint32 // Windows attributes a delete may clear (see fragileAttrs)
	copied  int64             // bytes that had to be copied instead of linked
	nCopied int
}

func (l *linker) file(src, dst string, fi os.FileInfo) error {
	rel := relKey(l.root, src)
	if l.record {
		if a, ok := fragileAttrs(fi); ok {
			l.attrs[rel] = a
		}
	}
	// On restore, a file that was read only is copied rather than linked, so
	// clearing the attribute on one link later cannot change the other.
	mustCopy := !l.record && l.attrs[rel]&readOnlyAttr != 0
	if !mustCopy {
		if err := os.Link(src, dst); err == nil {
			return nil
		}
		// Not ours to link (protected_hardlinks), too many links, or a
		// filesystem without links: copy just this file.
	}
	if err := cloneFile(src, dst, fi); err != nil {
		return err
	}
	l.copied += fi.Size()
	l.nCopied++
	setMeta(dst, fi, false)
	return nil
}

// linkTree snapshots src into dst with hard links. It returns errNoClone
// when hard links cannot work here (dst on another volume).
func linkTree(src, dst string) (*linker, []Skip, error) {
	if method == "copy" || !sameDevice(src, filepath.Dir(dst)) {
		return nil, nil, errNoClone
	}
	l := &linker{root: src, record: true, attrs: map[string]uint32{}}
	c := &treeCopy{file: l.file, skipFileMeta: true}
	err := c.walk(src, dst, true)
	return l, c.skipped, err
}

// relKey names p relative to root with forward slashes, "." for root.
func relKey(root, p string) string {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(r)
}

// pathKey folds case on Windows, where paths are case insensitive.
func pathKey(p string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

// within reports whether p is root or inside it.
func within(p, root string) bool {
	p, root = pathKey(filepath.Clean(p)), pathKey(filepath.Clean(root))
	if p == root {
		return true
	}
	if !strings.HasSuffix(root, string(filepath.Separator)) {
		root += string(filepath.Separator)
	}
	return strings.HasPrefix(p, root)
}

// overlaps reports whether either path contains the other.
func overlaps(a, b string) bool { return within(a, b) || within(b, a) }
