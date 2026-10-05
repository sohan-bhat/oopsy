// Package ui holds the terminal styling helpers used by every command.
package ui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"
)

var (
	color = os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" && isatty.IsTerminal(os.Stdout.Fd())

	accent  = lipgloss.NewStyle().Foreground(lipgloss.Color("#E8913A")).Bold(true)
	good    = lipgloss.NewStyle().Foreground(lipgloss.Color("#4CB782"))
	warn    = lipgloss.NewStyle().Foreground(lipgloss.Color("#E5B84B"))
	bad     = lipgloss.NewStyle().Foreground(lipgloss.Color("#E5534B"))
	dim     = lipgloss.NewStyle().Foreground(lipgloss.Color("#8B8B8B"))
	bold    = lipgloss.NewStyle().Bold(true)
	errOut  = os.Stderr
	errTerm = os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" && isatty.IsTerminal(os.Stderr.Fd())
)

func style(s lipgloss.Style, on bool, text string) string {
	if !on {
		return text
	}
	return s.Render(text)
}

// Accent renders a highlighted label.
func Accent(s string) string { return style(accent, color, s) }

// Good renders a success color.
func Good(s string) string { return style(good, color, s) }

// Warn renders a warning color.
func Warn(s string) string { return style(warn, color, s) }

// Bad renders an error color.
func Bad(s string) string { return style(bad, color, s) }

// Dim renders muted text.
func Dim(s string) string { return style(dim, color, s) }

// Bold renders bold text.
func Bold(s string) string { return style(bold, color, s) }

// ErrDim renders muted text for stderr.
func ErrDim(s string) string { return style(dim, errTerm, s) }

// ErrAccent renders accent text for stderr.
func ErrAccent(s string) string { return style(accent, errTerm, s) }

// ErrWarn renders warning text for stderr.
func ErrWarn(s string) string { return style(warn, errTerm, s) }

// Errorf prints an error line to stderr.
func Errorf(format string, a ...any) {
	fmt.Fprintf(errOut, "%s %s\n", style(bad, errTerm, "oops:"), fmt.Sprintf(format, a...))
}

// Bytes formats a byte count for humans.
func Bytes(n int64) string {
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

// Plural returns "1 file" or "3 files".
func Plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// Confirm asks a yes or no question on the controlling terminal.
// It returns false when there is no terminal to ask on.
func Confirm(question string) bool {
	var in io.Reader = os.Stdin
	var out io.Writer = os.Stderr
	if !stdinIsTerminal() {
		tty, err := openTTY()
		if err != nil {
			return false
		}
		defer tty.Close()
		in = tty
		if runtime.GOOS != "windows" {
			out = tty
		}
	}
	fmt.Fprintf(out, "%s %s ", question, Dim("[y/N]"))
	line, _ := bufio.NewReader(in).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

// Interactive reports whether a person can answer a prompt.
func Interactive() bool {
	if stdinIsTerminal() {
		return true
	}
	f, err := openTTY()
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// Shorten trims a string to n runes with an ellipsis.
func Shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// stdinIsTerminal is true for a console and for Git Bash's mintty.
func stdinIsTerminal() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd())
}

// openTTY opens the controlling terminal even when stdin is redirected.
func openTTY() (*os.File, error) {
	if runtime.GOOS == "windows" {
		// CONIN$ reads the console; prompts go to stderr, which is enough.
		return os.OpenFile("CONIN$", os.O_RDWR, 0)
	}
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}

// HomeRel replaces the home directory prefix with ~.
func HomeRel(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || len(p) < len(home) {
		return p
	}
	same := p[:len(home)] == home
	if runtime.GOOS == "windows" {
		same = strings.EqualFold(p[:len(home)], home)
	}
	if same && (len(p) == len(home) || os.IsPathSeparator(p[len(home)])) {
		return "~" + p[len(home):]
	}
	return p
}
