package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sohan-bhat/oopsy/internal/snap"
)

// Time limits. Captures run before the user's command, so they are short:
// a slow or unreachable cluster must never hold the command up for long.
// Restores are interactive and can wait longer.
const (
	captureTimeout = 15 * time.Second
	restoreTimeout = 2 * time.Minute
)

// tool is how to run a program the way the original command ran it.
type tool struct {
	Bin     string   `json:"bin"`               // program, a path when it was resolved
	Name    string   `json:"name"`              // kubectl, terraform and so on
	Globals []string `json:"globals,omitempty"` // flags such as --context=prod
	Assigns []string `json:"assigns,omitempty"` // VAR=value from the command line
	Cwd     string   `json:"cwd"`
	env     []string
}

func newTool(c call, globals []string) tool {
	bin := c.argv[0]
	if strings.ContainsAny(bin, `/\`) {
		bin = absIn(c.cwd, bin)
	} else if p, err := exec.LookPath(bin); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			bin = abs
		}
	}
	return tool{Bin: bin, Name: progName(c.argv[0]), Globals: globals, Assigns: c.assigns, Cwd: c.cwd, env: c.env}
}

// environ is the environment for running the tool now. At restore time the
// current environment is used, with the command line's assignments on top.
func (t tool) environ() []string {
	if t.env != nil {
		return t.env
	}
	return append(os.Environ(), t.Assigns...)
}

func (t tool) bin() string {
	if _, err := os.Stat(t.Bin); err == nil || !strings.ContainsAny(t.Bin, `/\`) {
		return t.Bin
	}
	return t.Name // moved since; try PATH
}

// run runs the tool with its global flags first, then args.
func (t tool) run(timeout time.Duration, stdin io.Reader, extraEnv []string, args ...string) ([]byte, error) {
	return t.runTo(timeout, stdin, nil, extraEnv, args...)
}

// runTo is run with stdout going to w (when w is not nil).
func (t tool) runTo(timeout time.Duration, stdin io.Reader, w io.Writer, extraEnv []string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.bin(), append(append([]string(nil), t.Globals...), args...)...)
	cmd.Dir = t.Cwd
	cmd.Env = append(append([]string(nil), t.environ()...), extraEnv...)
	cmd.WaitDelay = 3 * time.Second
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	if w != nil {
		cmd.Stdout = w
	} else {
		cmd.Stdout = &out
	}
	cmd.Stderr = &errb
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return out.Bytes(), fmt.Errorf("%s did not answer within %s", t.Name, timeout)
	}
	if err != nil {
		var nf *exec.Error
		if errors.As(err, &nf) {
			return out.Bytes(), fmt.Errorf("%s is not installed or not on PATH", t.Name)
		}
		msg := firstLines(errb.String(), 3)
		if msg == "" {
			msg = err.Error()
		}
		return out.Bytes(), &cmdError{msg: msg, stderr: errb.String()}
	}
	return out.Bytes(), nil
}

// cmdError keeps a failed command's whole error output.
type cmdError struct{ msg, stderr string }

func (e *cmdError) Error() string { return e.msg }

// firstLines trims s to its first n non empty lines, joined by spaces.
func firstLines(s string, n int) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			keep = append(keep, l)
			if len(keep) == n {
				break
			}
		}
	}
	return strings.Join(keep, " ")
}

// meta stores v as the record's JSON metadata.
func meta(v any) map[string]string {
	b, _ := json.Marshal(v)
	return map[string]string{"state": string(b)}
}

func unmeta(m map[string]string, v any) error {
	return json.Unmarshal([]byte(m["state"]), v)
}

var checkSpace = snap.CheckSpace

// writeFile writes data under dir after checking the free space floor.
func writeFile(dir, name string, data []byte) error {
	if err := checkSpace(dir, int64(len(data))); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), data, 0o600)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func listSome(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" and %d more", len(items)-n)
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
