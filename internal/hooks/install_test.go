package hooks

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProfileRoundTrip installs and removes the hook in profiles saved the
// ways Windows editors save them, and expects every byte back.
func TestProfileRoundTrip(t *testing.T) {
	cases := map[string][]byte{
		"empty":    nil,
		"lf":       []byte("Set-Alias ll Get-ChildItem\n"),
		"crlf":     []byte("Import-Module posh-git\r\nSet-Alias ll Get-ChildItem\r\n"),
		"utf8 bom": append([]byte{0xEF, 0xBB, 0xBF}, "$env:EDITOR = 'code'\r\n"...),
		"utf16":    encodeText("# prompt\r\nfunction prompt { 'PS> ' }\r\n", encUTF16LE),
	}
	for name, orig := range cases {
		dir := t.TempDir()
		p := filepath.Join(dir, "profile.ps1")
		if orig != nil {
			os.WriteFile(p, orig, 0o644)
		}
		tgt := Target{Name: "powershell", Kind: "shell", Path: p}
		if err := installShell(tgt, `C:/Tools/oops.exe`); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, _ := os.ReadFile(p)
		text, enc := decodeText(got)
		if !installed(tgt) || strings.Count(text, blockStart) != 1 {
			t.Fatalf("%s: hook not installed:\n%q", name, got)
		}
		if _, want := decodeText(orig); orig != nil && enc != want {
			t.Errorf("%s: encoding changed from %d to %d", name, want, enc)
		}
		if strings.Contains(string(orig), "\r\n") && strings.Count(text, "\n") != strings.Count(text, "\r\n") {
			t.Errorf("%s: mixed line endings:\n%q", name, text)
		}
		if !strings.Contains(text, `& 'C:/Tools/oops.exe' shell-init powershell`) {
			t.Errorf("%s: unexpected hook line:\n%s", name, text)
		}
		out := removeBlock(got)
		if orig == nil {
			if strings.TrimSpace(installedText(out)) != "" {
				t.Errorf("%s: leftovers %q", name, out)
			}
			continue
		}
		if !bytes.Equal(out, orig) {
			t.Errorf("%s: not restored byte for byte:\n%q\nwant\n%q", name, out, orig)
		}
	}
}

func TestPowerShellInitQuoting(t *testing.T) {
	s := powerShellInit(`C:\Users\O'Brien\oops.exe`)
	if !strings.Contains(s, `'C:\Users\O''Brien\oops.exe'`) {
		t.Fatalf("path not quoted for PowerShell:\n%s", s)
	}
	if !strings.Contains(s, "PSConsoleHostReadLine") || !strings.Contains(s, "OOPSY_LINE") {
		t.Fatal("hook does not wrap PSConsoleHostReadLine")
	}
}

// TestAgentHooksRoundTrip installs every agent's pre and post hooks next to
// the user's own, twice, and expects uninstall to leave only theirs.
func TestAgentHooksRoundTrip(t *testing.T) {
	user := map[string]string{
		"claude": `{"model":"opus","hooks":{"PreToolUse":[{"matcher":"Edit","hooks":[{"type":"command","command":"my-linter"}]}],"PostToolUse":[{"matcher":"Write","hooks":[{"type":"command","command":"prettier"}]}]}}`,
		"codex":  `{"hooks":{"PostToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"log-it"}]}]}}`,
		"gemini": `{"theme":"dark","hooks":{"AfterTool":[{"matcher":"read_file","hooks":[{"name":"x","type":"command","command":"audit"}]}]}}`,
		"cursor": `{"version":1,"hooks":{"afterFileEdit":[{"command":"format.sh"}]}}`,
	}
	want := map[string][]string{
		"claude": {"PreToolUse", "PostToolUse"},
		"codex":  {"PreToolUse", "PostToolUse"},
		"gemini": {"BeforeTool", "AfterTool"},
		"cursor": {"beforeShellExecution", "afterShellExecution", "afterFileEdit"},
	}
	for name, orig := range user {
		p := filepath.Join(t.TempDir(), "settings.json")
		os.WriteFile(p, []byte(orig), 0o644)
		for i := 0; i < 2; i++ {
			if err := editJSON(p, func(o *obj) { removeAgentHook(o, name); addAgentHook(o, name, "/usr/local/bin/oops") }); err != nil {
				t.Fatal(err)
			}
		}
		b, _ := os.ReadFile(p)
		text := string(b)
		for _, ev := range want[name] {
			if !strings.Contains(text, `"`+ev+`"`) {
				t.Errorf("%s: no %s hook:\n%s", name, ev, text)
			}
		}
		if n := strings.Count(text, " hook "+name+"-post"); n != len(want[name])-1 {
			t.Errorf("%s: %d post hooks, want %d:\n%s", name, n, len(want[name])-1, text)
		}
		if n := strings.Count(text, " hook "+name+`"`); n != 1 {
			t.Errorf("%s: %d pre hooks, want 1:\n%s", name, n, text)
		}
		switch name {
		case "claude":
			if !strings.Contains(text, "Bash|Edit|Write|MultiEdit|NotebookEdit") && !strings.Contains(text, "Bash|PowerShell|Edit|Write|MultiEdit|NotebookEdit") {
				t.Errorf("claude matcher misses edit tools:\n%s", text)
			}
		case "codex":
			if !strings.Contains(text, "apply_patch") {
				t.Errorf("codex matcher misses apply_patch:\n%s", text)
			}
		case "gemini":
			if !strings.Contains(text, "write_file|replace") {
				t.Errorf("gemini matcher misses edit tools:\n%s", text)
			}
		}
		if err := editJSON(p, func(o *obj) { removeAgentHook(o, name) }); err != nil {
			t.Fatal(err)
		}
		b, _ = os.ReadFile(p)
		got, _ := parseObj(b)
		exp, _ := parseObj([]byte(orig))
		gb, _ := got.MarshalJSON()
		eb, _ := exp.MarshalJSON()
		if string(gb) != string(eb) {
			t.Errorf("%s: not restored:\n%s\nwant\n%s", name, gb, eb)
		}
	}
}

func TestPatchPaths(t *testing.T) {
	patch := "*** Begin Patch\n*** Update File: src/a.go\n@@\n-x\n+y\n*** Add File: b.txt\n+hi\n*** Delete File: c.txt\n*** Update File: d.go\n*** Move to: e.go\n*** End Patch\n"
	got := strings.Join(patchPaths(patch), ",")
	if got != "src/a.go,b.txt,c.txt,d.go,e.go" {
		t.Fatalf("patchPaths = %s", got)
	}
}

func TestReverseEdits(t *testing.T) {
	cur := []byte("func a() {\n\treturn 2\n}\nfunc b() {}\n")
	edits := []edit{{OldString: "return 1", NewString: "return 2"}, {OldString: "func c() {}", NewString: "func b() {}"}}
	prev, ok := reverseEdits(cur, edits)
	if !ok || string(prev) != "func a() {\n\treturn 1\n}\nfunc c() {}\n" {
		t.Fatalf("reverse = %q %v", prev, ok)
	}
	if out, ok := applyEdits(prev, edits); !ok || !bytes.Equal(out, cur) {
		t.Fatalf("forward = %q %v", out, ok)
	}
	// A replacement that occurs twice cannot be located.
	if _, ok := reverseEdits([]byte("x x"), []edit{{OldString: "y", NewString: "x"}}); ok {
		t.Fatal("ambiguous edit reversed")
	}
}
