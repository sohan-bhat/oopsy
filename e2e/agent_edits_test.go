package e2e

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fileState is what undo must bring back for an edited file.
type fileState struct {
	data  string
	mode  fs.FileMode
	mtime time.Time
}

func stateOf(t *testing.T, p string) fileState {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	return fileState{string(b), fi.Mode(), fi.ModTime()}
}

func sameFile(t *testing.T, what string, want, got fileState) {
	t.Helper()
	if got.data != want.data {
		t.Errorf("%s: content %q, want %q", what, got.data, want.data)
	}
	if got.mode != want.mode {
		t.Errorf("%s: mode %v, want %v", what, got.mode, want.mode)
	}
	if !got.mtime.Equal(want.mtime) {
		t.Errorf("%s: mtime %v, want %v", what, got.mtime, want.mtime)
	}
}

// writeFile makes a file with a known mode and an old mtime.
func writeFile(t *testing.T, p, data string, mode fs.FileMode) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, mode)
	old := time.Unix(1600000000, 123456700)
	os.Chtimes(p, old, old)
}

// agentHook sends a payload to oops hook source and returns stdout.
func (e *env) agentHook(source string, payload map[string]any) string {
	e.t.Helper()
	b, _ := json.Marshal(payload)
	out, errOut, err := e.oopsCmd(string(b), "hook", source)
	if err != nil {
		e.t.Fatalf("hook %s: %v\n%s", source, err, errOut)
	}
	return out
}

func claudeEdit(e *env, event, tool, id string, input map[string]any) map[string]any {
	p := map[string]any{
		"session_id":      "sess1",
		"transcript_path": filepath.Join(e.home, ".claude", "projects", "x.jsonl"),
		"cwd":             e.work,
		"permission_mode": "default",
		"hook_event_name": event,
		"tool_name":       tool,
		"tool_input":      input,
		"tool_use_id":     id,
	}
	if event == "PostToolUse" {
		p["tool_response"] = map[string]any{"filePath": input["file_path"], "type": "update"}
	}
	return p
}

func TestClaudeCodeEditToolsUndo(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		tool, rel, field string
		extra            map[string]any
		before, after    string
	}{
		{"Edit", "src/app.ts", "file_path", map[string]any{"old_string": "return 1", "new_string": "return 2", "replace_all": false},
			"export function f() {\n  return 1\n}\n", "export function f() {\n  return 2\n}\n"},
		{"MultiEdit", "src/util.ts", "file_path", map[string]any{"edits": []any{map[string]any{"old_string": "a", "new_string": "b"}}},
			"const a = 1\n", "const b = 1\n"},
		{"Write", "README.md", "file_path", map[string]any{"content": "# new\n"}, "# old readme\nwith two lines\n", "# new\n"},
		{"NotebookEdit", "nb/analysis.ipynb", "notebook_path", map[string]any{"cell_id": "c1", "new_source": "print(2)", "edit_mode": "replace"},
			`{"cells":[{"id":"c1","source":"print(1)"}]}`, `{"cells":[{"id":"c1","source":"print(2)"}]}`},
	}
	for i, c := range cases {
		p := filepath.Join(e.work, filepath.FromSlash(c.rel))
		writeFile(t, p, c.before, 0o640)
		want := stateOf(t, p)
		input := map[string]any{c.field: p}
		for k, v := range c.extra {
			input[k] = v
		}
		id := "toolu_0" + string(rune('A'+i))
		if out := e.agentHook("claude", claudeEdit(e, "PreToolUse", c.tool, id, input)); strings.TrimSpace(out) != "" {
			t.Errorf("%s: pre hook printed %q", c.tool, out)
		}
		if err := os.WriteFile(p, []byte(c.after), 0o640); err != nil {
			t.Fatal(err)
		}
		e.agentHook("claude-post", claudeEdit(e, "PostToolUse", c.tool, id, input))
		list := e.mustOops("list")
		if !strings.Contains(list, "Claude Code") || !strings.Contains(list, c.tool+" "+c.rel) {
			t.Fatalf("%s: edit snapshot not listed:\n%s", c.tool, list)
		}
		e.mustOops("-y")
		sameFile(t, c.tool, want, stateOf(t, p))
	}

	// Write creating a new file has nothing to save and must not fail.
	p := filepath.Join(e.work, "src", "new.ts")
	input := map[string]any{"file_path": p, "content": "x\n"}
	e.agentHook("claude", claudeEdit(e, "PreToolUse", "Write", "toolu_new", input))
	os.WriteFile(p, []byte("x\n"), 0o644)
	e.agentHook("claude-post", claudeEdit(e, "PostToolUse", "Write", "toolu_new", input))
	if list := e.mustOops("list"); strings.Contains(list, "Write src/new.ts") {
		t.Fatalf("a new file made a snapshot:\n%s", list)
	}

	// An edit that failed leaves no snapshot behind.
	p = filepath.Join(e.work, "src", "same.ts")
	writeFile(t, p, "same\n", 0o644)
	input = map[string]any{"file_path": p, "old_string": "nope", "new_string": "x"}
	e.agentHook("claude", claudeEdit(e, "PreToolUse", "Edit", "toolu_fail", input))
	e.agentHook("claude-post", claudeEdit(e, "PostToolUse", "Edit", "toolu_fail", input))
	if list := e.mustOops("list"); strings.Contains(list, "Edit src/same.ts") {
		t.Fatalf("an unchanged file kept its snapshot:\n%s", list)
	}
}

