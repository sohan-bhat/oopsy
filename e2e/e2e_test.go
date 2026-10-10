// Package e2e runs the real oops binary against real files, shells, git
// repositories and agent hook payloads.
package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "oops-e2e-bin")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "oops")
	build := exec.Command("go", "build", "-o", bin, "../cmd/oops")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type env struct {
	t    *testing.T
	home string // fake HOME
	oops string // OOPS_HOME
	work string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	e := &env{t: t, home: filepath.Join(root, "home"), oops: filepath.Join(root, "home", ".oops"), work: filepath.Join(root, "work")}
	os.MkdirAll(e.home, 0o755)
	os.MkdirAll(e.work, 0o755)
	t.Cleanup(func() { forceRemove(root) })
	return e
}

func forceRemove(p string) {
	filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(path, 0o755)
		}
		return nil
	})
	os.RemoveAll(p)
}

func (e *env) environ() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "OOPS_HOME=") || strings.HasPrefix(kv, "NO_COLOR=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HOME="+e.home, "OOPS_HOME="+e.oops, "NO_COLOR=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_NOSYSTEM=1")
}

// oopsCmd runs the oops binary.
func (e *env) oopsCmd(stdin string, args ...string) (string, string, error) {
	c := exec.Command(bin, args...)
	c.Dir = e.work
	c.Env = e.environ()
	c.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err := c.Run()
	return out.String(), errb.String(), err
}

func (e *env) mustOops(args ...string) string {
	e.t.Helper()
	out, errOut, err := e.oopsCmd("", args...)
	if err != nil {
		e.t.Fatalf("oops %v: %v\n%s\n%s", args, err, out, errOut)
	}
	return out + errOut
}

// sh runs a command the way a user would.
func (e *env) sh(cmd string) string {
	e.t.Helper()
	c := exec.Command("/bin/sh", "-c", cmd)
	c.Dir = e.work
	c.Env = e.environ()
	out, err := c.CombinedOutput()
	if err != nil {
		e.t.Fatalf("sh %q: %v\n%s", cmd, err, out)
	}
	return string(out)
}

// guarded runs the shell hook and then the command, like the zsh preexec hook.
func (e *env) guarded(cmd string) string {
	e.t.Helper()
	_, hookErr, _ := e.oopsCmd("", "hook", "zsh", "--", cmd)
	e.sh(cmd)
	return hookErr
}

func setTimes(p string, t time.Time) {
	ts := []syscall.Timespec{syscall.NsecToTimespec(t.UnixNano()), syscall.NsecToTimespec(t.UnixNano())}
	_ = utimesNoFollow(p, ts)
}

// makeTree builds a tree with every kind of entry we care about.
func makeTree(t *testing.T, root string) {
	t.Helper()
	big := make([]byte, 5<<20)
	rand.Read(big)
	files := map[string][]byte{
		"README.md":               []byte("# project\n"),
		".env":                    []byte("SECRET=hunter2\n"),
		".gitignore":              []byte("node_modules\n"),
		"src/main.go":             []byte("package main\n"),
		"src/util/strings.go":     []byte("package util\n"),
		"data/big.bin":            big,
		"data/empty":              {},
		"weird name with spaces":  []byte("spaces\n"),
		"unicodé/файл.txt":        []byte("unicode\n"),
		"node_modules/x/index.js": []byte("module.exports = 1\n"),
		"deep/a/b/c/d/e/f.txt":    []byte("deep\n"),
	}
	for name, data := range files {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755)
	os.WriteFile(filepath.Join(root, "readonly.txt"), []byte("locked\n"), 0o444)
	os.Chmod(filepath.Join(root, "src/util"), 0o700)
	os.Symlink("README.md", filepath.Join(root, "link-to-readme"))
	os.Symlink("src", filepath.Join(root, "link-to-dir"))
	os.Symlink("does/not/exist", filepath.Join(root, "dangling"))
	os.MkdirAll(filepath.Join(root, "emptydir"), 0o750)
	// Old, distinct timestamps everywhere, deepest first so parents stick.
	var paths []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		paths = append(paths, p)
		return nil
	})
	sort.Sort(sort.Reverse(sort.StringSlice(paths)))
	base := time.Date(2021, 3, 4, 5, 6, 7, 123456000, time.UTC)
	for i, p := range paths {
		setTimes(p, base.Add(time.Duration(i)*time.Hour))
	}
}

