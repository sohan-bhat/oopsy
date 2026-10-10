package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChainAutoRun(t *testing.T) {
	ours := cmdAutoRunEntry(`C:\Users\a b\.oops\cmd\autorun.cmd`)
	cases := []string{
		"",
		`"C:\Program Files (x86)\clink\clink.bat" inject --autorun`,
		`doskey /macrofile=%USERPROFILE%\macros.txt & cls`,
		"  ",
	}
	for _, old := range cases {
		got := chainAutoRun(old, ours)
		if strings.TrimSpace(old) == "" && got != ours {
			t.Errorf("empty %q: got %q", old, got)
		}
		if strings.TrimSpace(old) != "" && got != ours+" & "+old {
			t.Errorf("chain %q: got %q", old, got)
		}
		back, ok := unchainAutoRun(got, ours)
		if !ok {
			t.Errorf("unchain %q: ours not found", got)
		}
		if strings.TrimSpace(old) != "" && back != old {
			t.Errorf("unchain %q: got %q want %q", got, back, old)
		}
	}
	// Another tool chained itself in front of ours after we installed.
	other := `"C:\clink\clink.bat" inject --autorun`
	cur := other + " & " + chainAutoRun("cls", ours)
	if back, ok := unchainAutoRun(cur, ours); !ok || back != other+" & cls" {
		t.Errorf("unchain from the middle: %q", back)
	}
	cur = "cls & " + ours
	if back, ok := unchainAutoRun(cur, ours); !ok || back != "cls" {
		t.Errorf("unchain from the end: %q", back)
	}
	if _, ok := unchainAutoRun("cls", ours); ok {
		t.Error("found ours where it is not")
	}
}

