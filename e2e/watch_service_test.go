package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// oops watch on starts the daemon through the platform's service manager
// (a launchd agent, a systemd user service or a detached process, a
// scheduled task) under a test label, and oops watch off removes it all.
func TestWatchOnOff(t *testing.T) {
	e := newEnv(t)
	label := map[string]string{"darwin": "dev.oopsy.watch.test", "linux": "oopsy-watch-test", "windows": "OopsyWatchTest"}[runtime.GOOS]
	label += fmt.Sprint(os.Getpid())
	run := func(args ...string) (string, error) {
		c := exec.Command(bin, args...)
		c.Dir = e.work
		c.Env = append(e.environ(), "OOPS_WATCH_LABEL="+label)
		out, err := c.CombinedOutput()
		return string(out), err
	}
	// Whatever happens, leave nothing running or installed.
	t.Cleanup(func() { run("watch", "off") })

	out, err := run("watch", "on")
	if err != nil {
		if strings.Contains(out, "launchctl") || strings.Contains(out, "schtasks") {
			// CI runners without a logged in desktop session cannot load
			// a gui agent or run an interactive task.
			t.Skipf("the service manager refused here: %s", out)
		}
		t.Fatalf("watch on: %v\n%s", err, out)
	}
	t.Logf("watch on:\n%s", out)
	status, _ := run("watch", "status")
	if !strings.Contains(status, "running") || !strings.Contains(status, "resident") {
		t.Fatalf("watch status:\n%s", status)
	}
	t.Logf("watch status:\n%s", status)
	st := e.readWatchStatus()
	if st == nil || st.PID == 0 || st.RSS == 0 {
		t.Fatalf("status file: %+v", st)
	}
	pid := st.PID
	out, err = run("watch", "off")
	if err != nil || !strings.Contains(out, "off") {
		t.Fatalf("watch off: %v\n%s", err, out)
	}
	end := time.Now().Add(15 * time.Second)
	for processAlive(pid) && time.Now().Before(end) {
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Fatalf("daemon %d still runs after watch off", pid)
	}
	if status, _ := run("watch", "status"); !strings.Contains(status, "off") {
		t.Fatalf("status after off:\n%s", status)
	}
	switch runtime.GOOS {
	case "darwin":
		if exec.Command("launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), label)).Run() == nil {
			t.Fatal("launchd job still loaded after watch off")
		}
	case "windows":
		if exec.Command("schtasks", "/Query", "/TN", label).Run() == nil {
			t.Fatal("scheduled task still there after watch off")
		}
	}
}

func processAlive(pid int) bool {
	if onWindows {
		out, _ := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH").Output()
		return strings.Contains(string(out), fmt.Sprint(pid))
	}
	return exec.Command("kill", "-0", fmt.Sprint(pid)).Run() == nil
}
