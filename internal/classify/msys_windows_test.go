//go:build windows

package classify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMsysPaths(t *testing.T) {
	for in, want := range map[string]string{
		"/c/Users/me/app":    `C:\Users\me\app`,
		"/c":                 `C:\`,
		"/cygdrive/d/a/b":    `D:\a\b`,
		"/tmp/build":         filepath.Join(os.TempDir(), "build"),
		`C:\already\windows`: `C:\already\windows`,
		"relative/path":      "relative/path",
	} {
		if got := msysPath(in, `C:\work`); !strings.EqualFold(got, want) {
			t.Errorf("msysPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGitBashCommands covers what Claude Code sends from Git Bash on
// Windows: POSIX style /c/ paths, forward slash drive paths and quoted
// backslash paths all name the same folder.
func TestGitBashCommands(t *testing.T) {
	dir := fixture(t)
	env := baseEnv(dir)
	vol := filepath.VolumeName(dir)
	posix := "/" + strings.ToLower(vol[:1]) + filepath.ToSlash(dir[len(vol):])
	for _, cmd := range []string{
		"rm -rf " + posix + "/dir1",
		"rm -rf " + filepath.ToSlash(dir) + "/dir1",
		"rm -rf '" + dir + `\dir1'`,
		`rm -rf "` + strings.ReplaceAll(dir, `\`, `\\`) + `\\dir1"`,
		"cd " + posix + " && rm -rf dir1",
		"git -C " + posix + " clean -fdx",
	} {
		check(t, dir, env, tc{cmd: cmd, want: true})
		res := Classify(cmd, `C:\`, env)
		found := false
		for _, a := range res.Actions {
			for _, p := range a.Paths {
				found = found || strings.EqualFold(p, filepath.Join(dir, "dir1"))
			}
			if a.Git != nil && strings.EqualFold(a.Git.Repo, dir) {
				found = true
			}
		}
		if !found {
			t.Errorf("%q from C:\\ did not resolve to %s: %+v", cmd, filepath.Join(dir, "dir1"), res.Actions)
		}
	}
}