// objectFiles lists the files in the object store.
func objectFiles(t *testing.T, e *env) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	filepath.WalkDir(filepath.Join(e.oops, "objects"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasPrefix(d.Name(), ".") {
			fi, _ := d.Info()
			out[p] = fi.Size()
		}
		return nil
	})
	return out
}

func TestEditVersionsAreStoredOnce(t *testing.T) {
	e := newEnv(t)
	var src strings.Builder
	src.WriteString("package app\n\nimport \"fmt\"\n\n")
	for i := 0; i < 40; i++ {
		src.WriteString("func handler" + string(rune('A'+i%26)) + "() { fmt.Println(\"hello from a typical handler\") }\n")
	}
	body := src.String()
	a := filepath.Join(e.work, "a.go")
	b := filepath.Join(e.work, "b.go")
	writeFile(t, a, body, 0o644)
	writeFile(t, b, body, 0o644)
	for i, p := range []string{a, b} {
		input := map[string]any{"file_path": p, "old_string": "package app", "new_string": "package main"}
		id := "toolu_d" + string(rune('0'+i))
		e.agentHook("claude", claudeEdit(e, "PreToolUse", "Edit", id, input))
		os.WriteFile(p, []byte(strings.Replace(body, "package app", "package main", 1)), 0o644)
		e.agentHook("claude-post", claudeEdit(e, "PostToolUse", "Edit", id, input))
	}
	objs := objectFiles(t, e)
	if len(objs) != 1 {
		t.Fatalf("two identical versions stored as %d objects: %v", len(objs), objs)
	}
	var stored int64
	for _, n := range objs {
		stored = n
	}
	metas, _ := filepath.Glob(filepath.Join(e.oops, "snapshots", "*", "meta.json"))
	var meta int64
	for _, m := range metas {
		fi, _ := os.Stat(m)
		meta += fi.Size()
	}
	t.Logf("a %d byte source file is stored as a %d byte object; two snapshots of it take %d bytes of metadata in all", len(body), stored, meta)
	e.mustOops("-y")
	e.mustOops("-y")
	for _, p := range []string{a, b} {
		if got, _ := os.ReadFile(p); string(got) != body {
			t.Errorf("%s not restored", p)
		}
	}
}

func TestGeminiEditToolsUndo(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		tool  string
		input map[string]any
	}{
		{"write_file", map[string]any{"content": "new\n"}},
		{"replace", map[string]any{"instruction": "fix", "old_string": "old", "new_string": "new"}},
	} {
		p := filepath.Join(e.work, c.tool+".txt")
		writeFile(t, p, "old\n", 0o644)
		want := stateOf(t, p)
		c.input["file_path"] = p
		payload := map[string]any{"session_id": "g1", "transcript_path": "", "cwd": e.work, "hook_event_name": "BeforeTool",
			"timestamp": time.Now().Format(time.RFC3339), "tool_name": c.tool, "tool_input": c.input}
		if out := e.agentHook("gemini", payload); strings.TrimSpace(out) != "{}" {
			t.Errorf("%s: BeforeTool stdout %q", c.tool, out)
		}
		os.WriteFile(p, []byte("new\n"), 0o644)
		payload["hook_event_name"] = "AfterTool"
		payload["tool_response"] = map[string]any{"llmContent": "ok", "returnDisplay": "ok"}
		if out := e.agentHook("gemini-post", payload); strings.TrimSpace(out) != "{}" {
			t.Errorf("%s: AfterTool stdout %q", c.tool, out)
		}
		if list := e.mustOops("list"); !strings.Contains(list, "Gemini CLI") || !strings.Contains(list, c.tool+" "+c.tool+".txt") {
			t.Fatalf("%s: not listed:\n%s", c.tool, list)
		}
		e.mustOops("-y")
		sameFile(t, c.tool, want, stateOf(t, p))
	}
}

