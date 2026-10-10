package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// needTools skips a test when a tool is missing, except in CI jobs that set
// OOPS_TEST_REMOTE=1, where a missing tool is a failure: those jobs install
// everything, so a skip there would hide a broken setup.
func needTools(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			if os.Getenv("OOPS_TEST_REMOTE") == "1" {
				t.Fatalf("%s is not installed, but OOPS_TEST_REMOTE=1 says it should be", n)
			}
			t.Skipf("%s is not installed", n)
		}
	}
}

// tool runs a program in the work folder with the test environment.
func (e *env) tool(stdin string, name string, args ...string) (string, error) {
	c := exec.Command(name, args...)
	c.Dir = e.work
	c.Env = e.environ()
	c.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err := c.Run()
	if err != nil {
		return out.String() + errb.String(), err
	}
	return out.String(), nil
}

func (e *env) mustTool(name string, args ...string) string {
	e.t.Helper()
	out, err := e.tool("", name, args...)
	if err != nil {
		e.t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return out
}

// hookThenRun runs the zsh hook for line, then line itself, as a user's
// shell would, and returns the hook's message.
func (e *env) hookThenRun(line string) string {
	e.t.Helper()
	_, msg, err := e.oopsCmd("", "hook", "zsh", "--", line)
	if err != nil {
		e.t.Fatalf("hook: %v %s", err, msg)
	}
	e.sh(line)
	return msg
}

// logRecords prints what each snapshot record cost, for the docs.
func (e *env) logRecords() {
	e.t.Helper()
	var list []struct {
		ID      int `json:"id"`
		Records []struct {
			Kind  string `json:"kind"`
			Label string `json:"label"`
			Size  int64  `json:"size"`
		} `json:"records"`
	}
	out := e.mustOops("list", "--json")
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		e.t.Logf("list --json: %v", err)
		return
	}
	for _, s := range list {
		for _, r := range s.Records {
			e.t.Logf("snapshot %d record %s: %d bytes on disk: %s", s.ID, r.Kind, r.Size, r.Label)
		}
	}
}