type node struct {
	mode  fs.FileMode
	link  string
	data  []byte
	mtime int64
}

func snapshotTree(t *testing.T, root string, withRootMtime bool) map[string]node {
	t.Helper()
	out := map[string]node{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		n := node{mode: fi.Mode(), mtime: fi.ModTime().UnixNano() / 1000}
		if rel == "." && !withRootMtime {
			n.mtime = 0
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			n.link, _ = os.Readlink(p)
			n.mtime = 0 // symlink times are not portable to compare
		case fi.Mode().IsRegular():
			n.data, _ = os.ReadFile(p)
		}
		out[rel] = n
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func compareTrees(t *testing.T, want, got map[string]node) {
	t.Helper()
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			t.Errorf("missing after restore: %s", k)
			continue
		}
		if w.mode != g.mode {
			t.Errorf("%s: mode %v, want %v", k, g.mode, w.mode)
		}
		if w.link != g.link {
			t.Errorf("%s: symlink %q, want %q", k, g.link, w.link)
		}
		if !bytes.Equal(w.data, g.data) {
			t.Errorf("%s: content differs (%d bytes vs %d)", k, len(g.data), len(w.data))
		}
		if w.mtime != g.mtime {
			t.Errorf("%s: mtime %v, want %v", k, time.UnixMicro(g.mtime), time.UnixMicro(w.mtime))
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected after restore: %s", k)
		}
	}
}

func TestRmRfThenOops(t *testing.T) {
	e := newEnv(t)
	tree := filepath.Join(e.work, "project")
	makeTree(t, tree)
	before := snapshotTree(t, tree, true)

	msg := e.guarded("rm -rf project")
	if !strings.Contains(msg, "saved snapshot 1") {
		t.Fatalf("hook did not report a snapshot: %q", msg)
	}
	if _, err := os.Lstat(tree); err == nil {
		t.Fatal("rm -rf did not remove the tree")
	}
	out := e.mustOops("-y")
	if !strings.Contains(out, "Restored snapshot 1") {
		t.Fatalf("unexpected output: %s", out)
	}
	compareTrees(t, before, snapshotTree(t, tree, true))
}

func TestGlobDeleteThenUndoByID(t *testing.T) {
	e := newEnv(t)
	tree := filepath.Join(e.work, "project")
	makeTree(t, tree)
	before := snapshotTree(t, tree, false)
	e.guarded("cd project && rm -rf * .env .gitignore")
	if _, err := os.Stat(filepath.Join(tree, "README.md")); err == nil {
		t.Fatal("files still there")
	}
	list := e.mustOops("list")
	if !strings.Contains(list, "cd project && rm -rf *") {
		t.Fatalf("list output: %s", list)
	}
	e.mustOops("undo", "1", "-y")
	compareTrees(t, before, snapshotTree(t, tree, false))
}

func TestRestoreNeverClobbers(t *testing.T) {
	e := newEnv(t)
	os.WriteFile(filepath.Join(e.work, "notes.txt"), []byte("original\n"), 0o644)
	e.guarded("rm notes.txt")
	os.WriteFile(filepath.Join(e.work, "notes.txt"), []byte("newer work\n"), 0o644)
	out := e.mustOops("-y")
	if !strings.Contains(out, "kept both") {
		t.Fatalf("expected keep both: %s", out)
	}
	cur, _ := os.ReadFile(filepath.Join(e.work, "notes.txt"))
	if string(cur) != "newer work\n" {
		t.Fatalf("newer file was clobbered: %q", cur)
	}
	saved, err := os.ReadFile(filepath.Join(e.work, "notes.oops-1.txt"))
	if err != nil || string(saved) != "original\n" {
		t.Fatalf("saved copy missing: %v %q", err, saved)
	}
}

