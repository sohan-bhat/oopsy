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
	"runtime"
	"strings"
	"time"
	"unicode/utf16"

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
	if runtime.GOOS == "windows" {
		// Scoop and other installers put a shim on PATH that runs the real
		// exe from a versioned folder, so trust PATH rather than compare.
		if _, err := exec.LookPath("oops"); err == nil {
			return "oops"
		}
		return filepath.ToSlash(shortPath(self))
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
	win := runtime.GOOS == "windows"
	psDir := filepath.Join(h, ".config", "powershell")
	if win {
		psDir = filepath.Join(documentsDir(), "PowerShell")
	}
	ts := []Target{
		{Name: "zsh", Label: "zsh", Kind: "shell", Path: filepath.Join(h, ".zshrc")},
		{Name: "bash", Label: "bash", Kind: "shell", Path: filepath.Join(h, ".bashrc")},
		{Name: "fish", Label: "fish", Kind: "shell", Path: filepath.Join(h, ".config", "fish", "conf.d", "oopsy.fish")},
		{Name: "powershell", Label: "PowerShell", Kind: "shell", Path: filepath.Join(psDir, "profile.ps1")},
	}
	if win {
		ts[1].Label = "Git Bash"
		ts = append(ts, Target{Name: "windowspowershell", Label: "Windows PowerShell", Kind: "shell",
			Path: filepath.Join(documentsDir(), "WindowsPowerShell", "profile.ps1")},
			Target{Name: "cmd", Label: "cmd.exe", Kind: "shell", Path: `HKCU\` + cmdAutoRunKey + `\AutoRun`})
	}
	ts = append(ts,
		Target{Name: "claude", Label: "Claude Code", Kind: "agent", Path: filepath.Join(h, ".claude", "settings.json")},
		Target{Name: "codex", Label: "Codex CLI", Kind: "agent", Path: filepath.Join(h, ".codex", "hooks.json")},
		Target{Name: "cursor", Label: "Cursor", Kind: "agent", Path: filepath.Join(h, ".cursor", "hooks.json")},
		Target{Name: "gemini", Label: "Gemini CLI", Kind: "agent", Path: filepath.Join(h, ".gemini", "settings.json")},
	)
	for i := range ts {
		t := &ts[i]
		switch t.Name {
		case "zsh":
			t.Detected = exists(t.Path) || userShell == "zsh"
		case "bash":
			t.Detected = exists(t.Path) || userShell == "bash" || (win && gitBashInstalled())
		case "fish":
			t.Detected = exists(filepath.Join(h, ".config", "fish")) || userShell == "fish"
		case "powershell":
			_, err := exec.LookPath("pwsh")
			t.Detected = exists(t.Path) || err == nil
		case "windowspowershell", "cmd":
			t.Detected = true // part of every Windows install
		default:
			t.Detected = exists(filepath.Dir(t.Path))
		}
		t.Installed = installed(*t)
	}
	return ts
}

func gitBashInstalled() bool {
	if exists(filepath.Join(os.Getenv("ProgramFiles"), "Git", "bin", "bash.exe")) {
		return true
	}
	p, err := exec.LookPath("bash")
	return err == nil && !strings.Contains(strings.ToLower(p), `\system32\`)
}

func installed(t Target) bool {
	if t.Name == "cmd" {
		return cmdInstalled()
	}
	b, err := os.ReadFile(t.Path)
	if err != nil {
		return false
	}
	if t.Kind == "shell" {
		text := installedText(b)
		return strings.Contains(text, blockStart) || (t.Name == "fish" && strings.Contains(text, "oops"))
	}
	return bytes.Contains(b, []byte(" hook "+t.Name))
}

// installedText reads a profile in any encoding.
func installedText(b []byte) string { s, _ := decodeText(b); return s }

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
	rel, err := filepath.Rel(home(), path)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = path[len(filepath.VolumeName(path)):]
	}
	name := strings.NewReplacer("/", "_", `\`, "_", ":", "_").Replace(strings.TrimLeft(rel, `/\`))
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
	if t.Name == "cmd" {
		return installCmd(bin)
	}
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
	if t.Name == "cmd" {
		return uninstallCmd()
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
		out := removeBlock(b)
		if strings.TrimSpace(installedText(out)) == "" {
			return bk, os.Remove(t.Path) // the file only held our hook
		}
		return bk, writeKeepMode(t.Path, out)
	}
	return bk, editJSON(t.Path, func(o *obj) { removeAgentHook(o, t.Name) })
}

func installShell(t Target, bin string) error {
	if t.Name == "fish" {
		body := fmt.Sprintf("# Added by oops init. Remove with: oops uninstall\nif command -q %s\n    %s shell-init fish | source\nend\n", shQuote(bin), shQuote(bin))
		return writeKeepMode(t.Path, []byte(body))
	}
	var line string
	switch {
	case t.Name == "powershell" || t.Name == "windowspowershell":
		if bin == "oops" {
			line = `if (Get-Command oops -ErrorAction SilentlyContinue) { oops shell-init powershell | Out-String | Invoke-Expression }`
		} else {
			line = fmt.Sprintf(`if (Test-Path %s) { & %s shell-init powershell | Out-String | Invoke-Expression }`, psQuote(bin), psQuote(bin))
		}
	case bin == "oops":
		line = fmt.Sprintf(`if command -v oops >/dev/null 2>&1; then eval "$(oops shell-init %s)"; fi`, t.Name)
	default:
		line = fmt.Sprintf(`if [ -x %s ]; then eval "$(%s shell-init %s)"; fi`, shQuote(bin), shQuote(bin), t.Name)
	}
	raw, _ := os.ReadFile(t.Path)
	text, enc := decodeText(raw)
	nl := "\n"
	if strings.Contains(text, "\r\n") || (text == "" && strings.HasSuffix(t.Path, ".ps1") && runtime.GOOS == "windows") {
		nl = "\r\n"
	}
	block := strings.Join([]string{blockStart, "# Snapshots destructive commands so you can undo them with: oops", line, blockEnd}, nl) + nl
	text = strings.TrimRight(text, "\r\n")
	if text == "" {
		text = block
	} else {
		text += nl + nl + block
	}
	return writeKeepMode(t.Path, encodeText(text, enc))
}

// removeBlock takes our block (and the blank line before it) out of a file,
// keeping its encoding and line endings.
func removeBlock(b []byte) []byte {
	s, enc := decodeText(b)
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
		if strings.HasPrefix(s[end:], "\r\n") {
			end += 2
		} else if strings.HasPrefix(s[end:], "\n") {
			end++
		}
		start := i
		for _, blank := range []string{"\r\n\r\n", "\n\n"} {
			if strings.HasSuffix(s[:start], blank) {
				start -= len(blank) / 2 // drop the blank line we added before the block
				break
			}
		}
		s = s[:start] + s[end:]
	}
	return encodeText(s, enc)
}

// Text encodings found in shell profiles. Windows PowerShell editors often
// save profiles as UTF-16 or with a UTF-8 byte order mark.
const (
	encUTF8 = iota
	encUTF8BOM
	encUTF16LE
)

func decodeText(b []byte) (string, int) {
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		return string(b[3:]), encUTF8BOM
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		u := make([]uint16, (len(b)-2)/2)
		for i := range u {
			u[i] = uint16(b[2+2*i]) | uint16(b[3+2*i])<<8
		}
		return string(utf16.Decode(u)), encUTF16LE
	}
	return string(b), encUTF8
}

func encodeText(s string, enc int) []byte {
	switch enc {
	case encUTF8BOM:
		return append([]byte{0xEF, 0xBB, 0xBF}, s...)
	case encUTF16LE:
		out := []byte{0xFF, 0xFE}
		for _, u := range utf16.Encode([]rune(s)) {
			out = append(out, byte(u), byte(u>>8))
		}
		return out
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

// hookCommand is the command line an agent runs. On Windows agents may run
// it through cmd, PowerShell or Git Bash, so the path is never quoted: it is
// either the bare name on PATH or a short path without spaces.
func hookCommand(bin, name string) string {
	if runtime.GOOS == "windows" {
		return bin + " hook " + name
	}
	return shQuote(bin) + " hook " + name
}

// agentMatcher is the Claude Code and Codex tool matcher. Claude Code on
// Windows can also run commands with a PowerShell tool.
func agentMatcher() string {
	if runtime.GOOS == "windows" {
		return "Bash|PowerShell"
	}
	return "Bash"
}

func addAgentHook(o *obj, name, bin string) {
	cmd := hookCommand(bin, name)
	switch name {
	case "claude", "codex":
		h := o.child("hooks")
		list, _ := h.vals["PreToolUse"].([]any)
		entry := newObj()
		entry.set("matcher", agentMatcher())
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
