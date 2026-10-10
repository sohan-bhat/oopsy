package classify

import (
	"path/filepath"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Commands that only read files and never change them. A line made only of
// these (with no redirect into a file) needs no snapshot, so hooks let it run
// at no cost. Anything not provably read only gets the generic project
// snapshot, so this list only has to be safe, not complete.
var readOnlyCmds = map[string]bool{
	"ls": true, "ll": true, "la": true, "l": true, "dir": true, "cat": true, "less": true, "more": true,
	"head": true, "tail": true, "grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true, "ack": true,
	"tree": true, "pwd": true, "cd": true, "pushd": true, "popd": true, "echo": true, "printf": true,
	"which": true, "type": true, "whereis": true, "whoami": true, "id": true, "date": true, "cal": true,
	"uptime": true, "printenv": true, "history": true, "clear": true, "exit": true, "true": true, "false": true,
	"man": true, "help": true, "file": true, "stat": true, "wc": true, "du": true, "df": true, "cmp": true,
	"bat": true, "ps": true, "top": true, "htop": true, "uname": true, "hostname": true, "jobs": true,
	"fg": true, "bg": true, "realpath": true, "basename": true, "dirname": true, "tldr": true, "z": true,
	"oops": true, "oops-db": true, "diff": true, "md5sum": true, "sha1sum": true, "sha256sum": true,
	"shasum": true, "md5": true, "xxd": true, "hexdump": true, "od": true, "strings": true, "nproc": true,
	"free": true, "lsof": true, "env": true, "alias": true, "unalias": true, "source": false,
}

// gitReadOnly lists git subcommands that never touch the work tree.
var gitReadOnly = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true, "blame": true, "rev-parse": true, "describe": true,
	"shortlog": true, "ls-files": true, "ls-tree": true, "grep": true, "cat-file": true, "rev-list": true,
	"whatchanged": true, "help": true, "version": true, "count-objects": true, "for-each-ref": true,
	"show-ref": true, "name-rev": true, "merge-base": true, "var": true, "check-ignore": true,
}

// ReadOnly reports whether every command in the POSIX shell line provably
// only reads. Unparseable lines are not read only.
func ReadOnly(cmd string) bool {
	if strings.TrimSpace(cmd) == "" {
		return true
	}
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(cmd), "")
	if err != nil {
		return false
	}
	ok := true
	syntax.Walk(f, func(n syntax.Node) bool {
		if !ok {
			return false
		}
		switch x := n.(type) {
		case nil, *syntax.File, *syntax.Stmt, *syntax.Word, *syntax.Lit, *syntax.SglQuoted, *syntax.DblQuoted,
			*syntax.ParamExp, *syntax.CmdSubst, *syntax.Assign:
		case *syntax.BinaryCmd:
			switch x.Op {
			case syntax.AndStmt, syntax.OrStmt, syntax.Pipe, syntax.PipeAll:
			default:
				ok = false
			}
		case *syntax.Redirect:
			ok = harmlessRedirect(x)
			return false
		case *syntax.CallExpr:
			if len(x.Args) > 0 && !readOnlyCall(x.Args) {
				ok = false
			}
		default:
			ok = false
		}
		return ok
	})
	return ok
}

func harmlessRedirect(r *syntax.Redirect) bool {
	switch r.Op {
	case syntax.RdrIn, syntax.DplIn, syntax.Hdoc, syntax.DashHdoc, syntax.WordHdoc:
		return true
	case syntax.DplOut:
		return true // 2>&1
	case syntax.RdrOut, syntax.AppOut, syntax.RdrAll, syntax.AppAll, syntax.ClbOut:
		w := r.Word.Lit()
		return w == "/dev/null" || w == "/dev/stderr" || w == "/dev/stdout" || strings.EqualFold(w, "nul")
	}
	return false
}

func readOnlyCall(args []*syntax.Word) bool {
	var argv []string
	for _, w := range args {
		argv = append(argv, wordText(w)) // "" for anything not a plain word
	}
	name := progName(argv[0])
	switch name {
	case "":
		return false
	case "git":
		return readOnlyGit(argv[1:])
	case "find":
		for _, a := range argv[1:] {
			switch a {
			case "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fprintf", "-fls", "":
				return false
			}
		}
		return true
	case "fd":
		for _, a := range argv[1:] {
			if a == "-x" || a == "-X" || strings.HasPrefix(a, "--exec") || a == "" {
				return false
			}
		}
		return true
	case "sort", "diff":
		for _, a := range argv[1:] {
			if a == "-o" || strings.HasPrefix(a, "--output") || (name == "sort" && strings.HasPrefix(a, "-o")) {
				return false
			}
		}
		return true
	case "env", "printenv":
		return len(argv) == 1 || (name == "printenv")
	case "oops", "oops-db":
		return true
	}
	return readOnlyCmds[name]
}

