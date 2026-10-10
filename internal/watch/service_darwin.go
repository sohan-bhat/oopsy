package watch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// macOS: a LaunchAgent in the user's gui domain, loaded with launchctl
// bootstrap. It starts at login and is restarted if it crashes.

func plistPath(name string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", name+".plist")
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

// plist is the LaunchAgent for the daemon.
func plist(name, exe string, args []string, log string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + xmlEscape(name) + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range append([]string{exe}, args...) {
		b.WriteString("\t\t<string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString(`	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>LowPriorityIO</key>
	<true/>
	<key>Nice</key>
	<integer>5</integer>
	<key>StandardOutPath</key>
	<string>` + xmlEscape(log) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(log) + `</string>
</dict>
</plist>
`)
	return b.String()
}

func guiDomain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func installService(exe string) (serviceInfo, string, error) {
	name := label("dev.oopsy.watch")
	p := plistPath(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return serviceInfo{}, "", err
	}
	if err := os.WriteFile(p, []byte(plist(name, exe, runArgs(), logPath())), 0o644); err != nil {
		return serviceInfo{}, "", err
	}
	_ = run("launchctl", "bootout", guiDomain()+"/"+name)
	if err := run("launchctl", "bootstrap", guiDomain(), p); err != nil {
		os.Remove(p)
		return serviceInfo{}, "", err
	}
	return serviceInfo{Manager: "launchd", Path: p}, "", nil
}

func removeService(si *serviceInfo) error {
	switch si.Manager {
	case "launchd":
		name := strings.TrimSuffix(filepath.Base(si.Path), ".plist")
		_ = run("launchctl", "bootout", guiDomain()+"/"+name)
		if err := os.Remove(si.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
