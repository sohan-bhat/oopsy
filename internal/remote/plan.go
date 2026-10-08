// Package remote saves state that lives outside the file system right
// before a command changes it: Kubernetes objects before kubectl delete,
// apply or replace, Terraform state before destroy or apply, Docker volume
// contents before docker volume rm, and Helm releases before helm uninstall.
//
// Plan only reads the command line. The captures it returns do the actual
// work when snap.Create runs them, and each one registers how it is undone.
package remote

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"

	"github.com/sohan-bhat/oopsy/internal/snap"
)

// Target is one thing Plan would save, with a description for oops explain.
type Target struct {
	Tool     string // kubectl, terraform, docker or helm
	Describe string // what would be saved, in one line
	Capture  snap.Capture
}

// call is one simple command found on the line.
type call struct {
	argv       []string
	assigns    []string // VAR=value given before the command
	env        []string // the environment the command would see
	cwd        string
	unresolved bool // some word could not be expanded before running
}

// quick skips lines that cannot involve any tool this package knows.
var quick = regexp.MustCompile(`(?i)(kubectl|\boc\b|terraform|tofu|docker|podman|helm)`)

// Plan returns the captures to run before cmd, an empty list when the
// command changes nothing this package can save.
func Plan(cmd, cwd string, env []string) []snap.Capture {
	var out []snap.Capture
	for _, t := range Targets(cmd, cwd, env) {
		out = append(out, t.Capture)
	}
	return out
}

// Targets is Plan with descriptions, for oops explain.
func Targets(cmd, cwd string, env []string) []Target {
	if !quick.MatchString(cmd) {
		return nil
	}
	var out []Target
	for _, c := range calls(cmd, cwd, env) {
		c.argv = unwrap(c.argv)
		if len(c.argv) == 0 {
			continue
		}
		var ts []Target
		switch progName(c.argv[0]) {
		case "kubectl", "oc":
			ts = kubeTargets(c)
		case "terraform", "tofu":
			ts = tfTargets(c)
		default:
			continue
		}
		if c.unresolved && len(ts) > 0 {
			// Guessing would save the wrong objects, so say so instead.
			name := progName(c.argv[0])
			ts = []Target{{Tool: name, Describe: "nothing, some arguments are only known when the command runs", Capture: func(string) (*snap.Record, error) {
				return nil, errors.New(name + ": not saved because some arguments could only be worked out by running the command")
			}}}
		}
		out = append(out, ts...)
	}
	return out
}

