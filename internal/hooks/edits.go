package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sohan-bhat/oopsy/internal/snap"
	"github.com/sohan-bhat/oopsy/internal/ui"
)

// Agent file edit tools are covered file by file: right before the tool
// writes, each target file is stored in the content addressed object store,
// and right after it, the snapshot records what the edit left, so undo swaps
// the file back while it still matches. No mirror sync runs for an edit.

// editInput is the part of an agent payload that names an edit tool's files.
type editInput struct {
	HookEventName string `json:"hook_event_name"`
	ToolName      string `json:"tool_name"`
	ToolUseID     string `json:"tool_use_id"`
	FilePath      string `json:"file_path"` // Cursor afterFileEdit
	Edits         []edit `json:"edits"`     // Cursor afterFileEdit
	ToolInput     struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		AbsolutePath string `json:"absolute_path"`
		Path         string `json:"path"`
		Command      string `json:"command"`
		Input        string `json:"input"`
		Patch        string `json:"patch"`
	} `json:"tool_input"`
}

type edit struct {
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

// editTools are the file writing tools of each agent.
var editTools = map[string]map[string]bool{
	"claude": {"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true},
	"codex":  {"apply_patch": true},
	"gemini": {"write_file": true, "replace": true},
}

// runEdit handles the call when it is about an edit tool, and reports
// whether it did. It never fails the tool.
func runEdit(source string, post bool, payload []byte, cwd, session string, stderr io.Writer) bool {
	var in editInput
	if json.Unmarshal(payload, &in) != nil {
		return false
	}
	if source == "cursor" && (in.HookEventName == "afterFileEdit" || (in.ToolName == "" && in.FilePath != "")) {
		cursorAfterEdit(in, cwd, session, stderr)
		return true
	}
	if !editTools[source][in.ToolName] {
		return false
	}
	paths := editPaths(source, in, cwd)
	key := editKey(source, session, in.ToolUseID, in.ToolName, paths)
	if post {
		finishEdit(key)
		return true
	}
	if len(paths) == 0 {
		return true
	}
	s, err := snap.SaveVersions(editLabel(in.ToolName, paths, cwd), cwd, source, paths)
	if err != nil {
		fmt.Fprintf(stderr, "%s could not save the file before the edit: %v\n", ui.ErrWarn("oops:"), err)
		return true
	}
	if s != nil {
		rememberEdit(key, s.ID)
	}
	return true
}

// editPaths lists the absolute paths an edit tool call writes.
func editPaths(source string, in editInput, cwd string) []string {
	var raw []string
	if source == "codex" {
		patch := in.ToolInput.Command
		if patch == "" {
			patch = in.ToolInput.Input
		}
		if patch == "" {
			patch = in.ToolInput.Patch
		}
		raw = patchPaths(patch)
	} else {
		for _, p := range []string{in.ToolInput.FilePath, in.ToolInput.NotebookPath, in.ToolInput.AbsolutePath, in.ToolInput.Path} {
			if p != "" {
				raw = append(raw, p)
				break
			}
		}
	}
	var out []string
	for _, p := range raw {
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		out = append(out, filepath.Clean(p))
	}
	return out
}

// patchPaths reads the files an apply_patch patch updates, deletes, adds
// or moves onto.
func patchPaths(patch string) []string {
	var out []string
	for _, line := range strings.Split(patch, "\n") {
		line = strings.TrimRight(line, "\r")
		for _, prefix := range []string{"*** Update File: ", "*** Delete File: ", "*** Add File: ", "*** Move to: "} {
			if p, ok := strings.CutPrefix(line, prefix); ok && strings.TrimSpace(p) != "" {
				out = append(out, strings.TrimSpace(p))
			}
		}
	}
	return out
}

// editLabel is the short command shown in oops list, such as Edit src/app.ts.
func editLabel(tool string, paths []string, cwd string) string {
	names := make([]string, 0, len(paths))
	for _, p := range paths {
		if rel, err := filepath.Rel(cwd, p); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			names = append(names, filepath.ToSlash(rel))
		} else {
			names = append(names, ui.HomeRel(p))
		}
	}
	if len(names) > 3 {
		names = append(names[:3], fmt.Sprintf("and %d more", len(names)-3))
	}
	return tool + " " + strings.Join(names, ", ")
}

// The pre hook remembers its snapshot so the post hook of the same tool
// call can find it. Calls are told apart by the agent's tool use id, or by
// session, tool and files when the agent sends no id.

func editsDir() string { return filepath.Join(snap.Dir(), "edits") }

func editKey(source, session, toolUseID, tool string, paths []string) string {
	parts := []string{source, session}
	if toolUseID != "" {
		parts = append(parts, toolUseID)
	} else {
		sorted := append([]string(nil), paths...)
		sort.Strings(sorted)
		parts = append(append(parts, tool), sorted...)
	}
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:12])
}

func rememberEdit(key string, id int) {
	dir := editsDir()
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, key), []byte(strconv.Itoa(id)), 0o600)
	// Drop records whose post hook never came, such as a denied tool call.
	ents, _ := os.ReadDir(dir)
	if len(ents) < 50 {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && fi.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// finishEdit records what the edit left in the snapshot its pre hook made.
func finishEdit(key string) {
	p := filepath.Join(editsDir(), key)
	b, err := os.ReadFile(p)
	if err != nil {
		return // nothing was saved, such as Write creating a new file
	}
	_ = os.Remove(p)
	id, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || snap.SetAfter(id) != nil {
		return
	}
	// A tool call that failed left every file as it was: drop the snapshot
	// so oops undoes the last real change instead.
	s, err := snap.Load(id)
	if err != nil {
		return
	}
	for _, e := range s.Entries {
		if e.After == nil || e.After.Size != e.Size || e.After.Mtime != e.Mtime {
			return
		}
	}
	_ = snap.Remove(id)
}
