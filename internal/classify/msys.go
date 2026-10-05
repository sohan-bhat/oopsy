package classify

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// msysPath turns a Git Bash (MSYS2) or Cygwin path into a Windows path:
// /c/Users/me and /cygdrive/c/Users/me become C:\Users\me, /tmp becomes the
// user's temp folder, and other absolute paths are asked of cygpath, since
// they live under the Git install. Windows paths pass through unchanged.
// Claude Code on Windows runs its Bash tool through Git Bash, so its
// commands mix both styles.
func msysPath(p, cwd string) string {
	if len(p) == 0 || p[0] != '/' || strings.HasPrefix(p, "//") {
		return p
	}
	rest := strings.TrimPrefix(p, "/cygdrive")
	if len(rest) >= 2 && rest[0] == '/' && isLetter(rest[1]) && (len(rest) == 2 || rest[2] == '/') {
		return strings.ToUpper(rest[1:2]) + `:\` + filepath.FromSlash(strings.TrimPrefix(rest[2:], "/"))
	}
	if p == "/tmp" || strings.HasPrefix(p, "/tmp/") {
		return filepath.Join(os.TempDir(), filepath.FromSlash(strings.TrimPrefix(p, "/tmp")))
	}
	if w := cygpath(p); w != "" {
		return w
	}
	// No cygpath: assume the default Git for Windows install.
	return filepath.Join(os.Getenv("ProgramFiles"), "Git", filepath.FromSlash(p))
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

var cygpathCache = map[string]string{}

func cygpath(p string) string {
	if w, ok := cygpathCache[p]; ok {
		return w
	}
	bin, err := exec.LookPath("cygpath")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-w", p).Output()
	w := ""
	if err == nil {
		w = strings.TrimSpace(string(out))
	}
	cygpathCache[p] = w
	return w
}