func TestOverwritesAreUndoable(t *testing.T) {
	e := newEnv(t)
	w := func(name, s string) { os.WriteFile(filepath.Join(e.work, name), []byte(s), 0o644) }
	read := func(name string) string { b, _ := os.ReadFile(filepath.Join(e.work, name)); return string(b) }
	cases := []struct {
		name, cmd string
	}{
		{"redirect", "echo clobbered > f.txt"},
		{"mv", "echo other > g.txt && mv g.txt f.txt"},
		{"cp", "echo other > g.txt; cp g.txt f.txt"},
		{"sed", "sed -i.bak 's/precious/gone/' f.txt"},
		{"truncate", "truncate -s 0 f.txt"},
		{"tee", "echo x | tee f.txt"},
		{"dd", "dd if=/dev/zero of=f.txt bs=1 count=3 2>/dev/null"},
	}
	for i, c := range cases {
		w("f.txt", "precious data\n")
		e.guarded(c.cmd)
		if read("f.txt") == "precious data\n" {
			t.Fatalf("%s: command did not change the file", c.name)
		}
		os.Remove(filepath.Join(e.work, "f.txt"))
		out := e.mustOops("undo", fmt.Sprint(i+1), "-y")
		if read("f.txt") != "precious data\n" {
			t.Errorf("%s: not restored (%q)\n%s", c.name, read("f.txt"), out)
		}
		os.Remove(filepath.Join(e.work, "g.txt"))
		os.Remove(filepath.Join(e.work, "f.txt.bak"))
	}
}

