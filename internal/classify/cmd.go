package classify

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ClassifyCmd parses line as a cmd.exe command line running in cwd.
func ClassifyCmd(line, cwd string, env []string) Result {
	if !MaybeDestructiveWindows(line) {
		return Result{}
	}
	c := newCmd(cwd, env)
	c.line(line)
	return Result{Actions: c.actions}
}

type cmdState struct {
	cwd     string
	env     map[string]string
	rawEnv  []string
	depth   int
	actions []Action
}

func newCmd(cwd string, env []string) *cmdState {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	c := &cmdState{cwd: cwd, env: map[string]string{}, rawEnv: env}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			c.env[strings.ToUpper(k)] = v
		}
	}
	return c
}

var cmdVar = regexp.MustCompile(`%([^%\s]+)%`)

// expandVars does cmd's %NAME% expansion. Unknown names and substring forms
// become unknown rather than empty, so they lead to a broader snapshot.
func (c *cmdState) expandVars(s string) string {
	return cmdVar.ReplaceAllStringFunc(s, func(m string) string {
		name := strings.ToUpper(m[1 : len(m)-1])
		switch name {
		case "CD":
			return c.cwd
		}
		if strings.Contains(name, ":") {
			return unknown
		}
		if v, ok := c.env[name]; ok && v != "" {
			return v
		}
		return unknown
	})
}

var cmdFor = regexp.MustCompile(`(?is)^\s*@?for\s+((?:/[a-z]\s+)*)(?:"[^"]*"\s+)?%%?(\w)\s+in\s*\((.*?)\)\s*do\s+(.*)$`)

func (c *cmdState) line(s string) {
	if c.depth > 8 {
		return
	}
	c.depth++
	defer func() { c.depth-- }()
	if m := cmdFor.FindStringSubmatch(s); m != nil {
		c.forLoop(strings.ToLower(m[1]), m[2], m[3], m[4])
		return
	}
	for _, seg := range splitCmd(c.expandVars(s)) {
		c.segment(seg)
	}
}

// forLoop runs the body once per item of the set, like cmd would.
func (c *cmdState) forLoop(flags, v, set, body string) {
	var items []string
	known := !strings.Contains(flags, "/f") && !strings.Contains(flags, "/r") && !strings.Contains(set, unknown)
	for _, w := range strings.Fields(c.expandVars(set)) {
		w = strings.Trim(w, `"`)
		if hasWild(w) {
			items = append(items, psGlob(psResolve(w, c.cwd))...)
		} else {
			items = append(items, w)
		}
	}
	if !known || len(items) == 0 || len(items) > 64 {
		items = []string{unknown}
	}
	re := regexp.MustCompile(`%%?~?[a-z]*` + regexp.QuoteMeta(v) + `\b`)
	for _, it := range items {
		c.line(re.ReplaceAllLiteralString(body, it))
	}
}

// cmdWord is one argument with its quotes removed.
type cmdWord struct {
	s  string
	ok bool
}

type cmdSegment struct {
	words  []cmdWord
	redirs []cmdWord // targets of > (not >>)
}

// splitCmd splits a line into simple commands at & && || | and parentheses,
// honoring "quotes" and ^ escapes, and pulls out redirections.
func splitCmd(s string) []cmdSegment {
	var segs []cmdSegment
	var cur cmdSegment
	var b strings.Builder
	inWord, quoted := false, false
	redir := 0 // 0 none, 1 >, 2 >>
	endWord := func() {
		if !inWord {
			return
		}
		w := cmdWord{s: b.String(), ok: !strings.Contains(b.String(), unknown)}
		switch redir {
		case 1:
			cur.redirs = append(cur.redirs, w)
		case 2:
		default:
			cur.words = append(cur.words, w)
		}
		redir = 0
		b.Reset()
		inWord = false
	}
	endSeg := func() {
		endWord()
		if len(cur.words) > 0 || len(cur.redirs) > 0 {
			segs = append(segs, cur)
		}
		cur = cmdSegment{}
	}
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if quoted {
			if r == '"' {
				quoted = false
			} else {
				b.WriteRune(r)
			}
			continue
		}
		switch r {
		case '"':
			quoted, inWord = true, true
		case '^':
			if i+1 < len(rs) {
				i++
				b.WriteRune(rs[i])
				inWord = true
			}
		case '&', '|', '(', ')', '\n', '\r':
			endSeg()
			if i+1 < len(rs) && (rs[i+1] == '&' || rs[i+1] == '|') && rs[i+1] == r {
				i++
			}
		case '>':
			// 2> and 1> name a stream; the digit is not part of a word.
			if inWord && (b.String() == "1" || b.String() == "2") {
				b.Reset()
				inWord = false
			}
			endWord()
			redir = 1
			if i+1 < len(rs) && rs[i+1] == '>' {
				redir = 2
				i++
			}
			if i+1 < len(rs) && rs[i+1] == '&' {
				redir = 0 // 2>&1
				i += 2
			}
		case '<':
			endWord()
			redir = 2 // input, ignore the file
		case ' ', '\t', ',', ';':
			endWord()
		default:
			b.WriteRune(r)
			inWord = true
		}
	}
	endSeg()
	return segs
}