// progName turns /usr/local/bin/kubectl or C:\bin\kubectl.exe into kubectl.
func progName(arg string) string {
	if i := strings.LastIndexAny(arg, `/\`); i >= 0 {
		arg = arg[i+1:]
	}
	arg = strings.ToLower(arg)
	return strings.TrimSuffix(arg, ".exe")
}

// unwrap drops sudo, env, time and similar prefixes.
func unwrap(argv []string) []string {
	for len(argv) > 0 {
		switch progName(argv[0]) {
		case "sudo", "doas", "command", "exec", "nohup", "time", "nice", "builtin", "noglob", "caffeinate":
			argv = argv[1:]
			for len(argv) > 0 && strings.HasPrefix(argv[0], "-") {
				argv = argv[1:]
			}
		case "env":
			argv = argv[1:]
			for len(argv) > 0 && (strings.HasPrefix(argv[0], "-") || strings.Contains(argv[0], "=")) {
				argv = argv[1:]
			}
		default:
			return argv
		}
	}
	return argv
}

// calls lists the simple commands on a line, expanding words the way the
// shell would. Lines the POSIX parser rejects (PowerShell, cmd.exe, fish)
// fall back to a plain split on separators and whitespace.
func calls(cmd, cwd string, env []string) []call {
	if windowsPath.MatchString(cmd) {
		return roughCalls(cmd, cwd, env) // backslashes are separators, not escapes
	}
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(cmd), "")
	if err != nil {
		return roughCalls(cmd, cwd, env)
	}
	vars := map[string]string{}
	var out []call
	syntax.Walk(f, func(n syntax.Node) bool {
		ce, ok := n.(*syntax.CallExpr)
		if !ok {
			return true
		}
		cfg := expandConfig(cwd, env, vars)
		var assigns []string
		for _, a := range ce.Assigns {
			v := ""
			if a.Value != nil {
				v, err = expand.Literal(cfg, a.Value)
				if err != nil || strings.Contains(v, unknown) {
					continue
				}
			}
			assigns = append(assigns, a.Name.Value+"="+v)
		}
		if len(ce.Args) == 0 {
			for _, kv := range assigns {
				k, v, _ := strings.Cut(kv, "=")
				vars[k] = v
			}
			return true
		}
		c := call{assigns: assigns, cwd: cwd, env: append(append([]string(nil), env...), assigns...)}
		for _, w := range ce.Args {
			fs, err := expand.Fields(cfg, w)
			if err != nil || containsUnknown(fs) {
				// Keep a stand in, so the command still parses as it will run.
				c.unresolved = true
				c.argv = append(c.argv, "unresolved")
				continue
			}
			c.argv = append(c.argv, fs...)
		}
		if len(c.argv) > 0 && (c.argv[0] == "cd" || c.argv[0] == "pushd") && len(c.argv) == 2 && !c.unresolved {
			cwd = absIn(cwd, c.argv[1])
			return true
		}
		out = append(out, c)
		return true
	})
	return out
}

var windowsPath = regexp.MustCompile(`(?i)(^|[\s"'])[a-z]:\\|\\[^\\\s]+\.exe\b`)

var roughSeps = regexp.MustCompile(`\s*(?:;|&&|\|\||\||\n)\s*`)

func roughCalls(cmd, cwd string, env []string) []call {
	var out []call
	for _, part := range roughSeps.Split(cmd, -1) {
		argv := splitWords(strings.TrimSpace(part))
		if len(argv) > 0 && strings.HasPrefix(argv[0], "&") {
			argv = argv[1:] // PowerShell's call operator
		}
		if len(argv) == 0 {
			continue
		}
		c := call{argv: argv, cwd: cwd, env: env}
		for _, a := range argv {
			if strings.Contains(a, "$") || strings.Contains(a, "%") {
				c.unresolved = true
			}
		}
		out = append(out, c)
	}
	return out
}

// splitWords splits on whitespace, honoring single and double quotes.
func splitWords(s string) []string {
	var out []string
	var cur strings.Builder
	in := false
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t' || r == '\r' || r == '\n':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

const unknown = "\x00unknown"

type unknownEnv struct{ expand.Environ }

func (e unknownEnv) Get(name string) expand.Variable {
	v := e.Environ.Get(name)
	if v.IsSet() || name == "IFS" || strings.HasPrefix(name, "HOME ") {
		return v
	}
	return expand.Variable{Set: true, Kind: expand.String, Str: unknown}
}

func containsUnknown(list []string) bool {
	for _, f := range list {
		if strings.Contains(f, unknown) {
			return true
		}
	}
	return false
}

func expandConfig(cwd string, env []string, vars map[string]string) *expand.Config {
	all := append([]string(nil), env...)
	for k, v := range vars {
		all = append(all, k+"="+v)
	}
	all = append(all, "PWD="+cwd)
	return &expand.Config{
		Env: unknownEnv{expand.ListEnviron(all...)},
		ReadDir2: func(d string) ([]fs.DirEntry, error) {
			return os.ReadDir(absIn(cwd, d))
		},
		CmdSubst:  func(io.Writer, *syntax.CmdSubst) error { return errors.New("command substitution") },
		ProcSubst: func(*syntax.ProcSubst) (string, error) { return "", errors.New("process substitution") },
	}
}

// absIn resolves p against dir.
func absIn(dir, p string) string {
	if p == "" {
		return dir
	}
	if strings.HasPrefix(p, "~/") || p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(dir, p)
}

// lookupEnv returns the last value of key in env.
func lookupEnv(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}
