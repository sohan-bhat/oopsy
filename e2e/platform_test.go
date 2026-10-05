package e2e

import (
	"os"
	"os/exec"
	"runtime"
)

const onWindows = runtime.GOOS == "windows"

// exe adds the .exe suffix Windows needs.
func exe(name string) string {
	if onWindows {
		return name + ".exe"
	}
	return name
}

// posixShell is the shell the tests type commands into: /bin/sh on Unix and
// Git Bash on Windows, which is also what Claude Code uses there.
func posixShell() string {
	if !onWindows {
		return "/bin/sh"
	}
	return gitBash()
}

// gitBash finds Git for Windows' bash, not the WSL launcher in System32.
func gitBash() string {
	for _, p := range []string{os.Getenv("ProgramFiles") + `\Git\bin\bash.exe`, `C:\Program Files\Git\bin\bash.exe`} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	p, _ := exec.LookPath("bash")
	return p
}

func pathWith(dir string) string {
	return "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")
}
