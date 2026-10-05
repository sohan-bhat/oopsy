// Package classify decides whether a shell command line is destructive and,
// if so, which paths or git state it is about to destroy. It parses the
// command with a real shell parser and resolves variables, tildes, braces and
// globs the way the shell will, right before the command runs.
package classify

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// GitOp describes git state that a command is about to destroy.
type GitOp struct {
	Kind      string   `json:"kind"` // reset-hard, checkout-paths, clean, branch-delete, stash-drop, stash-clear, push-force
	Repo      string   `json:"repo"` // directory git runs in
	Args      []string `json:"args,omitempty"`
	Paths     []string `json:"paths,omitempty"`
	Branches  []string `json:"branches,omitempty"`
	StashRefs []string `json:"stash_refs,omitempty"`
	Remote    string   `json:"remote,omitempty"`
	Refspecs  []string `json:"refspecs,omitempty"`
}

// Action is one destructive step found in a command line.
type Action struct {
	Program string   `json:"program"`
	Paths   []string `json:"paths,omitempty"` // absolute paths about to be removed or overwritten
	Git     *GitOp   `json:"git,omitempty"`
	Broad   bool     `json:"broad,omitempty"` // targets could not be resolved, a whole directory is saved instead
	Note    string   `json:"note,omitempty"`
	// Delete means the command only unlinks Paths and never writes into
	// them, so a hard link is a safe snapshot. Overwrites leave it false.
	Delete bool `json:"delete,omitempty"`
}

// Result is the outcome of classifying a command line.
type Result struct {
	Actions []Action
}

// Destructive reports whether anything in the command needs a snapshot.
func (r Result) Destructive() bool { return len(r.Actions) > 0 }

var quick = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])(g?(rm|mv|cp|ln|sed|dd|tee|find|shred|unlink|rmdir|truncate|install)|srm|git|rsync|perl|xargs)($|[^A-Za-z0-9_-])|>`)

// QuickPattern is the POSIX extended regex the shell hooks use to skip
// obviously harmless commands without starting a process.
const QuickPattern = `(^|[^A-Za-z0-9_.-])(g?(rm|mv|cp|ln|sed|dd|tee|find|shred|unlink|rmdir|truncate|install)|srm|git|rsync|perl|xargs)($|[^A-Za-z0-9_-])|>`

// MaybeDestructive is a cheap prefilter. False means the command is
// certainly not destructive; true means it needs a full parse.
func MaybeDestructive(cmd string) bool { return quick.MatchString(cmd) }

// Classify parses cmd as it would run in cwd with the given environment.
func Classify(cmd, cwd string, env []string) Result {
	if !MaybeDestructive(cmd) {
		return Result{}
	}
	s := newState(cwd, env)
	s.line(cmd)
	return Result{Actions: s.actions}
}

type state struct {
	cwd     string
	env     []string
	vars    map[string]string
	depth   int
	actions []Action
}

func newState(cwd string, env []string) *state {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	return &state{cwd: cwd, env: env, vars: map[string]string{}}
}

func (s *state) child() *state {
	c := &state{cwd: s.cwd, env: s.env, vars: map[string]string{}, depth: s.depth}
	for k, v := range s.vars {
		c.vars[k] = v
	}
	return c
}

func (s *state) merge(c *state) { s.actions = append(s.actions, c.actions...) }

func (s *state) add(a Action) { s.actions = append(s.actions, a) }

func (s *state) line(cmd string) {
	if s.depth > 8 {
		return
	}
	s.depth++
	defer func() { s.depth-- }()
	p := syntax.NewParser(syntax.Variant(syntax.LangBash), syntax.KeepComments(false))
	f, err := p.Parse(strings.NewReader(cmd), "")
	if err != nil {
		// Unparseable (for example fish syntax). Fall back to a rough split
		// so that we still err on the side of saving too much.
		s.rough(cmd)
		return
	}
	s.stmts(f.Stmts)
}

// rough handles command lines the bash parser rejects.
func (s *state) rough(cmd string) {
	seps := regexp.MustCompile(`\s*(?:;|&&|\|\||\||\band\b|\bor\b)\s*`)
	for _, part := range seps.Split(cmd, -1) {
		part = strings.TrimSpace(strings.Trim(strings.TrimSpace(part), "()"))
		if part == "" {
			continue
		}
		p := syntax.NewParser(syntax.Variant(syntax.LangBash))
		f, err := p.Parse(strings.NewReader(part), "")
		if err == nil {
			s.stmts(f.Stmts)
			continue
		}
		fields := strings.Fields(part)
		var argv []string
		for _, w := range fields {
			argv = append(argv, strings.Trim(w, `'"`))
		}
		if len(argv) > 0 {
			s.dispatch(argv, false)
		}
	}
}

