package watch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Linux: a systemd user service, enabled so it starts at login. Without a
// user systemd (containers, some CI runners, WSL without systemd) the
// daemon runs as a detached process instead, which does not come back
// after a reboot until oops watch on runs again.

func unitDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "systemd", "user")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user")
}

// unit is the systemd service file for the daemon.
func unit(exe string, args []string) string {
	quote := func(s string) string {
		if strings.ContainsAny(s, " \"'\\") {
			return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
		}
		return s
	}
	cmd := quote(exe)
	for _, a := range args {
		cmd += " " + quote(a)
	}
	return `[Unit]
Description=Oopsy watch: keeps the previous version of files you change

[Service]
ExecStart=` + cmd + `
Restart=on-failure
RestartSec=30
Nice=5
IOSchedulingClass=idle

[Install]
WantedBy=default.target
`
}

func userSystemd() bool {
	if os.Getenv("OOPS_WATCH_MANAGER") == "pid" {
		return false
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	return exec.Command("systemctl", "--user", "show-environment").Run() == nil
}

func installService(exe string) (serviceInfo, string, error) {
	if !userSystemd() {
		si, err := startDetached(exe)
		return si, "there is no user systemd here, so watch runs as a background process until you log out or restart; run oops watch on again after a restart", err
	}
	name := label("oopsy-watch") + ".service"
	p := filepath.Join(unitDir(), name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return serviceInfo{}, "", err
	}
	if err := os.WriteFile(p, []byte(unit(exe, runArgs())), 0o644); err != nil {
		return serviceInfo{}, "", err
	}
	_ = run("systemctl", "--user", "daemon-reload")
	err := run("systemctl", "--user", "enable", "--now", name)
	if err != nil {
		// The unit folder is not one this systemd reads (a custom HOME):
		// link the file in by its path.
		err = run("systemctl", "--user", "enable", "--now", p)
	}
	if err != nil {
		os.Remove(p)
		_ = run("systemctl", "--user", "daemon-reload")
		si, derr := startDetached(exe)
		return si, "systemd could not start the service (" + err.Error() + "), so watch runs as a background process instead", derr
	}
	return serviceInfo{Manager: "systemd", Path: p}, "", nil
}

func removeService(si *serviceInfo) error {
	if si.Manager != "systemd" {
		return nil
	}
	name := filepath.Base(si.Path)
	_ = run("systemctl", "--user", "disable", "--now", name)
	if err := os.Remove(si.Path); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = run("systemctl", "--user", "daemon-reload")
	return nil
}