func (e *env) git(args ...string) string {
	e.t.Helper()
	c := exec.Command("git", args...)
	c.Dir = e.work
	c.Env = e.environ()
	out, err := c.CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestGitResetHardAndClean(t *testing.T) {
	e := newEnv(t)
	e.git("init", "-q")
	os.WriteFile(filepath.Join(e.work, "tracked.txt"), []byte("v1\n"), 0o644)
	os.WriteFile(filepath.Join(e.work, ".gitignore"), []byte("*.log\n"), 0o644)
	e.git("add", ".")
	e.git("commit", "-qm", "one")
	os.WriteFile(filepath.Join(e.work, "tracked.txt"), []byte("uncommitted edits\n"), 0o644)
	os.WriteFile(filepath.Join(e.work, "staged-new.txt"), []byte("staged but never committed\n"), 0o644)
	e.git("add", "staged-new.txt")

	e.guarded("git reset --hard")
	if b, _ := os.ReadFile(filepath.Join(e.work, "tracked.txt")); string(b) != "v1\n" {
		t.Fatal("reset did not run")
	}
	e.mustOops("-y")
	if b, _ := os.ReadFile(filepath.Join(e.work, "tracked.txt")); string(b) != "uncommitted edits\n" {
		t.Errorf("tracked edits not restored: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(e.work, "staged-new.txt")); string(b) != "staged but never committed\n" {
		t.Errorf("staged file not restored: %q", b)
	}

	// git clean -fdx removes untracked and ignored files.
	os.WriteFile(filepath.Join(e.work, "debug.log"), []byte("ignored\n"), 0o644)
	os.MkdirAll(filepath.Join(e.work, "scratch"), 0o755)
	os.WriteFile(filepath.Join(e.work, "scratch", "idea.txt"), []byte("untracked\n"), 0o644)
	e.guarded("git clean -fdx")
	if _, err := os.Stat(filepath.Join(e.work, "debug.log")); err == nil {
		t.Fatal("clean did not run")
	}
	e.mustOops("-y")
	for _, f := range []string{"debug.log", "scratch/idea.txt", "staged-new.txt"} {
		if _, err := os.Stat(filepath.Join(e.work, f)); err != nil {
			t.Errorf("%s not restored after git clean", f)
		}
	}
}

func TestGitRefs(t *testing.T) {
	e := newEnv(t)
	e.git("init", "-q")
	os.WriteFile(filepath.Join(e.work, "a"), []byte("a\n"), 0o644)
	e.git("add", ".")
	e.git("commit", "-qm", "one")
	e.git("checkout", "-qb", "feature")
	os.WriteFile(filepath.Join(e.work, "b"), []byte("b\n"), 0o644)
	e.git("add", ".")
	e.git("commit", "-qm", "feature work")
	sha := e.git("rev-parse", "feature")
	e.git("checkout", "-q", "-")
	e.guarded("git branch -D feature")
	if out, _ := exec.Command("git", "-C", e.work, "rev-parse", "--verify", "-q", "feature").Output(); len(out) > 0 {
		t.Fatal("branch not deleted")
	}
	e.mustOops("-y")
	if got := e.git("rev-parse", "feature"); got != sha {
		t.Fatalf("branch restored at %s, want %s", got, sha)
	}

	os.WriteFile(filepath.Join(e.work, "a"), []byte("stashed change\n"), 0o644)
	e.git("stash", "-q")
	stash := e.git("rev-parse", "stash@{0}")
	e.guarded("git stash drop")
	e.mustOops("-y")
	if got := e.git("rev-parse", "stash@{0}"); got != stash {
		t.Fatalf("stash restored as %s, want %s", got, stash)
	}
}

func TestClaudeCodeHookEndToEnd(t *testing.T) {
	e := newEnv(t)
	tree := filepath.Join(e.work, "app")
	makeTree(t, tree)
	before := snapshotTree(t, tree, true)

	payload := map[string]any{
		"session_id":      "abc123",
		"transcript_path": filepath.Join(e.home, ".claude", "projects", "x.jsonl"),
		"cwd":             e.work,
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": "rm -rf app", "description": "Remove the app folder"},
	}
	b, _ := json.Marshal(payload)
	out, stderr, err := e.oopsCmd(string(b), "hook", "claude")
	if err != nil {
		t.Fatalf("hook failed: %v %s", err, stderr)
	}
	var reply map[string]any
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		t.Fatalf("hook stdout is not JSON: %q", out)
	}
	if msg, _ := reply["systemMessage"].(string); !strings.Contains(msg, "snapshot 1") {
		t.Fatalf("unexpected reply: %v", reply)
	}
	e.sh("rm -rf app")
	list := e.mustOops("list")
	if !strings.Contains(list, "Claude Code") {
		t.Fatalf("source not recorded: %s", list)
	}
	// An agent has no terminal, so plain oops must refuse instead of hanging.
	if _, errOut, err := e.oopsCmd("", "undo", "1"); err == nil || !strings.Contains(errOut, "-y") {
		t.Fatalf("expected a refusal without -y, got err=%v %s", err, errOut)
	}
	e.mustOops("undo", "1", "-y")
	compareTrees(t, before, snapshotTree(t, tree, true))

	// Harmless commands pass straight through with no output and no snapshot.
	payload["tool_input"] = map[string]any{"command": "ls -la && git status"}
	b, _ = json.Marshal(payload)
	out, _, err = e.oopsCmd(string(b), "hook", "claude")
	if err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("harmless command produced %q %v", out, err)
	}
	// Garbage input must never block the agent.
	if _, _, err := e.oopsCmd("not json", "hook", "claude"); err != nil {
		t.Fatalf("hook should exit 0 on bad input: %v", err)
	}
}

func TestOtherAgentPayloads(t *testing.T) {
	e := newEnv(t)
	for i, c := range []struct {
		source, payload, wantOut string
	}{
		{"codex", `{"session_id":"s","cwd":%q,"hook_event_name":"PreToolUse","tool_name":"Bash","tool_use_id":"t","tool_input":{"command":"rm f%d.txt"}}`, ""},
		{"gemini", `{"session_id":"s","cwd":%q,"hook_event_name":"BeforeTool","tool_name":"run_shell_command","tool_input":{"command":"rm f%d.txt"}}`, "{}"},
		{"cursor", `{"conversation_id":"c","hook_event_name":"beforeShellExecution","cwd":%q,"command":"rm f%d.txt","workspace_roots":[]}`, `{"continue":true,"permission":"allow"}`},
	} {
		name := fmt.Sprintf("f%d.txt", i)
		os.WriteFile(filepath.Join(e.work, name), []byte("keep me\n"), 0o644)
		out, _, err := e.oopsCmd(fmt.Sprintf(c.payload, e.work, i), "hook", c.source)
		if err != nil {
			t.Fatalf("%s: %v", c.source, err)
		}
		if strings.TrimSpace(out) != c.wantOut {
			t.Errorf("%s: stdout %q, want %q", c.source, out, c.wantOut)
		}
		os.Remove(filepath.Join(e.work, name))
		e.mustOops("-y")
		if b, _ := os.ReadFile(filepath.Join(e.work, name)); string(b) != "keep me\n" {
			t.Errorf("%s: file not restored", c.source)
		}
	}
}