func (s *state) stmts(list []*syntax.Stmt) {
	for _, st := range list {
		s.stmt(st)
	}
}

func (s *state) stmt(st *syntax.Stmt) {
	if st == nil {
		return
	}
	for _, r := range st.Redirs {
		s.redir(r)
	}
	s.command(st.Cmd)
}

func (s *state) command(cmd syntax.Command) {
	switch c := cmd.(type) {
	case nil:
	case *syntax.CallExpr:
		s.substs(c)
		if len(c.Args) == 0 {
			for _, a := range c.Assigns {
				s.assign(a)
			}
			return
		}
		saved := map[string]string{}
		for _, a := range c.Assigns {
			if a.Name != nil {
				saved[a.Name.Value] = s.vars[a.Name.Value]
			}
			s.assign(a)
		}
		argv, ok := s.fields(c.Args)
		if len(argv) > 0 {
			s.dispatch(argv, !ok)
		}
		for k, v := range saved {
			if v == "" {
				delete(s.vars, k)
			} else {
				s.vars[k] = v
			}
		}
	case *syntax.BinaryCmd:
		switch c.Op {
		case syntax.Pipe, syntax.PipeAll:
			l, r := s.child(), s.child()
			l.stmt(c.X)
			r.stmt(c.Y)
			s.merge(l)
			s.merge(r)
		default:
			s.stmt(c.X)
			s.stmt(c.Y)
		}
	case *syntax.Subshell:
		ch := s.child()
		ch.stmts(c.Stmts)
		s.merge(ch)
	case *syntax.Block:
		s.stmts(c.Stmts)
	case *syntax.IfClause:
		for ic := c; ic != nil; ic = ic.Else {
			s.stmts(ic.Cond)
			s.stmts(ic.Then)
		}
	case *syntax.WhileClause:
		s.stmts(c.Cond)
		s.stmts(c.Do)
	case *syntax.ForClause:
		s.forClause(c)
	case *syntax.CaseClause:
		s.substs(c.Word)
		for _, item := range c.Items {
			s.stmts(item.Stmts)
		}
	case *syntax.FuncDecl:
		ch := s.child()
		ch.stmt(c.Body)
		s.merge(ch)
	case *syntax.DeclClause:
		for _, a := range c.Args {
			s.substs(a)
			s.assign(a)
		}
	case *syntax.TimeClause:
		s.stmt(c.Stmt)
	case *syntax.CoprocClause:
		s.stmt(c.Stmt)
	default:
		s.substs(c)
	}
}

func (s *state) forClause(c *syntax.ForClause) {
	wi, ok := c.Loop.(*syntax.WordIter)
	if !ok {
		s.stmts(c.Do)
		return
	}
	s.substs(wi)
	items, resolved := s.fields(wi.Items)
	if !resolved || len(items) == 0 || len(items) > 64 {
		ch := s.child()
		ch.vars[wi.Name.Value] = "\x00unknown"
		ch.stmts(c.Do)
		s.merge(ch)
		return
	}
	for _, it := range items {
		ch := s.child()
		ch.vars[wi.Name.Value] = it
		ch.stmts(c.Do)
		s.merge(ch)
	}
}

// substs classifies command and process substitutions found inside node.
func (s *state) substs(node syntax.Node) {
	if node == nil {
		return
	}
	syntax.Walk(node, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.CmdSubst:
			ch := s.child()
			ch.stmts(x.Stmts)
			s.merge(ch)
			return false
		case *syntax.ProcSubst:
			ch := s.child()
			ch.stmts(x.Stmts)
			s.merge(ch)
			return false
		}
		return true
	})
}

func (s *state) assign(a *syntax.Assign) {
	if a == nil || a.Name == nil {
		return
	}
	if a.Value == nil {
		if !a.Naked {
			s.vars[a.Name.Value] = ""
		}
		return
	}
	v, err := expand.Literal(s.config(), a.Value)
	if err != nil {
		s.vars[a.Name.Value] = "\x00unknown"
		return
	}
	s.vars[a.Name.Value] = v
}

// unknownEnv reports unset variables as unknown instead of empty: the
// interactive shell may hold variables we cannot see, and guessing "empty"
// could turn "$DIR/build" into "/build".
type unknownEnv struct{ expand.Environ }

func (e unknownEnv) Get(name string) expand.Variable {
	v := e.Environ.Get(name)
	if v.IsSet() || name == "IFS" || strings.HasPrefix(name, "HOME ") || name == "?" || name == "$" || name == "#" {
		return v
	}
	return expand.Variable{Set: true, Kind: expand.String, Str: "\x00unknown"}
}

type errSubst struct{}

func (errSubst) Error() string { return "command substitution" }

