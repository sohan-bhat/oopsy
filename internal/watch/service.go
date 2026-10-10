package watch

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sohan-bhat/oopsy/internal/snap"
)

// label names the service: dev.oopsy.watch for launchd, oopsy-watch for
// systemd, OopsyWatch for the task scheduler. Tests set OOPS_WATCH_LABEL so
// they never touch a real installation.
func label(def string) string {
	if v := os.Getenv("OOPS_WATCH_LABEL"); v != "" {
		return v
	}
	return def
}

// exePath is the oops binary the service runs. A link on PATH that points
// at this binary (such as Homebrew's) is preferred, since it survives
// upgrades.
func exePath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(self)
	if err != nil {
		real = self
	}
	if p, err := exec.LookPath("oops"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			if r, _ := filepath.EvalSymlinks(abs); r == real {
				return abs, nil
			}
		}
	}
	return real, nil
}

// runArgs are the daemon's arguments. The store location is passed on the
// command line, since a scheduled task has no environment of its own.
func runArgs() []string {
	args := []string{"watch", "run"}
	if h := os.Getenv("OOPS_HOME"); h != "" {
		args = append(args, "--home", h)
	}
	return args
}

// Installed reports whether oops watch on set up a service.
func Installed() bool { return readService() != nil }

// Started describes how the daemon was started, for status.
type Started struct {
	Manager string // launchd, systemd, task or pid
	Path    string
	Since   time.Time
}

// Service returns how oops watch on started the daemon, or nil.
func Service() *Started {
	si := readService()
	if si == nil {
		return nil
	}
	return &Started{Manager: si.Manager, Path: si.Path, Since: si.Since}
}

// On installs and starts the service. It returns how the daemon was
// started and a note when it fell back to something weaker.
func On() (*Started, string, error) {
	exe, err := exePath()
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return nil, "", err
	}
	if si := readService(); si != nil {
		_ = removeService(si)
	}
	stopStray()
	si, note, err := installService(exe)
	if err != nil {
		return nil, "", err
	}
	si.Exe, si.Since = exe, time.Now()
	if err := writeService(si); err != nil {
		return nil, "", err
	}
	return &Started{Manager: si.Manager, Path: si.Path, Since: si.Since}, note, nil
}

// WaitRunning waits for the daemon to report in.
func WaitRunning(d time.Duration) (*Status, bool) {
	end := time.Now().Add(d)
	for {
		st, ok := Running()
		if ok || time.Now().After(end) {
			return st, ok
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Off stops the daemon and removes everything On added. It reports
// whether there was anything to remove.
func Off() (bool, error) {
	si := readService()
	var err error
	if si != nil {
		err = removeService(si)
	}
	stopped := stopStray()
	_ = os.Remove(servicePath())
	_ = os.Remove(statusPath())
	snap.DropWatchBases()
	return si != nil || stopped, err
}

// stopStray stops a daemon still running from the pid file, such as one
// started by hand or left over by a service manager.
func stopStray() bool {
	b, err := os.ReadFile(pidPath())
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 || pid == os.Getpid() || !alive(pid) {
		_ = os.Remove(pidPath())
		return false
	}
	_ = stopPID(pid)
	for i := 0; i < 50 && alive(pid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	_ = os.Remove(pidPath())
	return true
}

// startDetached runs the daemon as a plain background process, for systems
// without a service manager the user can reach.
func startDetached(exe string) (serviceInfo, error) {
	logf, err := os.OpenFile(logPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return serviceInfo{}, err
	}
	defer logf.Close()
	c := exec.Command(exe, runArgs()...)
	c.Stdout, c.Stderr = logf, logf
	detach(c)
	if err := c.Start(); err != nil {
		return serviceInfo{}, err
	}
	pid := c.Process.Pid
	_ = c.Process.Release()
	return serviceInfo{Manager: "pid", Path: pidPath() + " (" + strconv.Itoa(pid) + ")"}, nil
}

// run runs a service manager command and folds its output into the error.
func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return fmt.Errorf("%s %s: %v", name, strings.Join(args, " "), err)
		}
		return errors.New(name + ": " + msg)
	}
	return nil
}
