package snap

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Outcome is one line of a restore report.
type Outcome struct {
	Path   string
	Status string // restored, kept-both, same, merged, git, failed
	Detail string
}

// PlanLine describes something a restore will bring back.
type PlanLine struct {
	Path   string
	What   string
	Exists bool
}

// Plan lists what restoring s would do, without changing anything.
func Plan(s *Snapshot) []PlanLine {
	var out []PlanLine
	for _, e := range s.Entries {
		_, err := os.Lstat(e.Orig)
		what := e.Type
		if e.Type == "dir" {
			what = fmt.Sprintf("folder, %d items, %s", e.Files-1, human(e.Size))
		} else if e.Type == "file" {
			what = "file, " + human(e.Size)
		}
		out = append(out, PlanLine{Path: e.Orig, What: what, Exists: err == nil})
	}
	for _, g := range s.Git {
		out = append(out, PlanLine{Path: g.Repo, What: gitPlan(g)})
	}
	return out
}

func short(sha string) string {
	if len(sha) > 9 {
		return sha[:9]
	}
	return sha
}

func gitPlan(g GitRecord) string {
	switch g.Kind {
	case "reset-hard":
		return "move HEAD back to " + short(g.Head) + " if it was moved by the reset"
	case "branch-delete":
		return "recreate branch " + strings.TrimPrefix(strings.TrimPrefix(g.Ref, "refs/heads/"), "refs/remotes/") + " at " + short(g.SHA)
	case "stash":
		return "bring back stash " + short(g.SHA) + " (" + g.Message + ")"
	case "push":
		return "force push " + g.Remote + "/" + g.Ref + " back to " + short(g.SHA)
	}
	return g.Kind
}

// Restore brings snapshot s back to its original locations. Paths that exist
// again are never overwritten: the saved copy is placed next to them.
func Restore(s *Snapshot) ([]Outcome, error) {
	var out []Outcome
	for _, e := range s.Entries {
		src := filepath.Join(s.Dir(), e.Store)
		if _, err := os.Lstat(src); err != nil {
			out = append(out, Outcome{Path: e.Orig, Status: "failed", Detail: "snapshot data is missing"})
			continue
		}
		parent := filepath.Dir(e.Orig)
		_, perr := os.Stat(parent)
		if err := os.MkdirAll(parent, 0o755); err != nil {
			out = append(out, Outcome{Path: e.Orig, Status: "failed", Detail: err.Error()})
			continue
		}
		if e.GitRepo != "" && matchesHead(e.GitRepo, e.Orig) {
			// git put the committed version here; it is still in git, so the
			// saved uncommitted version can safely take its place.
			if err := os.Remove(e.Orig); err == nil {
				if err := place(src, e.Orig); err != nil {
					out = append(out, Outcome{Path: e.Orig, Status: "failed", Detail: err.Error()})
				} else {
					out = append(out, Outcome{Path: e.Orig, Status: "restored"})
				}
				continue
			}
		}
		out = append(out, restorePath(src, e.Orig, s.ID)...)
		if perr == nil && e.ParentMtime != 0 {
			setMtime(parent, e.ParentMtime)
		}
	}
	for _, g := range s.Git {
		out = append(out, restoreGit(s, g))
	}
	now := time.Now()
	s.Restored = &now
	return out, s.save()
}

// RestoreTo copies the snapshot's files into dir instead of their original place.
func RestoreTo(s *Snapshot, dir string) ([]Outcome, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var out []Outcome
	for _, e := range s.Entries {
		rel := strings.TrimPrefix(e.Orig, "/")
		if r, err := filepath.Rel(s.Cwd, e.Orig); err == nil && !strings.HasPrefix(r, "..") && r != "." {
			rel = r
		} else if r == "." {
			rel = filepath.Base(e.Orig)
		}
		dst := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			out = append(out, Outcome{Path: dst, Status: "failed", Detail: err.Error()})
			continue
		}
		out = append(out, restorePath(filepath.Join(s.Dir(), e.Store), dst, s.ID)...)
	}
	return out, nil
}

// restorePath puts src at dst. When dst exists, directories are merged and
// differing files are kept side by side.
func restorePath(src, dst string, id int) []Outcome {
	dfi, err := os.Lstat(dst)
	if errors.Is(err, fs.ErrNotExist) {
		if err := place(src, dst); err != nil {
			return []Outcome{{Path: dst, Status: "failed", Detail: err.Error()}}
		}
		return []Outcome{{Path: dst, Status: "restored"}}
	}
	if err != nil {
		return []Outcome{{Path: dst, Status: "failed", Detail: err.Error()}}
	}
	sfi, err := os.Lstat(src)
	if err != nil {
		return []Outcome{{Path: dst, Status: "failed", Detail: err.Error()}}
	}
	if sfi.IsDir() && dfi.IsDir() {
		ents, err := os.ReadDir(src)
		if err != nil {
			return []Outcome{{Path: dst, Status: "failed", Detail: err.Error()}}
		}
		var out []Outcome
		for _, e := range ents {
			out = append(out, restorePath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), id)...)
		}
		return out
	}
	if sameContent(src, dst, sfi, dfi) {
		return []Outcome{{Path: dst, Status: "same"}}
	}
	alt := altName(dst, id)
	if err := place(src, alt); err != nil {
		return []Outcome{{Path: dst, Status: "failed", Detail: err.Error()}}
	}
	return []Outcome{{Path: dst, Status: "kept-both", Detail: alt}}
}

