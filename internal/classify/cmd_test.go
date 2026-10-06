package classify

import (
	"path/filepath"
	"testing"
)

func TestCmdExe(t *testing.T) {
	dir := fixture(t)
	env := append(baseEnv(dir), "MYDIR="+filepath.Join(dir, "dir1"))
	for _, c := range []tc{
		{cmd: "del a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "del /q /f a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "DEL /F /Q a.txt", want: true},
		{cmd: "erase a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "@del a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "del *.log", want: true, paths: []string{"c.log", "d.log"}},
		{cmd: "del /s /q *.go", want: true, paths: []string{"sub/deeper/file.go", "src/main.go", "dst/main.go"}},
		{cmd: "del dir1", want: true, paths: []string{"dir1"}},
		{cmd: `del "space file.txt"`, want: true, paths: []string{"space file.txt"}},
		{cmd: "del a.txt,b.txt", want: true, paths: []string{"a.txt", "b.txt"}},
		{cmd: `del %MYDIR%\x`, want: true, paths: []string{"dir1/x"}},
		{cmd: `del %NOT_SET_ANYWHERE%\x`, want: true, broad: true},
		{cmd: "rd /s /q dir1", want: true, paths: []string{"dir1"}},
		{cmd: "rmdir emptydir", want: true, paths: []string{"emptydir"}},
		{cmd: "cd dir1 && del x", want: true, paths: []string{"dir1/x"}},
		{cmd: "cd /d dir1 & del y", want: true, paths: []string{"dir1/y"}},
		{cmd: "if exist a.txt del a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "if not exist z.txt (del b.txt) else (del a.txt)", want: true, paths: []string{"a.txt", "b.txt"}},
		{cmd: "for %f in (*.log) do del %f", want: true, paths: []string{"c.log", "d.log"}},
		{cmd: "for %%f in (a.txt b.txt) do del \"%%f\"", want: true, paths: []string{"a.txt", "b.txt"}},
		{cmd: "move /y b.txt a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "copy /y b.txt a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: `copy src\main.go dst`, want: true, paths: []string{"dst/main.go"}},
		{cmd: "copy a.txt+b.txt notes.md", want: true, paths: []string{"notes.md"}},
		{cmd: "echo x > a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "type a.txt > b.txt", want: true, paths: []string{"b.txt"}},
		{cmd: "dir 2> a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "robocopy src dst /MIR", want: true, paths: []string{"dst"}},
		{cmd: "robocopy src dst /MOVE", want: true, paths: []string{"dst", "src"}},
		{cmd: "xcopy src dst /y /e", want: true, paths: []string{"dst"}},
		{cmd: "git reset --hard", want: true, git: "reset-hard"},
		{cmd: "git clean -fdx", want: true, git: "clean"},
		{cmd: `powershell -NoProfile -Command "Remove-Item a.txt"`, want: true, paths: []string{"a.txt"}},
		{cmd: `cmd /c "del a.txt"`, want: true, paths: []string{"a.txt"}},
		{cmd: "echo x >> a.txt", want: false},
		{cmd: "dir > nul", want: false},
		{cmd: "echo del a.txt", want: false},
		{cmd: "copy a.txt brand-new.txt", want: false},
		{cmd: "move a.txt brand-new.txt", want: false},
		{cmd: "type a.txt", want: false},
		{cmd: "git status", want: false},
	} {
		checkWith(t, ClassifyCmd, dir, env, c)
	}
}

func TestCmdDeleteFlags(t *testing.T) {
	dir := fixture(t)
	for cmd, want := range map[string]bool{"del a.txt": true, "rd /s /q dir1": true, "copy /y b.txt a.txt": false, "echo x > a.txt": false} {
		res := ClassifyCmd(cmd, dir, baseEnv(dir))
		for _, a := range res.Actions {
			if a.Delete != want {
				t.Errorf("%q: Delete=%v, want %v", cmd, a.Delete, want)
			}
		}
	}
}