func TestInitAndUninstallAreClean(t *testing.T) {
	e := newEnv(t)
	zshrc := "# my zshrc\nexport PATH=$HOME/bin:$PATH\nalias ll='ls -l'\n"
	os.WriteFile(filepath.Join(e.home, ".zshrc"), []byte(zshrc), 0o600)
	os.WriteFile(filepath.Join(e.home, ".bashrc"), []byte("export A=1\n"), 0o644)
	os.MkdirAll(filepath.Join(e.home, ".claude"), 0o755)
	claude := `{
  "model": "opus",
  "permissions": {"allow": ["Bash(ls:*)"]},
  "hooks": {
    "PreToolUse": [
      {"matcher": "Edit", "hooks": [{"type": "command", "command": "my-linter"}]}
    ],
    "Stop": [{"hooks": [{"type": "command", "command": "notify"}]}]
  },
  "big": 12345678901234567890
}
`
	os.WriteFile(filepath.Join(e.home, ".claude", "settings.json"), []byte(claude), 0o644)
	os.MkdirAll(filepath.Join(e.home, ".codex"), 0o755)
	os.MkdirAll(filepath.Join(e.home, ".cursor"), 0o755)
	os.MkdirAll(filepath.Join(e.home, ".gemini"), 0o755)
	os.WriteFile(filepath.Join(e.home, ".gemini", "settings.json"), []byte(`{"theme": "dark"}`), 0o644)

	out := e.mustOops("init")
	for _, want := range []string{"zsh", "bash", "Claude Code", "Codex CLI", "Cursor", "Gemini CLI"} {
		if !strings.Contains(out, want) {
			t.Errorf("init output missing %s:\n%s", want, out)
		}
	}
	// Running init twice must not duplicate anything.
	e.mustOops("init")

	rc, _ := os.ReadFile(filepath.Join(e.home, ".zshrc"))
	if strings.Count(string(rc), "# >>> oopsy >>>") != 1 || !strings.HasPrefix(string(rc), zshrc) {
		t.Fatalf("zshrc not updated cleanly:\n%s", rc)
	}
	var settings map[string]any
	sb, _ := os.ReadFile(filepath.Join(e.home, ".claude", "settings.json"))
	if err := json.Unmarshal(sb, &settings); err != nil {
		t.Fatal(err)
	}
	pre := settings["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(pre) != 2 {
		t.Fatalf("expected the existing hook plus ours, got %d: %s", len(pre), sb)
	}
	if !strings.Contains(string(sb), `"big": 12345678901234567890`) || !strings.Contains(string(sb), "my-linter") {
		t.Fatalf("existing settings damaged:\n%s", sb)
	}
	if strings.Index(string(sb), `"model"`) > strings.Index(string(sb), `"hooks"`) {
		t.Fatalf("key order changed:\n%s", sb)
	}
	for _, f := range []string{".codex/hooks.json", ".cursor/hooks.json", ".gemini/settings.json"} {
		b, err := os.ReadFile(filepath.Join(e.home, f))
		if err != nil || !strings.Contains(string(b), "hook ") {
			t.Errorf("%s missing hook: %v %s", f, err, b)
		}
	}
	backups, _ := os.ReadDir(filepath.Join(e.oops, "backups"))
	if len(backups) == 0 {
		t.Error("no backups written")
	}
	status := e.mustOops("status")
	if !strings.Contains(status, "Claude Code") || !strings.Contains(status, "on") {
		t.Errorf("status: %s", status)
	}

	e.mustOops("uninstall")
	rc, _ = os.ReadFile(filepath.Join(e.home, ".zshrc"))
	if string(rc) != zshrc {
		t.Fatalf("zshrc not restored byte for byte:\n%q\nwant\n%q", rc, zshrc)
	}
	if b, _ := os.ReadFile(filepath.Join(e.home, ".bashrc")); string(b) != "export A=1\n" {
		t.Fatalf("bashrc not restored: %q", b)
	}
	var after, orig map[string]any
	sb, _ = os.ReadFile(filepath.Join(e.home, ".claude", "settings.json"))
	dec := json.NewDecoder(bytes.NewReader(sb))
	dec.UseNumber()
	dec.Decode(&after)
	dec = json.NewDecoder(strings.NewReader(claude))
	dec.UseNumber()
	dec.Decode(&orig)
	if fmt.Sprint(after) != fmt.Sprint(orig) {
		t.Fatalf("claude settings not restored:\n%s", sb)
	}
	if b, _ := os.ReadFile(filepath.Join(e.home, ".gemini", "settings.json")); !strings.Contains(string(b), "dark") || strings.Contains(string(b), "oops") {
		t.Fatalf("gemini settings: %s", b)
	}
	for _, f := range []string{".codex/hooks.json", ".cursor/hooks.json"} {
		if _, err := os.Stat(filepath.Join(e.home, f)); err == nil {
			t.Errorf("%s should be gone after uninstall", f)
		}
	}
}