func (s *state) config() *expand.Config {
	env := make([]string, 0, len(s.env)+len(s.vars)+1)
	env = append(env, s.env...)
	for k, v := range s.vars {
		env = append(env, k+"="+v)
	}
	env = append(env, "PWD="+s.cwd)
	return &expand.Config{
		Env:      unknownEnv{expand.ListEnviron(env...)},
		ReadDir2: os.ReadDir,
		CmdSubst: func(io.Writer, *syntax.CmdSubst) error { return errSubst{} },
		ProcSubst: func(*syntax.ProcSubst) (string, error) {
			return "", errSubst{}
		},
	}
}

// fields expands words the way the shell would. ok is false when some part
// could not be resolved (command substitution or an unknown variable).
func (s *state) fields(words []*syntax.Word) ([]string, bool) {
	cfg := s.config()
	out, err := expand.Fields(cfg, words...)
	if err == nil && !containsUnknown(out) {
		return out, true
	}
	// Resolve word by word, dropping the parts we cannot know.
	out = out[:0]
	for _, w := range words {
		f, err := expand.Fields(s.config(), w)
		if err != nil || containsUnknown(f) {
			continue
		}
		out = append(out, f...)
	}
	return out, false
}

func containsUnknown(list []string) bool {
	for _, f := range list {
		if strings.Contains(f, "\x00unknown") {
			return true
		}
	}
	return false
}