func altName(dst string, id int) string {
	ext := filepath.Ext(dst)
	base := strings.TrimSuffix(dst, ext)
	if ext == "" || strings.Contains(ext, "/") {
		base, ext = dst, ""
	}
	alt := fmt.Sprintf("%s.oops-%d%s", base, id, ext)
	for i := 2; ; i++ {
		if _, err := os.Lstat(alt); errors.Is(err, fs.ErrNotExist) {
			return alt
		}
		alt = fmt.Sprintf("%s.oops-%d-%d%s", base, id, i, ext)
	}
}

// place clones src to dst, falling back to a copy.
func place(src, dst string) error {
	err := cloneTree(src, dst)
	if errors.Is(err, errNoClone) {
		return copyTree(src, dst, cloneFile)
	}
	return err
}

func sameContent(a, b string, afi, bfi os.FileInfo) bool {
	if afi.Mode().Type() != bfi.Mode().Type() {
		return false
	}
	if afi.Mode()&os.ModeSymlink != 0 {
		x, _ := os.Readlink(a)
		y, _ := os.Readlink(b)
		return x == y
	}
	if !afi.Mode().IsRegular() || afi.Size() != bfi.Size() {
		return false
	}
	fa, err := os.Open(a)
	if err != nil {
		return false
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false
	}
	defer fb.Close()
	ba := make([]byte, 64<<10)
	bb := make([]byte, 64<<10)
	for {
		na, ea := io.ReadFull(fa, ba)
		nb, eb := io.ReadFull(fb, bb)
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false
		}
		if ea != nil || eb != nil {
			return ea != nil && eb != nil
		}
	}
}

// matchesHead reports whether path exists and is identical to the version
// committed at HEAD, meaning git can always give it back.
func matchesHead(repo, path string) bool {
	if _, err := os.Lstat(path); err != nil {
		return false
	}
	top := gitLine(repo, "rev-parse", "--show-toplevel")
	if top == "" {
		return false
	}
	rel, err := filepath.Rel(top, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return false
	}
	if gitLine(top, "ls-files", "--", rel) == "" {
		return false
	}
	_, err = git(top, "diff", "--quiet", "HEAD", "--", rel)
	return err == nil
}

func restoreGit(s *Snapshot, g GitRecord) Outcome {
	o := Outcome{Path: g.Repo, Status: "git"}
	switch g.Kind {
	case "reset-hard":
		cur := gitLine(g.Repo, "rev-parse", "--verify", "-q", "HEAD")
		if g.Head == "" || cur == g.Head {
			o.Detail = "HEAD unchanged"
			return o
		}
		last := gitLine(g.Repo, "reflog", "-1", "--format=%gs")
		if !strings.HasPrefix(last, "reset: moving to") {
			o.Detail = fmt.Sprintf("HEAD moved since the reset; previous HEAD was %s (run git reset --soft %s to go back)", short(g.Head), short(g.Head))
			return o
		}
		if _, err := git(g.Repo, "reset", "-q", "--soft", g.Head); err != nil {
			o.Status, o.Detail = "failed", err.Error()
			return o
		}
		o.Detail = "HEAD moved back to " + short(g.Head)
	case "branch-delete":
		name := strings.TrimPrefix(g.Ref, "refs/heads/")
		ref := g.Ref
		if cur := gitLine(g.Repo, "rev-parse", "--verify", "-q", ref); cur != "" {
			if cur == g.SHA {
				o.Detail = "branch " + name + " already exists at " + short(g.SHA)
				return o
			}
			name = fmt.Sprintf("%s-oops-%d", name, s.ID)
			ref = "refs/heads/" + name
		}
		if _, err := git(g.Repo, "update-ref", ref, g.SHA); err != nil {
			o.Status, o.Detail = "failed", err.Error()
			return o
		}
		o.Detail = "branch " + strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/remotes/") + " recreated at " + short(g.SHA)
	case "stash":
		list := gitLine(g.Repo, "stash", "list", "--format=%H")
		if strings.Contains(list, g.SHA) {
			o.Detail = "stash " + short(g.SHA) + " is still there"
			return o
		}
		if _, err := git(g.Repo, "stash", "store", "-m", g.Message, g.SHA); err != nil {
			o.Status, o.Detail = "failed", err.Error()
			return o
		}
		o.Detail = "stash " + short(g.SHA) + " is back"
	case "push":
		if _, err := git(g.Repo, "push", "--force-with-lease", g.Remote, g.SHA+":refs/heads/"+g.Ref); err != nil {
			o.Status, o.Detail = "failed", err.Error()
			return o
		}
		o.Detail = g.Remote + "/" + g.Ref + " is back at " + short(g.SHA)
	}
	return o
}
