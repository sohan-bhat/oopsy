package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCmdHookWithClink injects Clink into an interactive cmd.exe and types
// a chained line no doskey macro can see. CI downloads Clink's release zip
// and sets OOPS_TEST_CLINK to its folder.
func TestCmdHookWithClink(t *testing.T) {
	dir := os.Getenv("OOPS_TEST_CLINK")
	clink := filepath.Join(dir, "clink_x64.exe")
	if _, err := os.Stat(clink); dir == "" || err != nil {
		t.Skip("set OOPS_TEST_CLINK to a folder with Clink's release files")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed")
	}
	e := newEnv(t)
	orig := keepAutoRun(t, autoRunNow{})
	root := e.project(t)
	os.WriteFile(filepath.Join(root, "notes.txt"), []byte("clink notes\r\n"), 0o644)
	e.mustOops("mirror", "build", root)
	before := without(snapshotTree(t, root, false), ".git")

	envs := append(e.environ(), "OOPS_CLINK="+clink, "CLINK_NOAUTORUN=1")
	run := func(args ...string) string {
		c := exec.Command(bin, args...)
		c.Dir, c.Env = root, envs
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("oops %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	out := run("init", "--only", "cmd")
	if !strings.Contains(out, "Clink is installed") {
		t.Fatalf("init did not find Clink: %s", out)
	}
	t.Cleanup(func() { exec.Command(clink, "uninstallscripts", filepath.Join(e.oops, "cmd", "clink")).Run() })
	if _, err := os.Stat(filepath.Join(e.oops, "cmd", "clink", "oopsy.lua")); err != nil {
		t.Fatal(err)
	}

	profile := filepath.Join(e.home, "clink-profile")
	os.MkdirAll(profile, 0o755)
	con := startConPTY(t, "cmd.exe", root, envs)
	con.Wait(">", 20*time.Second)
	con.Type(clink + " inject --quiet --profile " + profile + "\r")
	con.Type("echo hook=%OOPSY_SHELL_HOOK%.\r")
	con.Wait("hook=cmd (Clink).", 30*time.Second)

	// The line starts with echo, so only Clink sees the del after &.
	con.Type("echo first & del /q notes.txt\r")
	con.Type("set /a 6*7\r")
	con.Wait("42", 30*time.Second)
	if _, err := os.Stat(filepath.Join(root, "notes.txt")); err == nil {
		t.Fatalf("del did not run:\n%s", con.Output())
	}
	con.Type(`node -e "require('fs').rmSync('build',{recursive:true})"` + "\r")
	con.Type("set /a 6*8\r")
	con.Wait("48", 60*time.Second)
	con.Type("exit\r")
	if !con.Exited(10 * time.Second) {
		t.Fatalf("cmd.exe did not exit:\n%s", con.Output())
	}
	t.Logf("cmd.exe screen:\n%s", con.Output())

	ids := e.waitSnapshots(t, root, "del /q notes.txt", "rmSync")
	if ids["del /q notes.txt"] == ids["rmSync"] {
		t.Fatalf("one snapshot for both lines: %v", ids)
	}
	for _, w := range []string{"rmSync", "del /q notes.txt"} {
		t.Logf("undo %s: %s", w, run("undo", "-y", fmt.Sprint(ids[w])))
	}
	compareTrees(t, before, without(snapshotTree(t, root, false), ".git"))

	run("uninstall")
	if got := readAutoRun(t); got != orig {
		t.Fatalf("AutoRun after uninstall %+v, want %+v", got, orig)
	}
}
