package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows/registry"
)

const cmdKey = `Software\Microsoft\Command Processor`

type autoRunNow struct {
	exists bool
	value  string
	typ    uint32
}

func readAutoRun(t *testing.T) autoRunNow {
	t.Helper()
	k, err := registry.OpenKey(registry.CURRENT_USER, cmdKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return autoRunNow{}
	}
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	v, typ, err := k.GetStringValue("AutoRun")
	if errors.Is(err, registry.ErrNotExist) {
		return autoRunNow{}
	}
	if err != nil {
		t.Fatal(err)
	}
	return autoRunNow{true, v, typ}
}

func writeAutoRun(a autoRunNow) {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, cmdKey, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	switch {
	case !a.exists:
		k.DeleteValue("AutoRun")
	case a.typ == registry.EXPAND_SZ:
		k.SetExpandStringValue("AutoRun", a.value)
	default:
		k.SetStringValue("AutoRun", a.value)
	}
}

// keepAutoRun sets AutoRun for the test and puts the runner's own value
// back afterwards whatever happens.
func keepAutoRun(t *testing.T, during autoRunNow) autoRunNow {
	orig := readAutoRun(t)
	t.Cleanup(func() { writeAutoRun(orig) })
	writeAutoRun(during)
	return during
}

type listed struct {
	ID       int    `json:"id"`
	Command  string `json:"command"`
	Restored any    `json:"restored"`
}

// waitSnapshots waits until a snapshot exists for every command in want.
func (e *env) waitSnapshots(t *testing.T, dir string, want ...string) map[string]int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, _, _ := e.inDir(dir).oopsCmd("", "list", "--json")
		var list []listed
		json.Unmarshal([]byte(out), &list)
		got := map[string]int{}
		for _, s := range list {
			for _, w := range want {
				if strings.Contains(s.Command, w) && got[w] == 0 {
					got[w] = s.ID
				}
			}
		}
		if len(got) == len(want) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("snapshots for %q never appeared; list:\n%s", want, out)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestCmdHookTypedIntoCmd installs the cmd.exe hook into the real AutoRun
// value, types destructive commands into an interactive cmd.exe on a pseudo
// console, and restores everything they removed.
func TestCmdHookTypedIntoCmd(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed")
	}
	e := newEnv(t)
	// A value already there, as Clink or a user would leave it.
	orig := keepAutoRun(t, autoRunNow{true, `set "OOPSY_TEST_PREV=1"`, registry.SZ})
	root := e.project(t)
	os.WriteFile(filepath.Join(root, "notes.txt"), []byte("keep me\r\nline two\r\n"), 0o644)
	e.mustOops("mirror", "build", root)
	before := without(snapshotTree(t, root, false), ".git")

	envs := append(e.environ(), "OOPS_CLINK=none")
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
	if !strings.Contains(out, "cmd.exe") {
		t.Fatalf("init: %s", out)
	}
	now := readAutoRun(t)
	if !strings.HasSuffix(now.value, " & "+orig.value) || !strings.Contains(now.value, "autorun.cmd") {
		t.Fatalf("AutoRun not chained: %q", now.value)
	}
	if s := run("status"); !strings.Contains(s, "cmd.exe") || !strings.Contains(s, "on") {
		t.Fatalf("status: %s", s)
	}
	if d := run("doctor"); !strings.Contains(d, "cmd.exe hook") {
		t.Fatalf("doctor: %s", d)
	}

	// cmd /c, which scripts and programs use, is untouched: same output,
	// nothing printed, and the previous AutoRun still runs.
	for line, want := range map[string]string{
		`echo hi`:                "hi\r\n",
		`echo a& echo b`:         "a\r\nb\r\n",
		`echo %OOPSY_TEST_PREV%`: "1\r\n",
	} {
		c := exec.Command("cmd.exe", "/c", line)
		c.Dir, c.Env = root, envs
		got, _ := c.CombinedOutput()
		if string(got) != want {
			t.Errorf("cmd /c %s: got %q want %q", line, got, want)
		}
	}

	con := startConPTY(t, "cmd.exe", root, envs)
	con.Wait(">", 20*time.Second)
	con.Type("echo hook=%OOPSY_SHELL_HOOK% prev=%OOPSY_TEST_PREV%\r")
	con.Wait("hook=cmd prev=1", 10*time.Second)

	con.Type("del /q notes.txt\r")
	con.Wait("saved snapshot", 30*time.Second)
	con.Type("set /a 6*7\r")
	con.Wait("42", 30*time.Second)
	if _, err := os.Stat(filepath.Join(root, "notes.txt")); err == nil {
		t.Fatalf("del did not run:\n%s", con.Output())
	}

	con.Type(`node -e "require('fs').rmSync('build',{recursive:true})"` + "\r")
	con.Type("set /a 6*8\r")
	con.Wait("48", 60*time.Second)
	if _, err := os.Stat(filepath.Join(root, "build")); err == nil {
		t.Fatalf("node did not run:\n%s", con.Output())
	}
	con.Type("exit\r")
	if !con.Exited(10 * time.Second) {
		t.Fatalf("cmd.exe did not exit:\n%s", con.Output())
	}
	t.Logf("cmd.exe screen:\n%s", con.Output())

	ids := e.waitSnapshots(t, root, "del /q notes.txt", "rmSync")
	for _, w := range []string{"rmSync", "del /q notes.txt"} {
		out := run("undo", "-y", fmt.Sprint(ids[w]))
		t.Logf("undo %s: %s", w, out)
	}
	compareTrees(t, before, without(snapshotTree(t, root, false), ".git"))
	if b, _ := os.ReadFile(filepath.Join(root, "notes.txt")); string(b) != "keep me\r\nline two\r\n" {
		t.Fatalf("notes.txt: %q", b)
	}

	run("uninstall")
	if got := readAutoRun(t); got != orig {
		t.Fatalf("AutoRun after uninstall %+v, want %+v", got, orig)
	}
	if _, err := os.Stat(filepath.Join(e.oops, "cmd")); err == nil {
		t.Fatal("cmd files left after uninstall")
	}
}

// TestCmdUninstallWithoutPreviousAutoRun deletes the value it created.
func TestCmdUninstallWithoutPreviousAutoRun(t *testing.T) {
	e := newEnv(t)
	keepAutoRun(t, autoRunNow{})
	envs := append(e.environ(), "OOPS_CLINK=none")
	for _, args := range [][]string{{"init", "--only", "cmd"}, {"init", "--only", "cmd"}, {"uninstall"}} {
		c := exec.Command(bin, args...)
		c.Dir, c.Env = e.work, envs
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("oops %v: %v\n%s", args, err, out)
		}
		if args[0] == "init" && !readAutoRun(t).exists {
			t.Fatal("init did not set AutoRun")
		}
	}
	if got := readAutoRun(t); got.exists {
		t.Fatalf("AutoRun left behind: %q", got.value)
	}
}
