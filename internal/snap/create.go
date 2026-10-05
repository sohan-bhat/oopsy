package snap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sohan-bhat/oopsy/internal/classify"
)

// Create snapshots everything the actions are about to destroy. It returns
// nil and no error when there turned out to be nothing worth saving.
func Create(command, cwd, source string, actions []classify.Action) (*Snapshot, error) {
	start := time.Now()
	cfg := LoadConfig()
	l, err := acquire()
	if err != nil {
		return nil, err
	}
	defer l.release()

	id, err := nextID()
	if err != nil {
		return nil, err
	}
	s := &Snapshot{ID: id, Time: start, Command: command, Cwd: cwd, Source: source}
	if err := os.MkdirAll(filepath.Join(s.Dir(), "files"), 0o700); err != nil {
		return nil, err
	}

	var paths []string
	gitOwned := map[string]string{}
	seenProg := map[string]bool{}
	for _, a := range actions {
		if !seenProg[a.Program] {
			seenProg[a.Program] = true
			s.Programs = append(s.Programs, a.Program)
		}
		if a.Broad {
			s.Broad = true
		}
		if a.Note != "" && len(a.Paths) == 0 && a.Git == nil {
			s.Notes = append(s.Notes, a.Note)
		}
		paths = append(paths, a.Paths...)
		if a.Git != nil {
			recs, gp, note := gitPrepare(a.Git)
			s.Git = append(s.Git, recs...)
			paths = append(paths, gp...)
			if a.Git.Kind == "reset-hard" || a.Git.Kind == "checkout-paths" {
				for _, p := range gp {
					gitOwned[filepath.Clean(p)] = a.Git.Repo
				}
			}
			if note != "" {
				s.Notes = append(s.Notes, note)
			}
		}
	}

	budget := cfg.CopyCapBytes
	for i, p := range minimalPaths(paths) {
		fi, err := os.Lstat(p)
		if err != nil {
			continue // nothing there to lose
		}
		if reason := refuse(p); reason != "" {
			s.Skipped = append(s.Skipped, Skip{Path: p, Reason: reason})
			continue
		}
		e := Entry{Orig: p, Store: filepath.Join("files", fmt.Sprint(i)), GitRepo: gitOwned[p]}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			e.Type = "symlink"
		case fi.IsDir():
			e.Type = "dir"
		default:
			e.Type = "file"
		}
		if pfi, err := os.Stat(filepath.Dir(p)); err == nil {
			e.ParentMtime = pfi.ModTime().UnixNano()
		}
		dst := filepath.Join(s.Dir(), e.Store)
		skipped, err := cloneTree(p, dst)
		if err == nil {
			s.Skipped = append(s.Skipped, skipped...)
			e.Method = cloneMethod
			e.Size, e.Files, _ = treeSize(dst)
		} else if errors.Is(err, errNoClone) {
			size, n, _ := treeSize(p)
			if size > budget {
				s.Skipped = append(s.Skipped, Skip{Path: p, Reason: fmt.Sprintf(
					"this filesystem has no copy on write clones and %s is over the copy limit of %s (raise copy_cap_bytes in %s)",
					human(size), human(budget), ConfigPath())})
				continue
			}
			skipped, err := copyTree(p, dst, cloneFile)
			if err != nil {
				s.Skipped = append(s.Skipped, Skip{Path: p, Reason: "copy failed: " + skipReason(err)})
				_ = forceRemove(dst)
				continue
			}
			s.Skipped = append(s.Skipped, skipped...)
			budget -= size
			e.Method, e.Size, e.Files = "copy", size, n
		} else {
			s.Skipped = append(s.Skipped, Skip{Path: p, Reason: err.Error()})
			_ = forceRemove(dst)
			continue
		}
		s.Entries = append(s.Entries, e)
		s.Size += e.Size
		s.Files += e.Files
	}

	if len(s.Entries) == 0 && len(s.Git) == 0 && len(s.Skipped) == 0 && len(s.Notes) == 0 {
		_ = forceRemove(s.Dir())
		return nil, nil
	}
	s.Millis = float64(time.Since(start).Microseconds()) / 1000
	if err := s.save(); err != nil {
		return nil, err
	}
	appendJournal(s)
	l.release()
	_, _ = Prune(cfg, false)
	return s, nil
}

// refuse returns a reason when a path is not something we should clone.
func refuse(p string) string {
	home, _ := os.UserHomeDir()
	oops := Dir()
	if p == oops || strings.HasPrefix(p, oops+"/") {
		return "this is Oopsy's own storage"
	}
	if p == "/" || p == home || strings.HasPrefix(oops, p+"/") {
		return "too broad to snapshot (it contains the whole home directory or the snapshot store)"
	}
	return ""
}

// minimalPaths dedupes paths and drops any path inside another one.
func minimalPaths(paths []string) []string {
	clean := map[string]bool{}
	for _, p := range paths {
		if p != "" {
			clean[filepath.Clean(p)] = true
		}
	}
	list := make([]string, 0, len(clean))
	for p := range clean {
		list = append(list, p)
	}
	sort.Strings(list)
	var out []string
	for _, p := range list {
		covered := false
		for _, q := range out {
			if q == "/" || strings.HasPrefix(p, q+"/") {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, p)
		}
	}
	return out
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func git(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), errors.New(msg)
	}
	return out.String(), nil
}