func readOnlyGit(args []string) bool {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "-C", "-c", "--git-dir", "--work-tree":
			if len(args) < 2 {
				return false
			}
			args = args[2:]
		default:
			args = args[1:]
		}
	}
	if len(args) == 0 {
		return true
	}
	sub, rest := args[0], args[1:]
	for _, a := range rest {
		if strings.HasPrefix(a, "--output") || a == "" {
			return false
		}
	}
	switch sub {
	case "branch", "tag":
		for _, a := range rest {
			switch a {
			case "-a", "-r", "-v", "-vv", "--all", "--list", "-l", "--remotes", "--show-current", "--merged", "--no-merged", "--contains":
			default:
				return false
			}
		}
		return true
	case "remote":
		return len(rest) == 0 || (len(rest) == 1 && rest[0] == "-v")
	case "stash":
		return len(rest) > 0 && (rest[0] == "list" || rest[0] == "show")
	case "config":
		return len(rest) > 0 && (rest[0] == "--get" || rest[0] == "--list" || rest[0] == "-l" || rest[0] == "--get-all")
	case "reflog":
		return len(rest) == 0 || rest[0] == "show"
	}
	return gitReadOnly[sub]
}

// ReadOnlyPattern is the POSIX extended regex the shell hooks use to skip
// provably read only lines without starting a process. It is deliberately
// narrower than ReadOnly: plain commands and pipes, no redirects into files,
// no substitutions. Anything it does not match goes to oops hook.
var ReadOnlyPattern = func() string {
	var simple []string
	for c, ok := range readOnlyCmds {
		if ok && c != "env" && c != "printenv" && c != "diff" && c != "oops" && c != "oops-db" {
			simple = append(simple, regexp.QuoteMeta(c))
		}
	}
	sortStrings(simple)
	arg := `([^|&;<>()$` + "`" + `\\]|\$[A-Za-z_{])*`
	gitArg := `([[:space:]]+[A-Za-z0-9_./:@^~%,-]+)*`
	git := `git([[:space:]]+-C[[:space:]]+[A-Za-z0-9_./~-]+)?[[:space:]]+((status|log|diff|show|blame|rev-parse|describe|shortlog|ls-files|ls-tree|grep|cat-file)` +
		gitArg + `|branch([[:space:]]+-(a|r|v|vv))*|remote([[:space:]]+-v)?|stash[[:space:]]+list)`
	seg := `[[:space:]]*((` + strings.Join(simple, "|") + `)([[:space:]]` + arg + `)?|oops([[:space:]][^|&;<>` + "`" + `]*)?|` + git + `)[[:space:]]*`
	return `^` + seg + `((\||&&|\|\|)` + seg + `)*$`
}()

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

var readOnlyRe = regexp.MustCompile(ReadOnlyPattern)

// QuickReadOnly is the Go twin of the shell prefilter, for tests.
func QuickReadOnly(cmd string) bool { return readOnlyRe.MatchString(cmd) }

// Words returns the literal words of every command in the line, for finding
// paths a generic command names.
func Words(cmd, cwd string) []string {
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(cmd), "")
	if err != nil {
		return strings.Fields(cmd)
	}
	var out []string
	syntax.Walk(f, func(n syntax.Node) bool {
		if w, ok := n.(*syntax.Word); ok {
			if lit := wordText(w); lit != "" {
				out = append(out, lit)
			}
			return false
		}
		return true
	})
	return out
}

func wordText(w *syntax.Word) string {
	var b strings.Builder
	for _, p := range w.Parts {
		switch x := p.(type) {
		case *syntax.Lit:
			b.WriteString(x.Value)
		case *syntax.SglQuoted:
			b.WriteString(x.Value)
		case *syntax.DblQuoted:
			for _, q := range x.Parts {
				l, ok := q.(*syntax.Lit)
				if !ok {
					return ""
				}
				b.WriteString(l.Value)
			}
		default:
			return ""
		}
	}
	return b.String()
}

// AbsWord turns a word into an absolute path relative to cwd, expanding ~.
func AbsWord(w, cwd, home string) string {
	switch {
	case w == "~":
		return home
	case strings.HasPrefix(w, "~/"):
		return filepath.Join(home, w[2:])
	case filepath.IsAbs(w):
		return filepath.Clean(w)
	}
	return filepath.Join(cwd, w)
}

// ReadOnlyPatternWindows is the case insensitive prefilter PowerShell and
// cmd.exe hooks use to skip read only lines without starting a process.
const ReadOnlyPatternWindows = `^\s*((get-childitem|gci|ls|dir|get-content|gc|cat|type|select-string|sls|cd|chdir|set-location|sl|pwd|get-location|gl|echo|write-output|write-host|cls|clear|clear-host|where|where\.exe|get-command|gcm|get-process|gps|get-help|help|man|measure-object|measure|get-item|gi|get-date|whoami|hostname|exit|findstr|more|tree|get-filehash|test-path|resolve-path|oops|oops-db)(\s[^|&;<>()$` + "`" + `{}@]*)?|git(\s+-C\s+[\w.:\\/~-]+)?\s+((status|log|diff|show|blame|rev-parse|describe|shortlog|ls-files|ls-tree|grep|cat-file)(\s+[\w./:@^~%,-]+)*|branch(\s+-(a|r|v|vv))*|remote(\s+-v)?|stash\s+list))\s*(\|\s*((select-string|sls|select-object|select|sort-object|sort|measure-object|measure|format-table|ft|format-list|fl|more|out-host|findstr|where-object|where)(\s[^|&;<>()$` + "`" + `{}@]*)?)\s*)*$`

var readOnlyWindowsRe = regexp.MustCompile(`(?i)` + ReadOnlyPatternWindows)

// QuickReadOnlyWindows is the Go twin of the PowerShell and cmd prefilter.
func QuickReadOnlyWindows(cmd string) bool { return readOnlyWindowsRe.MatchString(cmd) }
