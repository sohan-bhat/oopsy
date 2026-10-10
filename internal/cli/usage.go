// Package cli holds what both oops and oops-db need to talk to people:
// plain usage errors with the right usage and an example, suggestions for
// mistyped commands and options, and consistent exit codes.
package cli

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Exit codes, documented in the README and in oops --help.
const (
	ExitOK       = 0 // done, or nothing to do
	ExitFailed   = 1 // something went wrong; the message says what and what to do
	ExitUsage    = 2 // the command line was wrong; the message shows the right one
	ExitNotFound = 3 // the snapshot or change asked for does not exist
	ExitPartial  = 4 // a restore brought back some paths but not all
)

// ExitCodesHelp documents the exit codes for --help.
const ExitCodesHelp = `Exit codes:
  0  done, or nothing to do
  1  something went wrong (the message says what and what to do next)
  2  the command line was wrong (the message shows the right usage)
  3  the snapshot or database change asked for does not exist
  4  a restore brought back some paths but not all`

// exitError carries an exit code with a message.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// UsageErr is a command line mistake.
func UsageErr(format string, a ...any) error {
	return &exitError{code: ExitUsage, msg: fmt.Sprintf(format, a...)}
}

// NotFound is a missing snapshot or change.
func NotFound(format string, a ...any) error {
	return &exitError{code: ExitNotFound, msg: fmt.Sprintf(format, a...)}
}

// ExitCode picks the exit code for an error.
func ExitCode(err error) int {
	var e *exitError
	if errors.As(err, &e) {
		return e.code
	}
	return ExitFailed
}

// usageHelp is what each command needs, with an example, for messages
// about missing or extra arguments.
var usageHelp = map[string]struct{ need, example string }{
	"db undo":    {"at most one change id (oops db list shows them)", "oops db undo 42"},
	"explain":    {"a command to check", "oops explain rm -rf build"},
	"undo":       {"the id of a snapshot (oops list shows them)", "oops undo 12"},
	"show":       {"the id of a snapshot (oops list shows them)", "oops show 12"},
	"diff":       {"the id of a snapshot (oops list shows them)", "oops diff 12"},
	"restore":    {"the id of a snapshot and a folder to copy it into", "oops restore 12 --to ~/recovered"},
	"config":     {"a setting and a value, or nothing to show them all", "oops config keep_days 14"},
	"shell-init": {"the shell to print the hook for: zsh, bash, fish or powershell", "oops shell-init zsh"},
	"hook":       {"where the call comes from", "oops hook zsh -- rm -rf build"},
	"build":      {"at most one project folder", "oops mirror build ~/code/app"},
}

// friendlyArgs wraps every command's argument check so mistakes get a plain
// sentence with the right usage instead of cobra's default text.
// FriendlyArgs rewrites argument errors for c and its subcommands.
func FriendlyArgs(c *cobra.Command) {
	for _, sub := range c.Commands() {
		FriendlyArgs(sub)
	}
	orig := c.Args
	if orig == nil {
		if !c.HasSubCommands() {
			return
		}
		// A group like oops db: unknown words get a suggestion too.
		orig = cobra.NoArgs
		if c.Run == nil && c.RunE == nil {
			c.RunE = func(cmd *cobra.Command, args []string) error { return cmd.Help() }
		}
	}
	c.Args = func(cmd *cobra.Command, args []string) error {
		err := orig(cmd, args)
		if err == nil {
			return nil
		}
		return argsError(cmd, args)
	}
}

// displayPath is how a person types cmd. The oops-db binary's root is
// named "oops db", since it runs as oops db.
func displayPath(cmd *cobra.Command) string {
	path := cmd.CommandPath()
	root := cmd.Root()
	if use := strings.Fields(root.Use); len(use) > 1 && !strings.HasPrefix(use[1], "[") && !strings.HasPrefix(use[1], "<") {
		path = strings.Join(use[:2], " ") + strings.TrimPrefix(path, root.Name())
	}
	return path
}

