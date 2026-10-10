// Package hooks installs and removes Oopsy's shell and agent hooks, and
// handles the hook calls themselves.
package hooks

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sohan-bhat/oopsy/internal/snap"
)

const (
	blockStart = "# >>> oopsy >>>"
	blockEnd   = "# <<< oopsy <<<"
)

// Target is one place a hook can be installed.
type Target struct {
	Name      string // zsh, bash, fish, claude, codex, cursor, gemini
	Label     string
	Kind      string // shell or agent
	Path      string
	Detected  bool
	Installed bool
	Note      string
}

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Bin returns how hooks should call oops: the bare name when it is on PATH
// and resolves to this binary, otherwise an absolute path.
func Bin() string {
	self, err := os.Executable()
	if err != nil {
		return "oops"
	}
	selfReal, _ := filepath.EvalSymlinks(self)
	if p, err := exec.LookPath("oops"); err == nil {
		if real, _ := filepath.EvalSymlinks(p); real == selfReal {
			return "oops"
		}
	}
	if selfReal != "" {
		return selfReal
	}
	return self
}

// Targets lists every supported hook location and whether it is installed.
func Targets() []Target {
	h := home()
	userShell := filepath.Base(os.Getenv("SHELL"))
	ts := []Target{
		{Name: "zsh", Label: "zsh", Kind: "shell", Path: filepath.Join(h, ".zshrc")},
		{Name: "bash", Label: "bash", Kind: "shell", Path: filepath.Join(h, ".bashrc")},
		{Name: "fish", Label: "fish", Kind: "shell", Path: filepath.Join(h, ".config", "fish", "conf.d", "oopsy.fish")},
		{Name: "claude", Label: "Claude Code", Kind: "agent", Path: filepath.Join(h, ".claude", "settings.json")},
		{Name: "codex", Label: "Codex CLI", Kind: "agent", Path: filepath.Join(h, ".codex", "hooks.json")},
		{Name: "cursor", Label: "Cursor", Kind: "agent", Path: filepath.Join(h, ".cursor", "hooks.json")},
		{Name: "gemini", Label: "Gemini CLI", Kind: "agent", Path: filepath.Join(h, ".gemini", "settings.json")},
	}
	for i := range ts {
		t := &ts[i]
		switch t.Name {
		case "zsh":
			t.Detected = exists(t.Path) || userShell == "zsh"
		case "bash":
			t.Detected = exists(t.Path) || userShell == "bash"
		case "fish":
			t.Detected = exists(filepath.Join(h, ".config", "fish")) || userShell == "fish"
		default:
			t.Detected = exists(filepath.Dir(t.Path))
		}
		t.Installed = installed(*t)
	}
	return ts
}

func installed(t Target) bool {
	b, err := os.ReadFile(t.Path)
	if err != nil {
		return false
	}
	if t.Kind == "shell" {
		return bytes.Contains(b, []byte(blockStart)) || (t.Name == "fish" && bytes.Contains(b, []byte("oops")))
	}
	return bytes.Contains(b, []byte(" hook "+t.Name))
}

