package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// psProfile is where oops init puts the PowerShell 7 hook under the fake home.
func (e *env) psProfile(name string) string {
	if !onWindows {
		return filepath.Join(e.home, ".config", "powershell", "profile.ps1")
	}
	if name == "windowspowershell" {
		return filepath.Join(e.home, "Documents", "WindowsPowerShell", "profile.ps1")
	}
	return filepath.Join(e.home, "Documents", "PowerShell", "profile.ps1")
}

// runPS runs a script in a fresh PowerShell that loads the given profile the
// way an interactive session would, then reads one line through
// PSConsoleHostReadLine and runs it. The stand in for PSReadLine returns
// line, so the hook sees exactly what a person typed.
func (e *env) runPS(t *testing.T, exe, profile, line string) string {
	t.Helper()
	script := "function PSConsoleHostReadLine { " + psq(line) + " }\n" +
		". " + psq(profile) + "\n" +
		"$typed = PSConsoleHostReadLine\n" +
		"Invoke-Expression $typed\n"
	c := exec.Command(exe, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	if !onWindows {
		c = exec.Command(exe, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
	}
	c.Dir = e.work
	c.Env = e.environ()
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", exe, err, out)
	}
	return string(out)
}

func psq(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func TestPowerShellHookRemoveItem(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("no pwsh")
	}
	e := newEnv(t)
	tree := filepath.Join(e.work, "project")
	makeTree(t, tree)
	before := snapshotTree(t, tree, true)

	out := e.mustOops("init", "--only", "powershell")
	if !strings.Contains(out, "PowerShell") {
		t.Fatalf("init: %s", out)
	}
	profile := e.psProfile("powershell")
	msg := e.runPS(t, pwsh, profile, "Remove-Item -Recurse -Force project")
	if !strings.Contains(msg, "saved snapshot 1") {
		t.Fatalf("PowerShell hook did not take a snapshot:\n%s", msg)
	}
	if _, err := os.Lstat(tree); err == nil {
		t.Fatal("Remove-Item did not run")
	}
	list := e.mustOops("list")
	if !strings.Contains(list, "PowerShell") || !strings.Contains(list, "Remove-Item -Recurse -Force project") {
		t.Fatalf("list: %s", list)
	}
	e.mustOops("-y")
	compareTrees(t, before, snapshotTree(t, tree, true))

	// Harmless lines never reach oops: no new snapshot, no output.
	if out := e.runPS(t, pwsh, profile, "Get-ChildItem | Out-Null"); strings.Contains(out, "oops") {
		t.Fatalf("harmless command produced: %s", out)
	}
	// Quotes and spaces survive the trip to the hook.
	os.WriteFile(filepath.Join(e.work, `my "notes".txt`), []byte("quoted\n"), 0o644)
	if onWindows {
		os.Remove(filepath.Join(e.work, `my "notes".txt`)) // not a legal Windows name
		os.WriteFile(filepath.Join(e.work, "my notes.txt"), []byte("quoted\n"), 0o644)
		e.runPS(t, pwsh, profile, `Remove-Item -LiteralPath "my notes.txt"`)
	} else {
		e.runPS(t, pwsh, profile, `Remove-Item -LiteralPath 'my "notes".txt'`)
	}
	if !strings.Contains(e.mustOops("list"), "notes") {
		t.Fatal("quoted path command was not snapshotted")
	}

	e.mustOops("uninstall")
	if _, err := os.Stat(profile); err == nil {
		t.Fatalf("profile should be gone after uninstall (it only held the hook)")
	}
}

// TestPowerShellProfileKeepsUserContent checks init and uninstall leave an
// existing profile byte for byte, including CRLF line endings.
func TestPowerShellProfileKeepsUserContent(t *testing.T) {
	if _, err := exec.LookPath("pwsh"); err != nil && !onWindows {
		t.Skip("no pwsh")
	}
	e := newEnv(t)
	profile := e.psProfile("powershell")
	os.MkdirAll(filepath.Dir(profile), 0o755)
	orig := "Import-Module posh-git\r\nSet-Alias ll Get-ChildItem\r\n"
	os.WriteFile(profile, []byte(orig), 0o644)
	e.mustOops("init", "--only", "powershell")
	e.mustOops("init", "--only", "powershell")
	b, _ := os.ReadFile(profile)
	if strings.Count(string(b), "# >>> oopsy >>>") != 1 || !strings.HasPrefix(string(b), orig) {
		t.Fatalf("profile not updated cleanly:\n%q", b)
	}
	if status := e.mustOops("status"); !strings.Contains(status, "PowerShell") {
		t.Fatalf("status: %s", status)
	}
	e.mustOops("uninstall")
	if b, _ := os.ReadFile(profile); string(b) != orig {
		t.Fatalf("profile not restored byte for byte:\n%q\nwant\n%q", b, orig)
	}
}
