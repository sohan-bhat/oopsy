package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sohan-bhat/oopsy/internal/classify"
	"github.com/sohan-bhat/oopsy/internal/snap"
	"github.com/sohan-bhat/oopsy/internal/ui"
)

// The generic layer protects files from any command, not just the ones the
// command line parser understands. Before a command that is not provably
// read only, the project mirror is brought up to date; after it, whatever
// the command changed or removed becomes its snapshot.

// readOnlyFor reports lines that cannot change files.
func readOnlyFor(source, tool, cmd string) bool {
	switch source {
	case "zsh", "bash", "fish", "shell":
		return classify.ReadOnly(cmd)
	case "powershell", "cmd":
		return classify.QuickReadOnlyWindows(cmd)
	}
	if strings.EqualFold(tool, "PowerShell") || runtime.GOOS == "windows" {
		return classify.ReadOnly(cmd) || classify.QuickReadOnlyWindows(cmd)
	}
	return classify.ReadOnly(cmd)
}

// beforeCommand syncs the mirrors this command may touch and marks it
// pending. Errors never stop the command.
func beforeCommand(source, cmd, cwd, session string, s *snap.Snapshot, stderr io.Writer) {
	if os.Getenv("OOPS_NO_MIRROR") != "" {
		return
	}
	id := 0
	if s != nil {
		id = s.ID
	}
	for _, root := range scopes(cmd, cwd) {
		res, err := snap.MirrorBefore(root, snap.Pending{Command: cmd, Cwd: cwd, Source: source, Session: session, SnapID: id})
		var building snap.ErrBuilding
		switch {
		case errors.As(err, &building):
			if startBackground("mirror", "build", root) && shellSources[source] {
				fmt.Fprintf(stderr, "%s setting up undo for every command in %s (%d files) in the background. Until it is done, only commands Oopsy recognizes are covered.\n",
					ui.ErrDim("oops:"), ui.HomeRel(root), building.Files)
			}
		case err != nil:
			// No clones or hard links here, or too many files: the command
			// line layer still covers what it recognizes.
		case res != nil && res.Snapshot != nil && shellSources[source] && !snap.LoadConfig().Quiet:
			fmt.Fprintln(stderr, ui.ErrDim(Summary(res.Snapshot)+" (from the command before)"))
		}
	}
}

// scopes are the roots the generic layer mirrors for a command: the
// project around cwd, plus existing folders the command names outside it.
func scopes(cmd, cwd string) []string {
	var out []string
	root := snap.MirrorRoot(cwd)
	if root != "" {
		out = append(out, root)
	}
	for _, w := range classify.Words(cmd, cwd) {
		if w == "" || strings.HasPrefix(w, "-") {
			continue
		}
		p := classify.AbsWord(w, cwd, home())
		fi, err := os.Stat(p)
		if err != nil || !fi.IsDir() {
			continue
		}
		r := snap.MirrorRoot(p)
		if r == "" {
			continue
		}
		dup := false
		for _, o := range out {
			if within(r, o) || within(o, r) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, r)
		}
		if len(out) >= 4 {
			break
		}
	}
	return out
}

func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// afterCommand attributes what the finished command changed.
func afterCommand(source, session string, stdout, stderr io.Writer) {
	for _, res := range snap.FinishPending(session) {
		if res == nil || res.Snapshot == nil {
			continue
		}
		msg := Summary(res.Snapshot)
		switch source {
		case "claude", "codex":
			reply, _ := json.Marshal(map[string]any{"systemMessage": msg})
			fmt.Fprintln(stdout, string(reply))
			replied = true
		case "zsh", "bash", "fish", "shell", "powershell", "cmd":
			if os.Getenv("OOPSY_POST_QUIET") == "" && !snap.LoadConfig().Quiet {
				fmt.Fprintln(stderr, ui.ErrDim(msg))
			}
		default:
			fmt.Fprintln(stderr, msg)
		}
	}
}

// startBackground runs oops with args detached from this process.
func startBackground(args ...string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	c := exec.Command(self, args...)
	c.Stdin, c.Stdout, c.Stderr = nil, nil, nil
	detach(c)
	if err := c.Start(); err != nil {
		return false
	}
	_ = c.Process.Release()
	return true
}
