package hooks

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/sohan-bhat/oopsy/internal/classify"
	"github.com/sohan-bhat/oopsy/internal/remote"
	"github.com/sohan-bhat/oopsy/internal/snap"
	"github.com/sohan-bhat/oopsy/internal/ui"
)

// agentInput covers the stdin payloads of Claude Code, Codex, Gemini CLI and Cursor.
type agentInput struct {
	Cwd            string `json:"cwd"`
	Command        string `json:"command"` // Cursor
	SessionID      string `json:"session_id"`
	ConversationID string `json:"conversation_id"` // Cursor
	ToolName       string `json:"tool_name"`
	ToolInput      struct {
		Command string `json:"command"`
	} `json:"tool_input"`
	WorkspaceRoots []string `json:"workspace_roots"`
}

// shellSources are hooks that run in a shell rather than an agent.
var shellSources = map[string]bool{"zsh": true, "bash": true, "fish": true, "shell": true, "powershell": true, "cmd": true}

// Run handles one hook call. It never fails the command it guards: every
// error is reported and swallowed, and the exit code is always zero.
func Run(source string, args []string, stdin io.Reader, stdout, stderr io.Writer) (code int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "oops: hook error: %v\n", r)
		}
		agentReply(source, "", stdout)
	}()
	base, post := strings.CutSuffix(source, "-post")
	var cmd, cwd, tool, session string
	if shellSources[base] {
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
		session = os.Getenv("OOPSY_SESSION")
	} else {
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
		session = in.SessionID
		if session == "" {
			session = in.ConversationID
		}
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
	if post {
		afterCommand(base, session, stdout, stderr)
		return 0
	}
	if cmd == "" || readOnlyFor(base, tool, cmd) {
		return 0
	}
	res := classifyFor(base, tool, cmd, cwd)
	actions := append(res.Actions, lazyActions(cmd, cwd)...)
	caps := remote.Plan(cmd, cwd, os.Environ())
	var s *snap.Snapshot
	if len(actions) > 0 || len(caps) > 0 {
		var err error
		s, err = snap.Create(cmd, cwd, base, actions, caps...)
		if err != nil {
			fmt.Fprintf(stderr, "%s could not save a snapshot: %v\n", ui.ErrWarn("oops:"), err)
		}
	}
	beforeCommand(base, cmd, cwd, session, s, stderr)
	if s == nil {
		return 0
	}
	msg := Summary(s)
	cfg := snap.LoadConfig()
	switch base {
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
	case "gemini", "gemini-post":
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
	for _, r := range s.Records {
		parts = append(parts, r.Label)
	}
	what := strings.Join(parts, " and ")
	if what == "" {
		what = "nothing restorable"
	}
	return fmt.Sprintf("oops: saved snapshot %d (%s). Run oops to undo.", s.ID, what)
}