func (s *state) abs(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil && (p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`)) {
			p = home + p[1:]
		}
	}
	return absFrom(s.cwd, p)
}

// pathTargets turns raw operands into absolute paths. A trailing slash on a
// symlink means the command acts on the link target, so both are kept.
func (s *state) pathTargets(ops []string) []string {
	var out []string
	for _, op := range ops {
		if op == "" {
			continue
		}
		a := s.abs(op)
		out = append(out, a)
		if strings.HasSuffix(op, "/") && len(op) > 1 {
			if fi, err := os.Lstat(a); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				if t, err := filepath.EvalSymlinks(a); err == nil {
					out = append(out, t)
				}
			}
		}
	}
	return out
}

func (s *state) redir(r *syntax.Redirect) {
	if r.Word != nil {
		s.substs(r.Word)
	}
	switch r.Op {
	case syntax.RdrOut, syntax.ClbOut, syntax.RdrAll:
	default:
		return
	}
	if r.Word == nil {
		return
	}
	target, err := expand.Literal(s.config(), r.Word)
	if err != nil || target == "" || strings.HasPrefix(target, "/dev/") {
		return
	}
	p := s.abs(target)
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		return
	}
	s.add(Action{Program: ">", Paths: []string{p}, Note: "redirect overwrites file"})
}

var gnuNames = map[string]bool{"rm": true, "mv": true, "cp": true, "ln": true, "sed": true, "dd": true, "tee": true,
	"find": true, "shred": true, "unlink": true, "rmdir": true, "truncate": true, "install": true}

func progName(arg string) string {
	n := filepath.Base(strings.TrimPrefix(arg, `\`))
	if strings.HasPrefix(n, "g") && gnuNames[n[1:]] {
		n = n[1:]
	}
	return n
}

// dispatch looks at one simple command. unresolved means some operands could
// not be expanded, so a broader snapshot is taken.
func (s *state) dispatch(argv []string, unresolved bool) {
	for len(argv) > 0 {
		name := progName(argv[0])
		rest := argv[1:]
		switch name {
		case "sudo", "doas":
			argv = skipOpts(rest, "ugphCUrtTDR", true)
			continue
		case "env":
			argv = s.envArgs(rest)
			continue
		case "command", "builtin", "nohup", "and", "or", "not", "!", "chronic", "unbuffer", "noglob", "nocorrect", "exec", "caffeinate", "stdbuf", "ionice":
			argv = skipOpts(rest, "anocwt", false)
			continue
		case "time":
			argv = skipOpts(rest, "fo", false)
			continue
		case "nice":
			argv = skipOpts(rest, "n", false)
			continue
		case "timeout", "gtimeout":
			a := skipOpts(rest, "sk", false)
			if len(a) > 0 {
				a = a[1:]
			}
			argv = a
			continue
		case "xargs", "gxargs":
			a := skipOpts(rest, "IiLlnPsdEaR", false)
			if len(a) == 0 {
				return
			}
			s.dispatchExtra(a)
			return
		case "sh", "bash", "zsh", "dash", "ksh", "fish", "mksh", "busybox":
			if name == "busybox" {
				argv = rest
				continue
			}
			s.shellC(rest)
			return
		case "eval":
			s.line(strings.Join(rest, " "))
			return
		case "cd", "pushd":
			s.cd(rest)
			return
		}
		break
	}
	if len(argv) == 0 {
		return
	}
	name := progName(argv[0])
	args := argv[1:]
	h, ok := handlers[name]
	if !ok {
		return
	}
	before := len(s.actions)
	h(s, name, args)
	if unresolved && len(s.actions) == before && pathCommands[name] {
		s.broad(name, "some arguments could not be resolved before running")
	}
}

// dispatchExtra handles commands whose operands arrive from stdin (xargs).
func (s *state) dispatchExtra(argv []string) {
	name := progName(argv[0])
	if pathCommands[name] {
		before := len(s.actions)
		if h, ok := handlers[name]; ok {
			h(s, name, argv[1:])
		}
		s.actions = s.actions[:before]
		s.broad(name, "targets arrive through xargs")
		return
	}
	s.dispatch(argv, true)
}

func (s *state) broad(name, note string) {
	s.add(Action{Program: name, Paths: []string{s.cwd}, Broad: true, Note: note, Delete: deleteCommands[name]})
}

// deleteCommands only remove files. shred is missing on purpose: it
// overwrites the data in place before unlinking.
var deleteCommands = map[string]bool{"rm": true, "srm": true, "unlink": true, "rmdir": true}

var pathCommands = map[string]bool{"rm": true, "srm": true, "mv": true, "cp": true, "ln": true, "install": true, "shred": true,
	"unlink": true, "rmdir": true, "truncate": true, "sed": true, "perl": true, "tee": true, "dd": true, "find": true, "rsync": true}

func (s *state) envArgs(args []string) []string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return args[i+1:]
		case a == "-u" || a == "-C" || a == "-S" || a == "-P":
			if a == "-C" && i+1 < len(args) {
				s.cwd = s.abs(args[i+1])
			}
			if a == "-S" && i+1 < len(args) {
				return append(strings.Fields(args[i+1]), args[i+2:]...)
			}
			i++
		case strings.HasPrefix(a, "-"):
		case strings.Contains(a, "=") && !strings.HasPrefix(a, "="):
			kv := strings.SplitN(a, "=", 2)
			s.vars[kv[0]] = kv[1]
		default:
			return args[i:]
		}
	}
	return nil
}

func (s *state) shellC(args []string) {
	for i, a := range args {
		if a == "--" {
			return
		}
		if !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+") {
			return // a script file: we cannot see inside it
		}
		if !strings.HasPrefix(a, "--") && strings.Contains(a, "c") && i+1 < len(args) {
			s.line(args[i+1])
			return
		}
	}
}

func (s *state) cd(args []string) {
	ops := operands(args, "")
	if len(ops) == 0 {
		if home, err := os.UserHomeDir(); err == nil {
			s.cwd = home
		}
		return
	}
	if ops[0] == "-" {
		return
	}
	p := s.abs(ops[0])
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		s.cwd = p
	}
}

// skipOpts drops leading options. withArg lists short options that take a
// value. When stopAtDashDash is set, "--" ends option parsing.
func skipOpts(args []string, withArg string, stopAtDashDash bool) []string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return args[i+1:]
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			return args[i:]
		}
		if strings.HasPrefix(a, "--") {
			continue
		}
		last := a[len(a)-1:]
		if len(a) == 2 && strings.Contains(withArg, last) {
			i++
		}
	}
	return nil
}

// parsed is a generic command line split into options and operands.
type parsed struct {
	short    map[byte]bool
	long     map[string]string
	values   map[byte]string
	operands []string
}

func (p parsed) has(short byte, long ...string) bool {
	if short != 0 && p.short[short] {
		return true
	}
	for _, l := range long {
		if _, ok := p.long[l]; ok {
			return true
		}
	}
	return false
}

func (p parsed) longVal(name string) (string, bool) {
	v, ok := p.long[name]
	return v, ok
}

// parse splits args. withArg are short options taking a value; longArg are
// long options taking a separate value when written without "=".
func parse(args []string, withArg string, longArg ...string) parsed {
	p := parsed{short: map[byte]bool{}, long: map[string]string{}, values: map[byte]string{}}
	la := map[string]bool{}
	for _, l := range longArg {
		la[l] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			p.operands = append(p.operands, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "--") {
			name, val, hasVal := strings.Cut(a[2:], "=")
			if !hasVal && la[name] && i+1 < len(args) {
				val = args[i+1]
				i++
			}
			p.long[name] = val
			continue
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			for j := 1; j < len(a); j++ {
				c := a[j]
				p.short[c] = true
				if strings.IndexByte(withArg, c) >= 0 {
					if j+1 < len(a) {
						p.values[c] = a[j+1:]
					} else if i+1 < len(args) {
						p.values[c] = args[i+1]
						i++
					}
					break
				}
			}
			continue
		}
		p.operands = append(p.operands, a)
	}
	return p
}

func operands(args []string, withArg string) []string { return parse(args, withArg).operands }

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func isRealDir(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.IsDir()
}

type handler func(s *state, name string, args []string)

var handlers map[string]handler

func init() {
	handlers = map[string]handler{
		"rm":       hRm,
		"srm":      hRm,
		"unlink":   hSimple,
		"shred":    hShred,
		"rmdir":    hRmdir,
		"truncate": hTruncate,
		"mv":       hMvCp,
		"cp":       hMvCp,
		"ln":       hLn,
		"install":  hMvCp,
		"tee":      hTee,
		"dd":       hDd,
		"sed":      hSed,
		"perl":     hPerl,
		"find":     hFind,
		"rsync":    hRsync,
		"git":      hGit,
	}
}

func hRm(s *state, name string, args []string) {
	p := parse(args, "")
	recursive := p.has('r', "recursive") || p.has('R')
	dirFlag := p.has('d', "dir")
	var paths []string
	for _, t := range s.pathTargets(p.operands) {
		if isRealDir(t) && !recursive && !dirFlag {
			continue
		}
		paths = append(paths, t)
	}
	if len(paths) > 0 {
		s.add(Action{Program: name, Paths: paths, Delete: true})
	}
}

func hSimple(s *state, name string, args []string) {
	if paths := s.pathTargets(operands(args, "")); len(paths) > 0 {
		s.add(Action{Program: name, Paths: paths, Delete: deleteCommands[name]})
	}
}

func hShred(s *state, name string, args []string) {
	if paths := s.pathTargets(parse(args, "ns", "iterations", "size", "random-source").operands); len(paths) > 0 {
		s.add(Action{Program: name, Paths: paths})
	}
}

func hRmdir(s *state, name string, args []string) {
	p := parse(args, "")
	var paths []string
	for _, op := range p.operands {
		paths = append(paths, s.abs(op))
		if p.has('p', "parents") {
			d := filepath.Dir(strings.TrimSuffix(op, "/"))
			for d != "." && d != "/" && d != "" {
				paths = append(paths, s.abs(d))
				d = filepath.Dir(d)
			}
		}
	}
	if len(paths) > 0 {
		s.add(Action{Program: name, Paths: paths, Delete: true})
	}
}

func hTruncate(s *state, name string, args []string) {
	p := parse(args, "sr", "size", "reference")
	var paths []string
	for _, t := range s.pathTargets(p.operands) {
		if exists(t) {
			paths = append(paths, t)
		}
	}
	if len(paths) > 0 {
		s.add(Action{Program: name, Paths: paths})
	}
}

func hMvCp(s *state, name string, args []string) {
	withArg := "tS"
	if name == "install" {
		withArg = "tSmogM"
	}
	p := parse(args, withArg, "target-directory", "suffix", "mode", "owner", "group")
	if p.has('n', "no-clobber") {
		return
	}
	if v, ok := p.longVal("update"); ok && v == "none" {
		return
	}
	if name == "install" && p.has('d', "directory") {
		return
	}
	var dest string
	srcs := p.operands
	if t, ok := p.values['t']; ok {
		dest = t
	} else if t, ok := p.longVal("target-directory"); ok {
		dest = t
	} else {
		if len(srcs) < 2 {
			return
		}
		dest = srcs[len(srcs)-1]
		srcs = srcs[:len(srcs)-1]
	}
	paths := destTargets(s, dest, srcs, p.has('T', "no-target-directory"))
	if len(paths) > 0 {
		s.add(Action{Program: name, Paths: paths, Note: "overwrites existing files"})
	}
}

// destTargets lists existing paths a copy, move or link onto dest would replace.
func destTargets(s *state, dest string, srcs []string, noTargetDir bool) []string {
	d := s.abs(dest)
	var out []string
	if isDir(d) && !noTargetDir {
		for _, src := range srcs {
			if strings.HasSuffix(src, "/") && isDir(s.abs(src)) {
				// BSD cp copies the contents of src/ into dest.
				out = append(out, d)
				continue
			}
			t := filepath.Join(d, filepath.Base(strings.TrimSuffix(src, "/")))
			if exists(t) {
				out = append(out, t)
			}
		}
		return out
	}
	if exists(d) {
		out = append(out, d)
	}
	return out
}

func hLn(s *state, name string, args []string) {
	p := parse(args, "tS", "target-directory", "suffix")
	if !p.has('f', "force") {
		return
	}
	var dest string
	srcs := p.operands
	if t, ok := p.values['t']; ok {
		dest = t
	} else {
		if len(srcs) < 2 {
			return
		}
		dest = srcs[len(srcs)-1]
		srcs = srcs[:len(srcs)-1]
	}
	d := s.abs(dest)
	var paths []string
	noDeref := p.has('n', "no-dereference") || p.has('h') || p.has('T', "no-target-directory")
	if fi, err := os.Lstat(d); err == nil && !fi.IsDir() {
		paths = append(paths, d)
	}
	if !noDeref && isDir(d) {
		for _, src := range srcs {
			t := filepath.Join(d, filepath.Base(src))
			if exists(t) {
				paths = append(paths, t)
			}
		}
	}
	if len(paths) > 0 {
		s.add(Action{Program: name, Paths: paths, Note: "replaces existing files"})
	}
}

func hTee(s *state, name string, args []string) {
	p := parse(args, "")
	if p.has('a', "append") {
		return
	}
	var paths []string
	for _, t := range s.pathTargets(p.operands) {
		if fi, err := os.Stat(t); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
			paths = append(paths, t)
		}
	}
	if len(paths) > 0 {
		s.add(Action{Program: name, Paths: paths, Note: "overwrites files"})
	}
}

func hDd(s *state, name string, args []string) {
	for _, a := range args {
		if strings.HasPrefix(a, "of=") {
			t := s.abs(a[3:])
			if strings.HasPrefix(t, "/dev/") {
				s.add(Action{Program: name, Note: "writes to a device, which cannot be snapshotted: " + t})
				return
			}
			if exists(t) {
				s.add(Action{Program: name, Paths: []string{t}})
			}
		}
	}
}

func existingFiles(s *state, ops []string) []string {
	var out []string
	for _, op := range ops {
		t := s.abs(op)
		if fi, err := os.Stat(t); err == nil && fi.Mode().IsRegular() {
			out = append(out, t)
		}
	}
	return out
}

func hSed(s *state, name string, args []string) {
	inplace := false
	var ops []string
	scriptGiven := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			ops = append(ops, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "--") {
			n, _, hasVal := strings.Cut(a[2:], "=")
			switch {
			case strings.HasPrefix(n, "in-place"):
				inplace = true
			case n == "expression" || n == "file":
				scriptGiven = true
				if !hasVal {
					i++
				}
			}
			continue
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
		cluster:
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 'i':
					inplace = true
					break cluster // rest of cluster is the backup suffix
				case 'e', 'f':
					scriptGiven = true
					if j == len(a)-1 {
						i++
					}
					break cluster
				case 'l':
					if j == len(a)-1 {
						i++
					}
					break cluster
				}
			}
			continue
		}
		ops = append(ops, a)
	}
	if !inplace {
		return
	}
	_ = scriptGiven // the script operand never names an existing file in practice; keep every existing file
	if paths := existingFiles(s, ops); len(paths) > 0 {
		s.add(Action{Program: name, Paths: paths, Note: "edits files in place"})
	}
}

func hPerl(s *state, name string, args []string) {
	inplace := false
	var ops []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			ops = append(ops, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 && !strings.HasPrefix(a, "--") {
		cluster:
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 'i':
					inplace = true
					break cluster
				case 'e', 'E':
					if j == len(a)-1 {
						i++
					}
					break cluster
				case 'M', 'm', 'I', 'x', 'C', 'd', 'D':
					break cluster
				}
			}
			continue
		}
		ops = append(ops, a)
	}
	if !inplace {
		return
	}
	if paths := existingFiles(s, ops); len(paths) > 0 {
		s.add(Action{Program: name, Paths: paths, Note: "edits files in place"})
	}
}

var findDestructiveExec = map[string]bool{"rm": true, "srm": true, "shred": true, "unlink": true, "rmdir": true, "mv": true, "truncate": true}

func hFind(s *state, name string, args []string) {
	// Roots come before the first expression token.
	i := 0
	for i < len(args) && (args[i] == "-H" || args[i] == "-L" || args[i] == "-P" || args[i] == "-E" || args[i] == "-X" || args[i] == "-s" || args[i] == "-x" || args[i] == "-d") {
		i++
	}
	var roots []string
	for i < len(args) {
		a := args[i]
		if strings.HasPrefix(a, "-") || a == "(" || a == "!" || a == "\\(" {
			break
		}
		roots = append(roots, a)
		i++
	}
	expr := args[i:]
	destructive := false
	onlyDeletes := true
	safeToDryRun := true
	var dry []string
	for j := 0; j < len(expr); j++ {
		a := expr[j]
		switch a {
		case "-delete":
			destructive = true
			dry = append(dry, "-print")
		case "-exec", "-execdir", "-ok", "-okdir":
			end := j + 1
			for end < len(expr) && expr[end] != ";" && expr[end] != "+" {
				end++
			}
			if j+1 < len(expr) && findDestructiveExec[progName(expr[j+1])] {
				destructive = true
				onlyDeletes = onlyDeletes && deleteCommands[progName(expr[j+1])]
				dry = append(dry, "-print")
			} else if j+1 < len(expr) && (expr[j+1] == "sh" || expr[j+1] == "bash") {
				destructive = true
				onlyDeletes = false
				safeToDryRun = false
			} else {
				safeToDryRun = false
			}
			j = end
		case "-fprint", "-fprint0", "-fprintf", "-fls":
			safeToDryRun = false
			dry = append(dry, a)
		default:
			dry = append(dry, a)
		}
	}
	if !destructive {
		return
	}
	if len(roots) == 0 {
		roots = []string{"."}
	}
	if safeToDryRun {
		if list, ok := findDryRun(s.cwd, args[:len(args)-len(expr)], dry); ok {
			var paths []string
			for _, l := range list {
				paths = append(paths, s.abs(l))
			}
			if len(paths) > 0 {
				s.add(Action{Program: name, Paths: paths, Note: "files matched by find", Delete: onlyDeletes})
			}
			return
		}
	}
	s.add(Action{Program: name, Paths: s.pathTargets(roots), Broad: true, Note: "find roots saved whole", Delete: onlyDeletes})
}

func findDryRun(cwd string, head, expr []string) ([]string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	argv := append(append([]string{}, head...), expr...)
	cmd := exec.CommandContext(ctx, "find", argv...)
	cmd.Dir = cwd
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || out.Len() == 0 {
			return nil, false
		}
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) > 20000 {
		return nil, false
	}
	var res []string
	for _, l := range lines {
		if l != "" {
			res = append(res, l)
		}
	}
	return res, true
}

func hRsync(s *state, name string, args []string) {
	p := parse(args, "eTfB", "rsh", "temp-dir", "filter", "exclude", "include", "files-from", "backup-dir", "suffix", "rsync-path", "exclude-from", "include-from", "log-file", "partial-dir", "compare-dest", "link-dest", "copy-dest")
	deletes := false
	for l := range p.long {
		if strings.HasPrefix(l, "delete") || l == "del" {
			deletes = true
		}
	}
	if len(p.operands) < 2 {
		return
	}
	dest := p.operands[len(p.operands)-1]
	srcs := p.operands[:len(p.operands)-1]
	remote := func(x string) bool {
		if strings.HasPrefix(x, "rsync://") {
			return true
		}
		i := strings.Index(x, ":")
		return i > 0 && !strings.Contains(x[:i], "/")
	}
	var paths []string
	if _, ok := p.long["remove-source-files"]; ok {
		for _, src := range srcs {
			if !remote(src) {
				paths = append(paths, s.abs(src))
			}
		}
	}
	if remote(dest) {
		if deletes {
			s.add(Action{Program: name, Note: "rsync --delete to a remote host cannot be snapshotted"})
		}
		if len(paths) > 0 {
			s.add(Action{Program: name, Paths: paths})
		}
		return
	}
	d := s.abs(dest)
	if deletes && exists(d) {
		paths = append(paths, d)
	} else if fi, err := os.Stat(d); err == nil {
		if fi.IsDir() {
			paths = append(paths, destTargets(s, dest, srcs, false)...)
		} else {
			paths = append(paths, d)
		}
	}
	if len(paths) > 0 {
		s.add(Action{Program: name, Paths: paths})
	}
}

func hGit(s *state, name string, args []string) {
	repo := s.cwd
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "-C" && i+1 < len(args):
			repo = absFrom(repo, args[i+1])
			i += 2
			continue
		case a == "-c" && i+1 < len(args):
			i += 2
			continue
		case strings.HasPrefix(a, "--work-tree="):
			repo = absFrom(repo, strings.TrimPrefix(a, "--work-tree="))
		case a == "--work-tree" && i+1 < len(args):
			repo = absFrom(repo, args[i+1])
			i += 2
			continue
		case (a == "--git-dir" || a == "--namespace" || a == "--exec-path") && i+1 < len(args):
			i += 2
			continue
		case strings.HasPrefix(a, "-"):
		default:
			goto sub
		}
		i++
	}
	return
sub:
	sub := args[i]
	rest := args[i+1:]
	gs := &state{cwd: repo, env: s.env, vars: s.vars}
	switch sub {
	case "reset":
		p := parse(rest, "")
		if p.has(0, "hard", "merge", "keep") {
			s.add(Action{Program: "git reset", Git: &GitOp{Kind: "reset-hard", Repo: repo, Args: rest}})
		}
	case "checkout":
		p := parse(rest, "bB", "orphan", "conflict", "pathspec-from-file")
		if p.has('b') || p.has('B') || p.has(0, "orphan") {
			if len(p.operands) <= 1 && !p.has('f', "force") {
				return
			}
		}
		if p.has('f', "force") {
			s.add(Action{Program: "git checkout", Git: &GitOp{Kind: "reset-hard", Repo: repo, Args: rest}})
			return
		}
		if _, ok := p.long["pathspec-from-file"]; ok {
			s.add(Action{Program: "git checkout", Git: &GitOp{Kind: "checkout-paths", Repo: repo, Paths: []string{"."}}})
			return
		}
		paths := checkoutPaths(gs, rest)
		if len(paths) > 0 {
			s.add(Action{Program: "git checkout", Git: &GitOp{Kind: "checkout-paths", Repo: repo, Paths: paths}})
		}
	case "restore":
		p := parse(rest, "s", "source", "pathspec-from-file", "conflict")
		if p.has('S', "staged") && !p.has('W', "worktree") {
			return
		}
		paths := p.operands
		if _, ok := p.long["pathspec-from-file"]; ok {
			paths = []string{"."}
		}
		if len(paths) > 0 {
			s.add(Action{Program: "git restore", Git: &GitOp{Kind: "checkout-paths", Repo: repo, Paths: paths}})
		}
	case "switch":
		p := parse(rest, "cC", "create", "force-create", "orphan", "conflict")
		if p.has('f', "force", "discard-changes") {
			s.add(Action{Program: "git switch", Git: &GitOp{Kind: "reset-hard", Repo: repo, Args: rest}})
		}
	case "clean":
		p := parse(rest, "e", "exclude")
		if p.has('n', "dry-run") {
			return
		}
		if p.has('f', "force") || p.has('i', "interactive") {
			s.add(Action{Program: "git clean", Git: &GitOp{Kind: "clean", Repo: repo, Args: rest}})
		}
	case "branch":
		p := parse(rest, "u", "set-upstream-to")
		del := p.has('D') || p.has('d', "delete")
		if !del || len(p.operands) == 0 {
			return
		}
		op := &GitOp{Kind: "branch-delete", Repo: repo, Branches: p.operands}
		if p.has('r', "remotes") {
			op.Remote = "remote-tracking"
		}
		s.add(Action{Program: "git branch", Git: op})
	case "stash":
		if len(rest) == 0 {
			return
		}
		switch rest[0] {
		case "drop", "pop":
			refs := operands(rest[1:], "")
			if len(refs) == 0 {
				refs = []string{"stash@{0}"}
			}
			s.add(Action{Program: "git stash " + rest[0], Git: &GitOp{Kind: "stash-drop", Repo: repo, StashRefs: refs}})
		case "clear":
			s.add(Action{Program: "git stash clear", Git: &GitOp{Kind: "stash-clear", Repo: repo}})
		}
	case "push":
		p := parse(rest, "o", "push-option", "repo", "receive-pack", "exec")
		force := p.has('f', "force", "force-if-includes", "mirror", "delete") || p.has('d')
		for l := range p.long {
			if strings.HasPrefix(l, "force-with-lease") {
				force = true
			}
		}
		var remote string
		var specs []string
		if len(p.operands) > 0 {
			remote = p.operands[0]
			specs = p.operands[1:]
		}
		for _, sp := range specs {
			if strings.HasPrefix(sp, "+") || strings.HasPrefix(sp, ":") {
				force = true
			}
		}
		if !force {
			return
		}
		s.add(Action{Program: "git push", Git: &GitOp{Kind: "push-force", Repo: repo, Remote: remote, Refspecs: specs, Args: rest}})
	case "rm":
		p := parse(rest, "", "pathspec-from-file")
		if p.has(0, "cached") || p.has('n', "dry-run") {
			return
		}
		if paths := gs.pathTargets(p.operands); len(paths) > 0 {
			s.add(Action{Program: "git rm", Paths: paths, Delete: true})
		}
	case "mv":
		p := parse(rest, "")
		if !p.has('f', "force") || len(p.operands) < 2 {
			return
		}
		ops := p.operands
		if paths := destTargets(gs, ops[len(ops)-1], ops[:len(ops)-1], false); len(paths) > 0 {
			s.add(Action{Program: "git mv", Paths: paths})
		}
	case "worktree":
		if len(rest) > 1 && rest[0] == "remove" {
			ops := operands(rest[1:], "")
			if paths := gs.pathTargets(ops); len(paths) > 0 {
				s.add(Action{Program: "git worktree remove", Paths: paths, Delete: true})
			}
		}
	}
}

func absFrom(base, p string) string {
	if runtime.GOOS == "windows" {
		p = msysPath(p, base)
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if runtime.GOOS == "windows" && len(p) > 0 && os.IsPathSeparator(p[0]) {
		// \foo is relative to the root of the current drive.
		return filepath.Clean(filepath.VolumeName(base) + p)
	}
	return filepath.Join(base, p)
}

// checkoutPaths returns pathspecs a git checkout will overwrite.
func checkoutPaths(s *state, args []string) []string {
	var before, after []string
	dd := false
	for _, a := range args {
		if a == "--" && !dd {
			dd = true
			continue
		}
		if !dd && strings.HasPrefix(a, "-") {
			continue
		}
		if dd {
			after = append(after, a)
		} else {
			before = append(before, a)
		}
	}
	if dd {
		return after
	}
	if len(before) == 0 {
		return nil
	}
	if exists(s.abs(before[0])) || before[0] == "." {
		return before
	}
	return before[1:]
}
