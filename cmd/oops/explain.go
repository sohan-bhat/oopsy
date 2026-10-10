package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sohan-bhat/oopsy/internal/classify"
	"github.com/sohan-bhat/oopsy/internal/hooks"
	"github.com/sohan-bhat/oopsy/internal/snap"
	"github.com/sohan-bhat/oopsy/internal/ui"
)

// explainLine prints a plain verdict for a command line: what Oopsy would
// save before it runs, and why.
func explainLine(w io.Writer, line, cwd string) {
	if strings.TrimSpace(line) == "" {
		fmt.Fprintln(w, "There is no command to check.")
		return
	}
	if classify.ReadOnly(line) {
		fmt.Fprintf(w, "%s %s only reads files, so Oopsy lets it run with no snapshot and no cost.\n", ui.Good("Read only."), ui.Bold(line))
		return
	}
	if why := deletesNothing(line); why != "" {
		fmt.Fprintf(w, "%s %s\n", ui.Good("Harmless."), why)
		return
	}
	res := classify.Classify(line, cwd, os.Environ())
	lazy := hooks.LazyTargets(line, cwd)
	if res.Destructive() || len(lazy) > 0 {
		fmt.Fprintln(w, ui.Warn("Destructive.")+" Before it runs, Oopsy saves:")
		for _, a := range res.Actions {
			for _, l := range describeAction(a, line) {
				fmt.Fprintf(w, "  %s\n", l)
			}
		}
		for _, p := range lazy {
			if namedBy(res.Actions, p) {
				continue
			}
			fmt.Fprintf(w, "  %s, whole, since the command or its script names it (rebuildable, kept one day)\n", ui.HomeRel(p))
		}
	}
	root := snap.MirrorRoot(cwd)
	switch {
	case root == "":
		fmt.Fprintf(w, "%s is too broad to mirror (a home folder, a disk root or a system folder), so only what the command names is saved.\n", ui.HomeRel(cwd))
	case res.Destructive():
		fmt.Fprintf(w, "Anything else it changes in %s is caught by the project mirror too.\n", ui.HomeRel(root))
	default:
		fmt.Fprintf(w, "%s Oopsy does not know what %s does, so it relies on the project mirror of %s: whatever the command changes or deletes there is saved as it happens, and %s brings it back.\n",
			ui.Warn("Unknown command."), ui.Bold(firstWord(line)), ui.HomeRel(root), ui.Bold("oops"))
	}
}

// namedBy reports a path the classifier already saves.
func namedBy(actions []classify.Action, p string) bool {
	for _, a := range actions {
		for _, q := range a.Paths {
			if q == p {
				return true
			}
		}
	}
	return false
}

func firstWord(line string) string {
	f := strings.Fields(line)
	if len(f) == 0 {
		return line
	}
	return f[0]
}

// deletesNothing explains delete commands with nothing to delete.
func deletesNothing(line string) string {
	words := classify.Words(line, "")
	if len(words) == 0 {
		return ""
	}
	prog := strings.ToLower(filepath.Base(words[0]))
	switch prog {
	case "rm", "rmdir", "unlink", "shred", "srm", "del", "erase", "rd", "remove-item", "ri":
	default:
		return ""
	}
	for _, w := range words[1:] {
		if !strings.HasPrefix(w, "-") && !(strings.HasPrefix(w, "/") && len(w) <= 3 && prog != "rm") {
			return ""
		}
	}
	return fmt.Sprintf("%s with no path deletes nothing, so there is nothing to save.", strings.Join(words, " "))
}

// describeAction says in words what one classifier action protects.
func describeAction(a classify.Action, line string) []string {
	if g := a.Git; g != nil {
		repo := ui.HomeRel(g.Repo)
		cmd := "git " + strings.TrimPrefix(firstGit(line), "git ")
		switch g.Kind {
		case "reset-hard":
			return []string{fmt.Sprintf("your branch position and uncommitted changes before %s in %s", cmd, repo)}
		case "checkout-paths":
			return []string{fmt.Sprintf("uncommitted changes to %s that %s would overwrite, in %s", strings.Join(g.Paths, " "), cmd, repo)}
		case "clean":
			return []string{fmt.Sprintf("the untracked files %s would delete in %s", cmd, repo)}
		case "branch-delete":
			return []string{fmt.Sprintf("branch %s and the commit it points to, in %s", strings.Join(g.Branches, ", "), repo)}
		case "stash-drop":
			return []string{fmt.Sprintf("the stash %s would drop, in %s", cmd, repo)}
		case "stash-clear":
			return []string{fmt.Sprintf("every stash in %s", repo)}
		case "push-force":
			return []string{fmt.Sprintf("where the remote branch pointed before the force push from %s, so it can be pushed back", repo)}
		}
		return []string{fmt.Sprintf("git state in %s", repo)}
	}
	verb := "about to be overwritten"
	if a.Delete {
		verb = "about to be deleted"
	}
	paths := append([]string{}, a.Paths...)
	sort.Strings(paths)
	var out []string
	for i, p := range paths {
		if i == 20 {
			out = append(out, ui.Dim(fmt.Sprintf("and %d more", len(paths)-20)))
			break
		}
		what := "missing now, nothing to save"
		if fi, err := os.Lstat(p); err == nil {
			switch {
			case fi.IsDir():
				what = "folder"
			case fi.Mode()&os.ModeSymlink != 0:
				what = "link"
			default:
				what = "file, " + ui.Bytes(fi.Size())
			}
		}
		out = append(out, fmt.Sprintf("%s (%s, %s)", ui.HomeRel(p), what, verb))
	}
	if a.Broad && a.Note != "" {
		out = append(out, ui.Dim(a.Note))
	}
	return out
}

func firstGit(line string) string {
	i := strings.Index(line, "git ")
	if i < 0 {
		return line
	}
	rest := line[i:]
	for _, sep := range []string{"&&", "||", ";", "|"} {
		if j := strings.Index(rest, sep); j >= 0 {
			rest = rest[:j]
		}
	}
	return strings.TrimSpace(rest)
}