func argsError(cmd *cobra.Command, args []string) error {
	path := displayPath(cmd)
	if !cmd.HasParent() || (cmd.HasSubCommands() && len(args) > 0) {
		// An unknown word after oops (or oops db, oops watch).
		word := args[0]
		msg := fmt.Sprintf("%s %s is not a command.", path, word)
		if s := closestCommand(cmd, word); s != "" {
			msg += fmt.Sprintf(" Did you mean %s %s?", path, s)
		}
		return UsageErr("%s Run %s --help to see them all.", msg, path)
	}
	h, ok := usageHelp[cmd.Name()]
	if hh, ok2 := usageHelp[strings.TrimPrefix(path, "oops ")]; ok2 {
		h, ok = hh, true
	}
	if !ok {
		if len(args) > 0 {
			return UsageErr("%s takes no arguments, but got %q. Run %s --help for how to use it.", path, strings.Join(args, " "), path)
		}
		return UsageErr("%s is missing something. Run %s --help for how to use it.", path, path)
	}
	if len(args) == 0 {
		return UsageErr("%s needs %s. Try: %s", path, h.need, h.example)
	}
	return UsageErr("%s needs %s, but got %q. Try: %s", path, h.need, strings.Join(args, " "), h.example)
}

// flagError rewrites pflag's messages and suggests the closest flag.
// FlagError rewrites option errors; use it with SetFlagErrorFunc.
func FlagError(c *cobra.Command, err error) error {
	msg := err.Error()
	name := ""
	switch {
	case strings.HasPrefix(msg, "unknown flag: "):
		name = strings.TrimPrefix(msg, "unknown flag: ")
	case strings.HasPrefix(msg, "unknown shorthand flag: "):
		// unknown shorthand flag: 'x' in -x
		if i := strings.Index(msg, "' in "); i > 0 {
			name = msg[i+5:]
		}
	}
	if name != "" {
		out := fmt.Sprintf("%s does not have a %s option.", displayPath(c), name)
		if s := closestFlag(c, strings.TrimLeft(name, "-")); s != "" {
			out += fmt.Sprintf(" Did you mean %s?", s)
		}
		return UsageErr("%s Run %s --help to see its options.", out, displayPath(c))
	}
	if strings.Contains(msg, "flag needs an argument") {
		return UsageErr("%s: %s. Run %s --help for an example.", displayPath(c), strings.ReplaceAll(msg, "flag needs an argument", "this option needs a value"), displayPath(c))
	}
	if strings.HasPrefix(msg, "invalid argument") {
		return UsageErr("%s: %s. Run %s --help for an example.", displayPath(c), msg, displayPath(c))
	}
	return UsageErr("%s. Run %s --help for how to use it.", msg, displayPath(c))
}

func closestCommand(c *cobra.Command, word string) string {
	best, bestD := "", 3
	for _, sub := range c.Commands() {
		if sub.Hidden || !sub.IsAvailableCommand() {
			continue
		}
		for _, n := range append([]string{sub.Name()}, sub.Aliases...) {
			d := distance(word, n)
			if strings.HasPrefix(n, word) && len(word) >= 2 {
				d = 1
			}
			if d < bestD {
				best, bestD = sub.Name(), d
			}
		}
	}
	return best
}

func closestFlag(c *cobra.Command, name string) string {
	type cand struct {
		s string
		d int
	}
	var all []cand
	c.Flags().VisitAll(func(f *pflag.Flag) {
		all = append(all, cand{"--" + f.Name, distance(name, f.Name)})
		if f.Shorthand != "" {
			all = append(all, cand{"-" + f.Shorthand, distance(name, f.Shorthand)})
		}
	})
	sort.Slice(all, func(i, j int) bool { return all[i].d < all[j].d })
	if len(all) > 0 && all[0].d <= 2 {
		return all[0].s
	}
	return ""
}

func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
