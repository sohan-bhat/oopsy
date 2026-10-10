package e2e

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestErrorMessagesGolden runs commands people get wrong and compares what
// oops says, and its exit code, with testdata/errors.golden. Regenerate
// with OOPS_UPDATE_GOLDEN=1 after a deliberate wording change.
func TestErrorMessagesGolden(t *testing.T) {
	e := newEnv(t)
	e.git("init", "-q", e.work)
	cases := [][]string{
		{"explain"},
		{"explain", "rm", "-rf"},
		{"explain", "git", "reset", "--hard"},
		{"explain", "ls", "-la"},
		{"lsit"},
		{"undoo", "3"},
		{"undo"},
		{"undo", "99"},
		{"undo", "x"},
		{"show", "7"},
		{"--yse"},
		{"list", "extra"},
		{"restore", "1"},
		{"config", "keep_days"},
		{"config", "nope", "1"},
		{},
		{"list"},
		{"db", "lsit"},
		{"db", "list", "--url", "postgres://u:p@127.0.0.1:1/shop"},
		{"db", "undo", "abc"},
	}
	var b strings.Builder
	for _, args := range cases {
		out, errOut, err := e.oopsCmd("", args...)
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
		text := strings.TrimSpace(out + errOut)
		text = strings.ReplaceAll(text, e.work, "$WORK")
		text = strings.ReplaceAll(text, filepath.ToSlash(e.work), "$WORK")
		fmt.Fprintf(&b, "$ oops %s\n%s\n[exit %d]\n\n", strings.Join(args, " "), text, code)
		for _, r := range text {
			if r == '–' || r == '—' {
				t.Errorf("oops %v printed a dash: %s", args, text)
			}
		}
		if strings.Contains(text, "arg(s)") || strings.Contains(text, "unknown command") || strings.Contains(text, "unknown flag") {
			t.Errorf("oops %v printed a raw cobra error: %s", args, text)
		}
	}
	golden := filepath.Join("testdata", "errors.golden")
	if os.Getenv("OOPS_UPDATE_GOLDEN") != "" {
		os.WriteFile(golden, []byte(b.String()), 0o644)
	}
	want, _ := os.ReadFile(golden)
	if got := b.String(); strings.ReplaceAll(string(want), "\r\n", "\n") != got {
		t.Errorf("messages changed; run with OOPS_UPDATE_GOLDEN=1 if that was deliberate\n--- got\n%s", got)
	}
}

// Runtime problems say what happened and what to do next.
func TestRuntimeErrorsAreExplained(t *testing.T) {
	e := newEnv(t)
	// A settings file that is not JSON.
	os.MkdirAll(e.oops, 0o700)
	os.WriteFile(filepath.Join(e.oops, "config.json"), []byte("{keep_days: 3"), 0o600)
	_, errOut, _ := e.oopsCmd("", "list")
	if !strings.Contains(errOut, "is not valid JSON") || !strings.Contains(errOut, "default settings") {
		t.Errorf("bad config not explained: %q", errOut)
	}
	os.Remove(filepath.Join(e.oops, "config.json"))

	// A restore conflict keeps both and says how to sort it out.
	f := filepath.Join(e.work, "plan.txt")
	os.WriteFile(f, []byte("v1\n"), 0o644)
	e.guarded("rm plan.txt")
	os.WriteFile(f, []byte("v2\n"), 0o644)
	out := e.mustOops("-y")
	if !strings.Contains(out, "nothing was overwritten") {
		t.Errorf("conflict not explained:\n%s", out)
	}

	// Permission denied on restore names the folder in the way.
	if runtime.GOOS != "windows" && os.Getuid() != 0 {
		d := filepath.Join(e.work, "locked")
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "a.txt"), []byte("a\n"), 0o644)
		e.guarded("rm locked/a.txt")
		os.Chmod(d, 0o555)
		defer os.Chmod(d, 0o755)
		out, errOut, err := e.oopsCmd("", "-y")
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 4 {
			t.Errorf("want exit 4 for a partial restore, got %v", err)
		}
		if !strings.Contains(out+errOut, "not writable by you") && !strings.Contains(out+errOut, "permission") {
			t.Errorf("permission problem not explained:\n%s%s", out, errOut)
		}
	}
}