func gitLine(dir string, args ...string) string {
	out, err := git(dir, args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// gitPrepare records git state for op and returns paths that must be saved.
func gitPrepare(op *classify.GitOp) ([]GitRecord, []string, string) {
	top := gitLine(op.Repo, "rev-parse", "--show-toplevel")
	if top == "" {
		return nil, nil, ""
	}
	head := gitLine(top, "rev-parse", "--verify", "-q", "HEAD")
	switch op.Kind {
	case "reset-hard":
		return []GitRecord{{Kind: op.Kind, Repo: top, Head: head}}, changedPaths(top, top, nil), ""
	case "checkout-paths":
		return nil, changedPaths(top, op.Repo, op.Paths), ""
	case "clean":
		return nil, cleanPaths(op), ""
	case "branch-delete":
		var recs []GitRecord
		for _, b := range op.Branches {
			ref := "refs/heads/" + b
			if op.Remote == "remote-tracking" {
				ref = "refs/remotes/" + b
			}
			if sha := gitLine(top, "rev-parse", "--verify", "-q", ref); sha != "" {
				recs = append(recs, GitRecord{Kind: op.Kind, Repo: top, Ref: ref, SHA: sha})
			}
		}
		return recs, nil, ""
	case "stash-drop":
		var recs []GitRecord
		for _, r := range op.StashRefs {
			if sha := gitLine(top, "rev-parse", "--verify", "-q", r); sha != "" {
				msg := gitLine(top, "show", "-s", "--format=%s", sha)
				recs = append(recs, GitRecord{Kind: "stash", Repo: top, Ref: r, SHA: sha, Message: msg})
			}
		}
		return recs, nil, ""
	case "stash-clear":
		var recs []GitRecord
		out := gitLine(top, "stash", "list", "--format=%H%x09%s")
		for _, line := range strings.Split(out, "\n") {
			sha, msg, ok := strings.Cut(line, "\t")
			if ok {
				recs = append(recs, GitRecord{Kind: "stash", Repo: top, SHA: sha, Message: msg})
			}
		}
		return recs, nil, ""
	case "push-force":
		return pushRecords(top, op), nil, ""
	}
	return nil, nil, ""
}

// changedPaths lists tracked files with uncommitted changes, which a reset or
// checkout would overwrite. pathspecs are relative to dir.
func changedPaths(top, dir string, pathspecs []string) []string {
	args := []string{"status", "--porcelain=v1", "-z", "--untracked-files=no", "--ignore-submodules"}
	if len(pathspecs) > 0 {
		args = append(args, "--")
		args = append(args, pathspecs...)
	}
	out, err := git(dir, args...)
	if err != nil {
		return nil
	}
	var paths []string
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 {
			continue
		}
		xy, p := f[:2], f[3:]
		if xy[0] == 'R' || xy[0] == 'C' {
			i++ // the next field is the original name
		}
		paths = append(paths, filepath.Join(top, p))
	}
	return paths
}

func cleanPaths(op *classify.GitOp) []string {
	args := []string{"clean", "-n"}
	for _, a := range op.Args {
		switch {
		case a == "-f" || a == "--force" || a == "-i" || a == "--interactive" || a == "-q" || a == "--quiet":
			continue
		case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && a != "-e":
			kept := strings.Map(func(r rune) rune {
				if r == 'f' || r == 'i' || r == 'q' {
					return -1
				}
				return r
			}, a[1:])
			if kept != "" {
				args = append(args, "-"+kept)
			}
			continue
		}
		args = append(args, a)
	}
	out, err := git(op.Repo, args...)
	if err != nil {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		p, ok := strings.CutPrefix(line, "Would remove ")
		if !ok {
			continue
		}
		paths = append(paths, filepath.Join(op.Repo, strings.TrimSuffix(p, "/")))
	}
	return paths
}

func pushRecords(top string, op *classify.GitOp) []GitRecord {
	remote := op.Remote
	if remote == "" {
		up := gitLine(top, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
		if r, _, ok := strings.Cut(up, "/"); ok {
			remote = r
		} else {
			remote = "origin"
		}
	}
	current := gitLine(top, "symbolic-ref", "--short", "-q", "HEAD")
	var branches []string
	all := false
	for _, a := range op.Args {
		if a == "--mirror" || a == "--all" {
			all = true
		}
	}
	if all {
		out := gitLine(top, "for-each-ref", "--format=%(refname)", "refs/remotes/"+remote+"/")
		for _, r := range strings.Split(out, "\n") {
			if b := strings.TrimPrefix(r, "refs/remotes/"+remote+"/"); b != "" && b != r && b != "HEAD" {
				branches = append(branches, b)
			}
		}
	}
	for _, sp := range op.Refspecs {
		sp = strings.TrimPrefix(sp, "+")
		dst := sp
		if i := strings.LastIndex(sp, ":"); i >= 0 {
			dst = sp[i+1:]
		}
		if dst == "" || dst == "HEAD" {
			dst = current
		}
		branches = append(branches, strings.TrimPrefix(dst, "refs/heads/"))
	}
	if len(branches) == 0 && current != "" {
		branches = []string{current}
	}
	var recs []GitRecord
	for _, b := range branches {
		ref := "refs/remotes/" + remote + "/" + b
		if sha := gitLine(top, "rev-parse", "--verify", "-q", ref); sha != "" {
			recs = append(recs, GitRecord{Kind: "push", Repo: top, Remote: remote, Ref: b, SHA: sha})
		}
	}
	return recs
}