func TestBashHookRunsInRealShell(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	e := newEnv(t)
	os.WriteFile(filepath.Join(e.work, "keep.txt"), []byte("bash keeps this\n"), 0o644)
	init := e.mustOops("shell-init", "bash")
	script := init + "\nrm -f keep.txt\n"
	c := exec.Command("bash", "-c", script)
	c.Dir = e.work
	c.Env = append(e.environ(), "PATH="+filepath.Dir(bin)+":"+os.Getenv("PATH"))
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v %s", err, out)
	}
	if !strings.Contains(string(out), "saved snapshot") {
		t.Fatalf("bash hook did not fire: %s", out)
	}
	e.mustOops("-y")
	if b, _ := os.ReadFile(filepath.Join(e.work, "keep.txt")); string(b) != "bash keeps this\n" {
		t.Fatal("not restored")
	}
}

func TestZshHookRunsInRealShell(t *testing.T) {
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("no zsh")
	}
	e := newEnv(t)
	os.WriteFile(filepath.Join(e.work, "keep.txt"), []byte("zsh keeps this\n"), 0o644)
	init := e.mustOops("shell-init", "zsh")
	initFile := filepath.Join(e.home, "init.zsh")
	os.WriteFile(initFile, []byte(init), 0o644)
	// An interactive zsh reading commands from stdin runs preexec hooks.
	c := exec.Command("zsh", "-f", "-i")
	c.Dir = e.work
	c.Env = append(e.environ(), "PATH="+filepath.Dir(bin)+":"+os.Getenv("PATH"))
	c.Stdin = strings.NewReader("source " + initFile + "\nrm -f keep.txt\nexit\n")
	out, _ := c.CombinedOutput()
	if !strings.Contains(string(out), "saved snapshot") {
		t.Skipf("interactive zsh did not run preexec without a terminal here: %s", out)
	}
	e.mustOops("-y")
	if b, _ := os.ReadFile(filepath.Join(e.work, "keep.txt")); string(b) != "zsh keeps this\n" {
		t.Fatal("not restored")
	}
}

func TestPruneAndDoctor(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 3; i++ {
		os.WriteFile(filepath.Join(e.work, "x.txt"), []byte("data"), 0o644)
		e.guarded("rm x.txt")
	}
	if out := e.mustOops("list"); strings.Count(out, "rm x.txt") != 3 {
		t.Fatalf("list: %s", out)
	}
	out := e.mustOops("prune", "--all")
	if !strings.Contains(out, "Removed 3 snapshots") {
		t.Fatalf("prune: %s", out)
	}
	doc := e.mustOops("doctor")
	if !strings.Contains(doc, "hook speed") || !strings.Contains(doc, "copy on write") {
		t.Fatalf("doctor: %s", doc)
	}
	if runtime.GOOS == "darwin" && !strings.Contains(doc, "APFS clones") {
		t.Errorf("expected APFS clone support: %s", doc)
	}
}
