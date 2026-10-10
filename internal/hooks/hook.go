package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/sohan-bhat/oopsy/internal/classify"
	"github.com/sohan-bhat/oopsy/internal/snap"
	"github.com/sohan-bhat/oopsy/internal/ui"
)

// agentInput covers the stdin payloads of Claude Code, Codex, Gemini CLI and Cursor.
type agentInput struct {
	Cwd       string `json:"cwd"`
	Command   string `json:"command"` // Cursor
	ToolName  string `json:"tool_name"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
	WorkspaceRoots []string `json:"workspace_roots"`
}

// Run handles one hook call. It never fails the command it guards: every
// error is reported and swallowed, and the exit code is always zero.
func Run(source string, args []string, stdin io.Reader, stdout, stderr io.Writer) (code int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "oops: hook error: %v\n", r)
		}
		agentReply(source, "", stdout)
	}()
	var cmd, cwd, tool string
	switch source {
	case "zsh", "bash", "fish", "shell", "powershell", "cmd":
		cmd = strings.Join(args, " ")
		if cmd == "" {
			// The PowerShell hook passes the line in the environment, since
			// Windows PowerShell mangles quotes in native arguments.
			cmd = os.Getenv("OOPSY_LINE")
		}
		cwd = os.Getenv("OOPSY_CWD")
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
	default:
		b, err := io.ReadAll(io.LimitReader(stdin, 4<<20))
		if err != nil {
			return 0
		}
		var in agentInput
		if err := json.Unmarshal(b, &in); err != nil {
			return 0
		}
		cmd = in.ToolInput.Command
		if cmd == "" {
			cmd = in.Command
		}
		tool = in.ToolName
		cwd = in.Cwd
		if cwd == "" && len(in.WorkspaceRoots) > 0 {
			cwd = in.WorkspaceRoots[0]
		}
		if cwd == "" {
			cwd = os.Getenv("GEMINI_CWD")
		}
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
	}
	if cmd == "" {
		return 0
	}
	res := classifyFor(source, tool, cmd, cwd)
	if !res.Destructive() {
		return 0
	}
	s, err := snap.Create(cmd, cwd, source, res.Actions)
	if err != nil {
		fmt.Fprintf(stderr, "%s could not save a snapshot: %s\n", ui.ErrWarn("oops:"), ui.Explain(err))
		return 0
	}
	if s == nil {
		return 0
	}
	msg := Summary(s)
	cfg := snap.LoadConfig()
	switch source {
	case "zsh", "bash", "fish", "shell", "powershell", "cmd":
		if !cfg.Quiet || len(s.Skipped) > 0 {
			fmt.Fprintln(stderr, ui.ErrDim(msg))
		}
		for _, sk := range s.Skipped {
			fmt.Fprintf(stderr, "%s not saved: %s (%s)\n", ui.ErrWarn("oops:"), ui.HomeRel(sk.Path), sk.Reason)
		}
	case "claude":
		reply, _ := json.Marshal(map[string]any{"systemMessage": msg})
		fmt.Fprintln(stdout, string(reply))
		replied = true
	default:
		fmt.Fprintln(stderr, msg)
	}
	return 0
}

var replied bool

// classifyFor picks the parser for where the command came from. Agents on
// Windows may run commands through Git Bash, PowerShell or cmd.exe, and the
// payload does not always say which, so all three are tried and merged:
// an extra snapshot is cheap, a missed one is not.
func classifyFor(source, tool, cmd, cwd string) classify.Result {
	env := os.Environ()
	switch source {
	case "zsh", "bash", "fish", "shell":
		return classify.Classify(cmd, cwd, env)
	case "powershell":
		return classify.ClassifyPowerShell(cmd, cwd, env)
	case "cmd":
		return classify.ClassifyCmd(cmd, cwd, env)
	}
	if strings.EqualFold(tool, "PowerShell") {
		return classify.ClassifyPowerShell(cmd, cwd, env)
	}
	if runtime.GOOS != "windows" {
		return classify.Classify(cmd, cwd, env)
	}
	return classify.Merge(classify.Classify(cmd, cwd, env), classify.ClassifyPowerShell(cmd, cwd, env), classify.ClassifyCmd(cmd, cwd, env))
}

// agentReply prints the JSON some agents require on stdout.
func agentReply(source, _ string, stdout io.Writer) {
	if replied {
		return
	}
	switch source {
	case "cursor":
		fmt.Fprintln(stdout, `{"continue":true,"permission":"allow"}`)
	case "gemini":
		fmt.Fprintln(stdout, `{}`)
	}
	replied = true
}

// Summary is the one line shown after a snapshot is taken.
func Summary(s *snap.Snapshot) string {
	var parts []string
	if n := len(s.Entries); n > 0 {
		parts = append(parts, ui.Plural(s.Files, "item")+", "+ui.Bytes(s.Size))
	}
	if n := len(s.Git); n > 0 {
		parts = append(parts, ui.Plural(n, "git ref"))
	}
	what := strings.Join(parts, " and ")
	if what == "" {
		what = "nothing restorable"
	}
	return fmt.Sprintf("oops: saved snapshot %d (%s). Run oops to undo.", s.ID, what)
}