func (c *cmdState) segment(seg cmdSegment) {
	for _, r := range seg.redirs {
		if !r.ok {
			continue
		}
		switch strings.ToLower(r.s) {
		case "nul", "con", "prn", "aux":
			continue
		}
		if p := psResolve(r.s, c.cwd); p != "" {
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
				c.actions = append(c.actions, Action{Program: ">", Paths: []string{p}, Note: "redirect overwrites file"})
			}
		}
	}
	var argv []string
	ok := true
	for _, w := range seg.words {
		argv = append(argv, w.s)
		ok = ok && w.ok
	}
	if len(argv) > 0 {
		c.dispatchOK(argv, ok)
	}
}

func (c *cmdState) dispatch(argv []string) { c.dispatchOK(argv, true) }

// cmdName lower cases a program name and drops its folder and extension;
// unlike PowerShell, cmd has no aliases.
func cmdName(n string) string {
	n = strings.ToLower(strings.TrimPrefix(n, "@"))
	if i := strings.LastIndexAny(n, `\/`); i >= 0 {
		n = n[i+1:]
	}
	for _, ext := range []string{".exe", ".cmd", ".bat", ".com"} {
		n = strings.TrimSuffix(n, ext)
	}
	return n
}

// cmdArgs splits /switches from operands. A switch has no further slashes,
// which keeps absolute Unix paths (in tests) as operands.
func cmdArgs(args []string) (map[string]bool, []string) {
	sw := map[string]bool{}
	var ops []string
	for _, a := range args {
		if strings.HasPrefix(a, "/") && len(a) > 1 && len(a) <= 12 && !strings.ContainsAny(a[1:], `/\`) {
			sw[strings.ToLower(a[1:])] = true
			continue
		}
		ops = append(ops, a)
	}
	return sw, ops
}

func (c *cmdState) dispatchOK(argv []string, resolved bool) {
	if len(argv) == 0 {
		return
	}
	name := cmdName(argv[0])
	args := argv[1:]
	switch name {
	case "if":
		c.dispatchOK(skipIf(args), resolved)
		return
	case "else", "do":
		if len(args) > 0 {
			c.dispatchOK(args, resolved)
		}
		return
	case "cd", "chdir", "pushd":
		var parts []string
		for _, a := range args {
			if !strings.EqualFold(a, "/d") {
				parts = append(parts, a)
			}
		}
		if p := psResolve(strings.Join(parts, " "), c.cwd); p != "" && isDir(p) {
			c.cwd = p
		}
		return
	}
	before := len(c.actions)
	switch name {
	case "del", "erase":
		sw, ops := cmdArgs(args)
		var paths []string
		for _, op := range ops {
			paths = append(paths, c.delTargets(op, sw["s"])...)
		}
		if len(paths) > 0 {
			c.actions = append(c.actions, Action{Program: name, Paths: paths, Delete: true})
		}
	case "rd", "rmdir":
		_, ops := cmdArgs(args)
		var paths []string
		for _, op := range ops {
			if p := psResolve(op, c.cwd); p != "" {
				paths = append(paths, p)
			}
		}
		if len(paths) > 0 {
			c.actions = append(c.actions, Action{Program: name, Paths: paths, Delete: true})
		}
	case "move", "copy":
		_, ops := cmdArgs(args)
		var srcs []string
		dest := c.cwd
		if len(ops) >= 2 {
			dest = psResolve(ops[len(ops)-1], c.cwd)
			ops = ops[:len(ops)-1]
		}
		for _, op := range ops {
			for _, part := range strings.Split(op, "+") {
				if part = strings.TrimSpace(part); part != "" {
					srcs = append(srcs, psResolve(part, c.cwd))
				}
			}
		}
		if paths := cmdDest(dest, srcs); len(paths) > 0 {
			c.actions = append(c.actions, Action{Program: name, Paths: paths, Note: "overwrites existing files"})
		}
	case "xcopy", "robocopy":
		sw, ops := cmdArgs(args)
		if len(ops) == 0 {
			return
		}
		dest := c.cwd
		if len(ops) >= 2 {
			dest = psResolve(ops[1], c.cwd)
		}
		if dest != "" && exists(dest) {
			c.actions = append(c.actions, Action{Program: name, Paths: []string{dest}, Note: "copies over existing files"})
		}
		if name == "robocopy" && (sw["mov"] || sw["move"]) {
			if src := psResolve(ops[0], c.cwd); src != "" {
				c.actions = append(c.actions, Action{Program: name, Paths: []string{src}, Delete: true, Note: "moves files away"})
			}
		}
	case "git":
		bs := newState(c.cwd, c.rawEnv)
		hGit(bs, "git", args)
		c.actions = append(c.actions, bs.actions...)
	case "cmd":
		if line := cmdSlashC(args); line != "" {
			c.line(line)
		}
	case "powershell", "pwsh":
		if script := psCommandArg(args); script != "" {
			c.actions = append(c.actions, ClassifyPowerShell(script, c.cwd, c.rawEnv).Actions...)
		}
	case "bash", "sh", "wsl":
		bs := newState(c.cwd, c.rawEnv)
		if name == "wsl" {
			bs.dispatch(args, false)
		} else {
			bs.shellC(args)
		}
		c.actions = append(c.actions, bs.actions...)
	default:
		return
	}
	if !resolved && len(c.actions) == before && name != "git" {
		c.actions = append(c.actions, Action{Program: name, Paths: []string{c.cwd}, Broad: true,
			Delete: name == "del" || name == "erase" || name == "rd" || name == "rmdir",
			Note:   "some arguments are only known when the command runs"})
	}
}

// skipIf drops an if condition and returns the command it guards.
func skipIf(args []string) []string {
	i := 0
	for i < len(args) && (strings.EqualFold(args[i], "/i") || strings.EqualFold(args[i], "not")) {
		i++
	}
	if i >= len(args) {
		return nil
	}
	switch strings.ToLower(args[i]) {
	case "exist", "defined", "errorlevel", "cmdextversion":
		i += 2
	default:
		if i+1 < len(args) {
			switch strings.ToLower(args[i+1]) {
			case "==", "equ", "neq", "lss", "leq", "gtr", "geq":
				i += 3
			default:
				i++
			}
		} else {
			i++
		}
	}
	if i >= len(args) {
		return nil
	}
	return args[i:]
}

// delTargets lists what del would remove for one operand. A folder means
// the files in it; /s means matching files in every subfolder too.
func (c *cmdState) delTargets(op string, recursive bool) []string {
	p := psResolve(op, c.cwd)
	if p == "" {
		return nil
	}
	if !recursive {
		if hasWild(op) {
			return psGlob(p)
		}
		if exists(p) {
			return []string{p}
		}
		return nil
	}
	dir, pattern := filepath.Dir(p), filepath.Base(p)
	if !hasWild(pattern) && isDir(p) {
		return []string{p}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var out []string
	seen := 0
	tooMany := false
	_ = filepath.WalkDir(dir, func(q string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		seen++
		if seen > maxListed || ctx.Err() != nil {
			tooMany = true
			return filepath.SkipAll
		}
		if wildMatch(pattern, d.Name()) {
			out = append(out, q)
		}
		return nil
	})
	if tooMany {
		return []string{dir}
	}
	return out
}

// cmdDest lists existing files a copy or move onto dest would replace.
func cmdDest(dest string, srcs []string) []string {
	if dest == "" {
		return nil
	}
	if isDir(dest) {
		var out []string
		for _, src := range srcs {
			names := []string{filepath.Base(src)}
			if hasWild(src) {
				names = nil
				for _, m := range psGlob(src) {
					names = append(names, filepath.Base(m))
				}
			}
			for _, n := range names {
				if t := filepath.Join(dest, n); exists(t) && !isDir(t) {
					out = append(out, t)
				}
			}
		}
		return out
	}
	if exists(dest) {
		return []string{dest}
	}
	return nil
}

// cmdSlashC returns the command after cmd /c or /k.
func cmdSlashC(args []string) string {
	for i, a := range args {
		l := strings.ToLower(a)
		if l == "/c" || l == "/k" || l == "/r" {
			line := strings.Join(args[i+1:], " ")
			if len(line) > 1 && line[0] == '"' && line[len(line)-1] == '"' {
				line = line[1 : len(line)-1]
			}
			return line
		}
		if strings.HasPrefix(l, "/c") || strings.HasPrefix(l, "/k") {
			return strings.Trim(a[2:], `"`) + " " + strings.Join(args[i+1:], " ")
		}
	}
	return ""
}