func TestCursorAfterFileEditUndo(t *testing.T) {
	e := newEnv(t)
	p := filepath.Join(e.work, "main.py")
	before := "def main():\n    print('one')\n\nmain()\n"
	writeFile(t, p, before, 0o644)
	after := strings.Replace(before, "print('one')", "print('two')\n    print('three')", 1)
	os.WriteFile(p, []byte(after), 0o644)
	payload := map[string]any{
		"conversation_id": "c1", "generation_id": "g1", "hook_event_name": "afterFileEdit",
		"workspace_roots": []string{e.work}, "file_path": p,
		"edits": []any{map[string]any{"old_string": "print('one')", "new_string": "print('two')\n    print('three')"}},
	}
	if out := e.agentHook("cursor-post", payload); strings.TrimSpace(out) != "{}" {
		t.Errorf("afterFileEdit stdout %q", out)
	}
	if list := e.mustOops("list"); !strings.Contains(list, "Cursor") || !strings.Contains(list, "Edit main.py") {
		t.Fatalf("not listed:\n%s", list)
	}
	e.mustOops("-y")
	if got, _ := os.ReadFile(p); string(got) != before {
		t.Fatalf("not restored: %q", got)
	}

	// When a replacement occurs twice its place is unknown: nothing is
	// guessed, and Cursor still gets its reply.
	q := filepath.Join(e.work, "twice.txt")
	os.WriteFile(q, []byte("x\nx\n"), 0o644)
	payload["file_path"] = q
	payload["edits"] = []any{map[string]any{"old_string": "y", "new_string": "x"}}
	if out := e.agentHook("cursor-post", payload); strings.TrimSpace(out) != "{}" {
		t.Errorf("afterFileEdit stdout %q", out)
	}
	if list := e.mustOops("list"); strings.Contains(list, "twice.txt") {
		t.Fatalf("an unrecoverable edit was saved:\n%s", list)
	}

	// afterShellExecution only finishes pending commands.
	shell := map[string]any{"conversation_id": "c1", "hook_event_name": "afterShellExecution", "command": "ls", "output": "", "duration": 5, "sandbox": false}
	if out := e.agentHook("cursor-post", shell); strings.TrimSpace(out) != "{}" {
		t.Errorf("afterShellExecution stdout %q", out)
	}
}

func TestCodexApplyPatchUndo(t *testing.T) {
	e := newEnv(t)
	a := filepath.Join(e.work, "src", "a.rs")
	b := filepath.Join(e.work, "old.txt")
	writeFile(t, a, "fn main() {}\n", 0o644)
	writeFile(t, b, "remove me\n", 0o600)
	wantA, wantB := stateOf(t, a), stateOf(t, b)
	patch := "*** Begin Patch\n*** Update File: src/a.rs\n@@\n-fn main() {}\n+fn main() { run() }\n*** Delete File: old.txt\n*** Add File: new.txt\n+hi\n*** End Patch\n"
	payload := map[string]any{"session_id": "x1", "turn_id": "t1", "cwd": e.work, "hook_event_name": "PreToolUse", "model": "gpt",
		"permission_mode": "default", "tool_name": "apply_patch", "tool_use_id": "call_1", "tool_input": map[string]any{"command": patch}}
	e.agentHook("codex", payload)
	os.WriteFile(a, []byte("fn main() { run() }\n"), 0o644)
	os.Remove(b)
	os.WriteFile(filepath.Join(e.work, "new.txt"), []byte("hi\n"), 0o644)
	payload["hook_event_name"] = "PostToolUse"
	payload["tool_response"] = "Success"
	e.agentHook("codex-post", payload)
	if list := e.mustOops("list"); !strings.Contains(list, "Codex") || !strings.Contains(list, "apply_patch src/a.rs, old.txt") {
		t.Fatalf("not listed:\n%s", list)
	}
	e.mustOops("-y")
	sameFile(t, "updated file", wantA, stateOf(t, a))
	sameFile(t, "deleted file", wantB, stateOf(t, b))

	// The shell post hook runs for Bash too and replies nothing.
	payload["tool_name"] = "Bash"
	payload["tool_input"] = map[string]any{"command": "ls"}
	if out := e.agentHook("codex-post", payload); strings.TrimSpace(out) != "" {
		t.Errorf("codex Bash post hook printed %q", out)
	}
}
