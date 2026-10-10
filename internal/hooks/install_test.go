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