func TestCmdFiles(t *testing.T) {
	macros := cmdMacros(`C:\Users\x$y\.oops\cmd\hook.cmd`, `C:\Users\x$y\.oops\cmd\hookcall.cmd`)
	for _, want := range []string{
		"del=\"C:\\Users\\x$$y\\.oops\\cmd\\hook.cmd\" del $*\r\n",
		"rd=\"C:\\Users\\x$$y\\.oops\\cmd\\hook.cmd\" rd $*\r\n",
		"npm=\"C:\\Users\\x$$y\\.oops\\cmd\\hookcall.cmd\" npm $*\r\n",
	} {
		if !strings.Contains(macros, want) {
			t.Errorf("macros missing %q:\n%s", want, macros)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(macros), "\r\n") {
		name, _, ok := strings.Cut(line, "=")
		if !ok || strings.ContainsAny(name, " \"") {
			t.Errorf("bad macro line %q", line)
		}
	}

	hook := cmdHookBatch(`C:\Program Files\oops\oops.exe`, false)
	for _, want := range []string{
		"@set \"OOPSY_LINE=%*\"\r\n",
		"@if not defined OOPSY_CLINK \"C:\\Program Files\\oops\\oops.exe\" hook cmd\r\n",
		"\r\n@%*\r\n",
		"hook cmd-post",
	} {
		if !strings.Contains(hook, want) {
			t.Errorf("hook batch missing %q:\n%s", want, hook)
		}
	}
	if !strings.Contains(cmdHookBatch("oops", true), "\r\n@call %*\r\n") {
		t.Error("call batch does not use call")
	}
	if strings.Contains(strings.ReplaceAll(hook, "\r\n", ""), "\n") {
		t.Error("batch file has bare LF line endings")
	}

	auto := cmdAutoRunBatch(`C:\h\.oops\cmd\macros.txt`)
	for _, want := range []string{"!CMDCMDLINE:/c=!", "exit /b 0", `doskey /macrofile="C:\h\.oops\cmd\macros.txt"`} {
		if !strings.Contains(auto, want) {
			t.Errorf("autorun batch missing %q:\n%s", want, auto)
		}
	}
	if i, j := strings.Index(auto, "/c=!"), strings.Index(auto, "doskey"); i > j {
		t.Error("macros load before the cmd /c check")
	}
}

func TestCmdQuote(t *testing.T) {
	for in, want := range map[string]string{
		"oops":                 "oops",
		`C:/Tools/oops.exe`:    `C:/Tools/oops.exe`,
		`C:\Program Files\oop`: `"C:\Program Files\oop"`,
		`C:\a&b\oops.exe`:      `"C:\a&b\oops.exe"`,
	} {
		if got := cmdQuote(in); got != want {
			t.Errorf("cmdQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClinkScript(t *testing.T) {
	s := clinkScript(`C:\Program Files\oops\oops.exe`)
	for _, want := range []string{
		`local bin = [["C:\Program Files\oops\oops.exe"]]`,
		"clink.onendedit(function(line)",
		`os.execute(bin .. " hook cmd")`,
		"hook cmd-post",
		`["dir"]=true`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, `["del"]`) {
		t.Error("del must not be read only")
	}
	if got := luaLong("a]]b"); got != "[=[a]]b]=]" {
		t.Errorf("luaLong: %s", got)
	}
}

type fakeAutoRun struct{ v autoRunValue }

func (f *fakeAutoRun) get() (autoRunValue, error) { return f.v, nil }
func (f *fakeAutoRun) set(v autoRunValue) error {
	if !v.Exists {
		v = autoRunValue{}
	}
	f.v = v
	return nil
}

// TestCmdInstallRoundTrip installs and uninstalls over the AutoRun values
// people really have and expects each back exactly.
func TestCmdInstallRoundTrip(t *testing.T) {
	cases := map[string]autoRunValue{
		"none":   {},
		"empty":  {Exists: true},
		"clink":  {Exists: true, Value: `"C:\Program Files (x86)\clink\clink.bat" inject --autorun`},
		"expand": {Exists: true, Value: `doskey /macrofile=%USERPROFILE%\m.txt`, Expand: true},
	}
	for name, orig := range cases {
		h := t.TempDir()
		t.Setenv("HOME", h)
		t.Setenv("USERPROFILE", h)
		t.Setenv("OOPS_HOME", filepath.Join(h, ".oops"))
		t.Setenv("OOPS_CLINK", "none")
		f := &fakeAutoRun{v: orig}
		saved := autoRun
		autoRun = f
		bk, err := installCmd("oops")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if bk == "" || !cmdInstalled() {
			t.Fatalf("%s: not installed (backup %q, value %q)", name, bk, f.v.Value)
		}
		if !strings.HasPrefix(f.v.Value, cmdEntry()) || f.v.Expand != orig.Expand {
			t.Fatalf("%s: value %+v", name, f.v)
		}
		for _, p := range []string{cmdAutoRunFile(), cmdHookFile(), cmdCallFile(), cmdMacroFile()} {
			if _, err := os.Stat(p); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		// A second init leaves AutoRun alone.
		before := f.v
		if _, err := installCmd("oops"); err != nil || f.v != before {
			t.Fatalf("%s: second install changed AutoRun to %+v (%v)", name, f.v, err)
		}
		if _, err := uninstallCmd(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if f.v != orig {
			t.Fatalf("%s: AutoRun after uninstall %+v, want %+v", name, f.v, orig)
		}
		if _, err := os.Stat(cmdDir()); err == nil {
			t.Fatalf("%s: %s left behind", name, cmdDir())
		}
		autoRun = saved
	}
}

// TestCmdUninstallAfterOtherChange removes only our entry when another
// tool changed AutoRun after oops init.
func TestCmdUninstallAfterOtherChange(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("OOPS_HOME", filepath.Join(h, ".oops"))
	t.Setenv("OOPS_CLINK", "none")
	f := &fakeAutoRun{v: autoRunValue{Exists: true, Value: "cls"}}
	saved := autoRun
	autoRun = f
	defer func() { autoRun = saved }()
	if _, err := installCmd("oops"); err != nil {
		t.Fatal(err)
	}
	f.v.Value = `"C:\clink\clink.bat" inject --autorun & ` + f.v.Value
	if _, err := uninstallCmd(); err != nil {
		t.Fatal(err)
	}
	if want := `"C:\clink\clink.bat" inject --autorun & cls`; f.v.Value != want {
		t.Fatalf("got %q want %q", f.v.Value, want)
	}
}