func backup(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	dir := filepath.Join(snap.Dir(), "backups", time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := strings.ReplaceAll(strings.TrimPrefix(path, home()+"/"), "/", "_")
	dst := filepath.Join(dir, name)
	return dst, os.WriteFile(dst, b, 0o600)
}

func writeKeepMode(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".oopsy-tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Install adds the hook for t. It is safe to run twice.
func Install(t Target, bin string) (string, error) {
	if installed(t) {
		if t.Kind == "agent" {
			// Refresh the command path in case the binary moved.
			if _, err := Uninstall(t); err != nil {
				return "", err
			}
		} else {
			return "", nil
		}
	}
	bk, err := backup(t.Path)
	if err != nil {
		return "", err
	}
	switch t.Kind {
	case "shell":
		return bk, installShell(t, bin)
	default:
		return bk, editJSON(t.Path, func(o *obj) { addAgentHook(o, t.Name, bin) })
	}
}

// Uninstall removes the hook for t, leaving everything else untouched.
func Uninstall(t Target) (string, error) {
	if !installed(t) {
		return "", nil
	}
	bk, err := backup(t.Path)
	if err != nil {
		return "", err
	}
	if t.Kind == "shell" {
		if t.Name == "fish" {
			return bk, os.Remove(t.Path)
		}
		b, err := os.ReadFile(t.Path)
		if err != nil {
			return bk, err
		}
		return bk, writeKeepMode(t.Path, removeBlock(b))
	}
	return bk, editJSON(t.Path, func(o *obj) { removeAgentHook(o, t.Name) })
}

func installShell(t Target, bin string) error {
	if t.Name == "fish" {
		body := fmt.Sprintf("# Added by oops init. Remove with: oops uninstall\nif command -q %s\n    %s shell-init fish | source\nend\n", shQuote(bin), shQuote(bin))
		return writeKeepMode(t.Path, []byte(body))
	}
	var line string
	if bin == "oops" {
		line = fmt.Sprintf(`if command -v oops >/dev/null 2>&1; then eval "$(oops shell-init %s)"; fi`, t.Name)
	} else {
		line = fmt.Sprintf(`if [ -x %s ]; then eval "$(%s shell-init %s)"; fi`, shQuote(bin), shQuote(bin), t.Name)
	}
	block := fmt.Sprintf("\n%s\n# Snapshots destructive commands so you can undo them with: oops\n%s\n%s\n", blockStart, line, blockEnd)
	b, _ := os.ReadFile(t.Path)
	b = append(bytes.TrimRight(b, "\n"), []byte("\n"+block)...)
	if len(bytes.TrimSpace(b)) == len(bytes.TrimSpace([]byte(block))) {
		b = []byte(strings.TrimLeft(block, "\n"))
	}
	return writeKeepMode(t.Path, b)
}

func removeBlock(b []byte) []byte {
	s := string(b)
	for {
		i := strings.Index(s, blockStart)
		if i < 0 {
			break
		}
		j := strings.Index(s[i:], blockEnd)
		if j < 0 {
			break
		}
		end := i + j + len(blockEnd)
		if end < len(s) && s[end] == '\n' {
			end++
		}
		start := i
		if start > 0 && s[start-1] == '\n' && (start < 2 || s[start-2] == '\n') {
			start-- // drop the blank line we added before the block
		}
		s = s[:start] + s[end:]
	}
	return []byte(s)
}

func editJSON(path string, fn func(o *obj)) error {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	o, err := parseObj(b)
	if err != nil {
		return fmt.Errorf("%s is not valid JSON (%v); fix it or remove it, then run again", path, err)
	}
	fn(o)
	if len(o.keys) == 0 && len(b) > 0 {
		return os.Remove(path)
	}
	out, err := formatObj(o)
	if err != nil {
		return err
	}
	return writeKeepMode(path, out)
}

func hookCommand(bin, name string) string { return shQuote(bin) + " hook " + name }

func addAgentHook(o *obj, name, bin string) {
	cmd := hookCommand(bin, name)
	switch name {
	case "claude", "codex":
		h := o.child("hooks")
		list, _ := h.vals["PreToolUse"].([]any)
		entry := newObj()
		entry.set("matcher", "Bash")
		hk := newObj()
		hk.set("type", "command")
		hk.set("command", cmd)
		hk.set("timeout", 120)
		entry.set("hooks", []any{hk})
		h.set("PreToolUse", append(list, entry))
	case "gemini":
		h := o.child("hooks")
		list, _ := h.vals["BeforeTool"].([]any)
		entry := newObj()
		entry.set("matcher", "run_shell_command")
		hk := newObj()
		hk.set("name", "oopsy")
		hk.set("type", "command")
		hk.set("command", cmd)
		hk.set("timeout", 120000)
		entry.set("hooks", []any{hk})
		h.set("BeforeTool", append(list, entry))
	case "cursor":
		if _, ok := o.get("version"); !ok {
			o.set("version", 1)
		}
		h := o.child("hooks")
		list, _ := h.vals["beforeShellExecution"].([]any)
		hk := newObj()
		hk.set("command", cmd)
		hk.set("timeout", 120)
		h.set("beforeShellExecution", append(list, hk))
	}
}

func isOurs(v any, name string) bool {
	o, ok := v.(*obj)
	if !ok {
		return false
	}
	c, _ := o.vals["command"].(string)
	return strings.Contains(c, " hook "+name) && strings.Contains(c, "oops")
}

func removeAgentHook(o *obj, name string) {
	h, ok := o.vals["hooks"].(*obj)
	if !ok {
		return
	}
	event := map[string]string{"claude": "PreToolUse", "codex": "PreToolUse", "gemini": "BeforeTool", "cursor": "beforeShellExecution"}[name]
	list, _ := h.vals[event].([]any)
	var kept []any
	for _, item := range list {
		if name == "cursor" {
			if !isOurs(item, name) {
				kept = append(kept, item)
			}
			continue
		}
		group, ok := item.(*obj)
		if !ok {
			kept = append(kept, item)
			continue
		}
		inner, _ := group.vals["hooks"].([]any)
		var innerKept []any
		for _, hk := range inner {
			if !isOurs(hk, name) {
				innerKept = append(innerKept, hk)
			}
		}
		if len(innerKept) == 0 && len(inner) > 0 {
			continue
		}
		if len(innerKept) != len(inner) {
			group.set("hooks", innerKept)
		}
		kept = append(kept, item)
	}
	if len(kept) == 0 {
		h.del(event)
	} else {
		h.set(event, kept)
	}
	if len(h.keys) == 0 {
		o.del("hooks")
	}
	if name == "cursor" && len(o.keys) == 1 {
		if _, ok := o.vals["version"]; ok {
			o.del("version")
		}
	}
}
