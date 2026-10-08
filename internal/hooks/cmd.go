package hooks

import (
	"path/filepath"
	"strings"

	"github.com/sohan-bhat/oopsy/internal/snap"
)

// cmd.exe has no event before a command runs, so the hook comes in through
// the AutoRun registry value, which cmd runs whenever it starts:
//
//   - doskey macros for destructive builtins and common tools. A macro only
//     expands when its name starts a typed line, and it hands the whole line
//     to a small batch file that runs the hook and then the real command.
//   - with Clink, which injects a line editor into cmd.exe, a Lua script sees
//     every line after Enter, chains and pipes included, and runs the hook.
//
// AutoRun also runs for every cmd /c that scripts and programs start, so the
// AutoRun step is a batch file that returns at once unless cmd is interactive.

// cmdAutoRunKey is the registry key holding AutoRun, under HKCU.
const cmdAutoRunKey = `Software\Microsoft\Command Processor`

// cmdBuiltins are cmd builtins that delete, move or overwrite files.
var cmdBuiltins = []string{"del", "erase", "rd", "rmdir", "move", "ren", "rename", "copy"}

// cmdTools are programs typed often that can change files. The batch file
// starts them with call, so .cmd and .bat shims return to it.
var cmdTools = []string{"xcopy", "robocopy", "git", "npm", "npx", "pnpm", "yarn", "node", "python", "py", "make"}

// cmdDir holds the AutoRun batch file, the hook batch files, the macro file
// and the Clink script.
func cmdDir() string { return filepath.Join(snap.Dir(), "cmd") }

func cmdAutoRunFile() string { return filepath.Join(cmdDir(), "autorun.cmd") }
func cmdMacroFile() string   { return filepath.Join(cmdDir(), "macros.txt") }
func cmdHookFile() string    { return filepath.Join(cmdDir(), "hook.cmd") }
func cmdCallFile() string    { return filepath.Join(cmdDir(), "hookcall.cmd") }
func cmdClinkDir() string    { return filepath.Join(cmdDir(), "clink") }
func cmdStateFile() string   { return filepath.Join(cmdDir(), "state.json") }

// cmdQuote quotes s for a cmd command line when it needs it.
func cmdQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t&|<>()^,;=%!\"") {
		return s
	}
	return `"` + s + `"`
}

// cmdAutoRunEntry is what oops adds to AutoRun. The guard keeps cmd silent
// if the files are deleted without oops uninstall.
func cmdAutoRunEntry(file string) string {
	return `(if exist "` + file + `" call "` + file + `")`
}

// chainAutoRun puts ours in front of an existing AutoRun value.
func chainAutoRun(old, ours string) string {
	if strings.TrimSpace(old) == "" {
		return ours
	}
	return ours + " & " + old
}

// unchainAutoRun takes ours out of an AutoRun value wherever it ended up,
// leaving everything else as it was. It reports whether ours was found.
func unchainAutoRun(cur, ours string) (string, bool) {
	for _, form := range []string{ours + " & ", " & " + ours, ours} {
		if i := strings.Index(cur, form); i >= 0 {
			return cur[:i] + cur[i+len(form):], true
		}
	}
	return cur, false
}

// cmdAutoRunBatch is run by AutoRun. It returns at once for cmd /c and
// cmd /r, so scripts and programs never see it, and loads the macros for
// interactive windows. Delayed expansion reads CMDCMDLINE safely whatever
// quotes or ampersands it holds.
func cmdAutoRunBatch(macros string) string {
	return crlf(`@setlocal EnableExtensions EnableDelayedExpansion
@if /i not "!CMDCMDLINE:/c=!"=="!CMDCMDLINE!" (endlocal & exit /b 0)
@if /i not "!CMDCMDLINE:/r=!"=="!CMDCMDLINE!" (endlocal & exit /b 0)
@endlocal
@doskey /macrofile="` + macros + `" >nul 2>&1
@if not defined OOPSY_CLINK set "OOPSY_SHELL_HOOK=cmd"
`)
}

// cmdHookBatch runs the hook for the line it was given, then the line, then
// the after step in the background. With Clink loaded the Lua script has
// already run the hook, so this only runs the line. Run from a batch file,
// copy, move and xcopy would overwrite without asking, so COPYCMD keeps the
// prompt they show when typed. With call set, the line is started with call
// so .cmd shims such as npm come back to run the after step.
func cmdHookBatch(bin string, call bool) string {
	b := cmdQuote(bin)
	run := "@%*"
	if call {
		run = "@call %*"
	}
	return crlf(`@setlocal
@set "OOPSY_LINE=%*"
@set "OOPSY_CWD=%CD%"
@set "OOPSY_SESSION=cmd%RANDOM%%RANDOM%"
@if not defined OOPSY_CLINK ` + b + ` hook cmd
@set "OOPSY_LINE="
@set "OOPSY_CWD="
@if not defined COPYCMD set "COPYCMD=/-Y"
` + run + `
@set "OOPSY_RC=%ERRORLEVEL%"
@set "COPYCMD="
@if not defined OOPSY_CLINK (set "OOPSY_POST_QUIET=1" & start "" /b ` + b + ` hook cmd-post >nul 2>&1)
@endlocal & exit /b %OOPSY_RC%
`)
}

// cmdMacros is the doskey macro file. $* is the rest of the typed line.
func cmdMacros(hook, call string) string {
	var sb strings.Builder
	for _, n := range cmdBuiltins {
		sb.WriteString(n + `="` + doskeyEscape(hook) + `" ` + n + " $*\r\n")
	}
	for _, n := range cmdTools {
		sb.WriteString(n + `="` + doskeyEscape(call) + `" ` + n + " $*\r\n")
	}
	return sb.String()
}

// doskeyEscape doubles $, which starts a doskey macro code.
func doskeyEscape(s string) string { return strings.ReplaceAll(s, "$", "$$") }

func crlf(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
