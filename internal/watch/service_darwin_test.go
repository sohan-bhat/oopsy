package watch

import (
	"os/exec"
	"strings"
	"testing"
)

func TestPlist(t *testing.T) {
	p := plist("dev.oopsy.watch", "/opt/homebrew/bin/oops", []string{"watch", "run", "--home", "/tmp/a&b"}, "/tmp/log")
	for _, want := range []string{
		"<string>dev.oopsy.watch</string>",
		"<string>/opt/homebrew/bin/oops</string>\n\t\t<string>watch</string>\n\t\t<string>run</string>",
		"<string>/tmp/a&amp;b</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>SuccessfulExit</key>\n\t\t<false/>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist lacks %q:\n%s", want, p)
		}
	}
	if _, err := exec.LookPath("plutil"); err == nil {
		c := exec.Command("plutil", "-lint", "-")
		c.Stdin = strings.NewReader(p)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("plutil: %v %s", err, out)
		}
	}
}
