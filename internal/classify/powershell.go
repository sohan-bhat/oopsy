package classify

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ClassifyPowerShell parses cmd as PowerShell running in cwd.
func ClassifyPowerShell(cmd, cwd string, env []string) Result {
	if !MaybeDestructiveWindows(cmd) {
		return Result{}
	}
	ps := newPS(cwd, env)
	ps.run(cmd, nil)
	return Result{Actions: ps.actions}
}

type psState struct {
	cwd     string
	env     map[string]string // upper case names
	rawEnv  []string
	vars    map[string]string // lower case names
	depth   int
	actions []Action
}

func newPS(cwd string, env []string) *psState {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	ps := &psState{cwd: cwd, env: map[string]string{}, rawEnv: env, vars: map[string]string{}}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			ps.env[strings.ToUpper(k)] = v
		}
	}
	return ps
}

func (ps *psState) child() *psState {
	c := &psState{cwd: ps.cwd, env: ps.env, rawEnv: ps.rawEnv, vars: map[string]string{}, depth: ps.depth + 1}
	for k, v := range ps.vars {
		c.vars[k] = v
	}
	return c
}

func (ps *psState) lookup(name string) (string, bool) {
	n := strings.ToLower(name)
	for _, scope := range []string{"global:", "script:", "local:", "private:"} {
		n = strings.TrimPrefix(n, scope)
	}
	if strings.HasPrefix(n, "env:") {
		v, ok := ps.env[strings.ToUpper(n[4:])]
		return v, ok && v != ""
	}
	switch n {
	case "home":
		h, err := os.UserHomeDir()
		return h, err == nil
	case "pwd":
		return ps.cwd, true
	case "null":
		return "", true
	case "true":
		return "True", true
	case "false":
		return "False", true
	}
	v, ok := ps.vars[n]
	return v, ok
}

// psInput is what flows down a pipeline into a command: paths when the
// source is something like Get-ChildItem, or unknown objects.
type psInput struct {
	paths []string
	known bool
}

// run classifies a script: statements separated by ; newlines && and ||.
func (ps *psState) run(src string, input *psInput) {
	if ps.depth > 8 {
		return
	}
	lx := &psLexer{src: []rune(src), vars: ps.lookup}
	var stmt []psTok
	flush := func() {
		if len(stmt) > 0 {
			ps.statement(stmt, input)
		}
		stmt = nil
	}
	for {
		t, ok := lx.next()
		if !ok {
			break
		}
		if t.kind == tOp && (t.val == ";" || t.val == "&&" || t.val == "||") {
			flush()
			continue
		}
		stmt = append(stmt, t)
	}
	flush()
}

var psKeywords = map[string]bool{"if": true, "elseif": true, "else": true, "for": true, "while": true, "do": true, "until": true,
	"switch": true, "try": true, "catch": true, "finally": true, "function": true, "filter": true, "trap": true,
	"begin": true, "process": true, "end": true, "param": true, "return": true, "throw": true, "exit": true, "break": true, "continue": true}

func (ps *psState) statement(toks []psTok, input *psInput) {
	first := toks[0]
	// $x = value
	if first.kind == tWord && strings.HasPrefix(first.raw, "$") && len(toks) >= 2 && toks[1].kind == tWord && (toks[1].raw == "=" || toks[1].raw == "+=") {
		name := strings.ToLower(strings.Trim(first.raw[1:], "{}"))
		if len(toks) == 3 && toks[2].kind == tWord && toks[2].ok && toks[1].raw == "=" {
			ps.vars[name] = toks[2].val
		} else {
			ps.vars[name] = unknown
			ps.pipeline(toks[2:], input) // $x = Remove-Item ... still runs
		}
		return
	}
	if first.kind == tWord && !first.quoted && psKeywords[strings.ToLower(first.val)] {
		ps.keyword(strings.ToLower(first.val), toks, input)
		return
	}
	if first.kind == tWord && !first.quoted && strings.EqualFold(first.val, "foreach") && len(toks) > 1 && toks[1].kind == tParen {
		ps.foreach(toks)
		return
	}
	ps.pipeline(toks, input)
}

// keyword runs every script block of an if, while, try, switch and so on.
// Conditions in parentheses may run commands too.
func (ps *psState) keyword(kw string, toks []psTok, input *psInput) {
	for _, t := range toks[1:] {
		switch t.kind {
		case tBlock:
			c := ps.child()
			c.run(t.val, input)
			ps.actions = append(ps.actions, c.actions...)
		case tParen:
			if kw != "param" && kw != "function" {
				c := ps.child()
				c.run(t.val, input)
				ps.actions = append(ps.actions, c.actions...)
			}
		}
	}
}

// foreach ($f in <source>) { body }
func (ps *psState) foreach(toks []psTok) {
	head := toks[1].val
	in := &psInput{}
	name := ""
	if m := regexp.MustCompile(`(?is)^\s*\$(\w+)\s+in\s+(.*)$`).FindStringSubmatch(head); m != nil {
		name = strings.ToLower(m[1])
		src := strings.TrimSpace(m[2])
		if inner, ok := strings.CutPrefix(src, "$("); ok {
			src = strings.TrimSuffix(inner, ")")
		} else if inner, ok := strings.CutPrefix(src, "("); ok {
			src = strings.TrimSuffix(inner, ")")
		}
		in = ps.source(src)
	}
	for _, t := range toks[2:] {
		if t.kind == tBlock {
			c := ps.child()
			if name != "" {
				c.vars[name] = psItem
			}
			c.run(t.val, in)
			ps.actions = append(ps.actions, c.actions...)
		}
	}
}

// source evaluates a pipeline used only for its output, such as the list in
// foreach ($f in (Get-ChildItem *.log)), and returns the paths it yields.
func (ps *psState) source(src string) *psInput {
	c := ps.child()
	var out *psInput
	lx := &psLexer{src: []rune(src), vars: c.lookup}
	var toks []psTok
	for {
		t, ok := lx.next()
		if !ok {
			break
		}
		toks = append(toks, t)
	}
	for _, cmd := range splitPipeline(toks) {
		out = c.command(cmd, out, true)
	}
	ps.actions = append(ps.actions, c.actions...)
	if out == nil {
		return &psInput{}
	}
	return out
}

func splitPipeline(toks []psTok) [][]psTok {
	var cmds [][]psTok
	var cur []psTok
	for _, t := range toks {
		if t.kind == tOp && t.val == "|" {
			cmds = append(cmds, cur)
			cur = nil
			continue
		}
		cur = append(cur, t)
	}
	return append(cmds, cur)
}

func (ps *psState) pipeline(toks []psTok, input *psInput) {
	cmds := splitPipeline(toks)
	in := input
	for i, cmd := range cmds {
		if len(cmd) == 0 {
			continue
		}
		in = ps.command(cmd, in, i < len(cmds)-1)
	}
}

// evalPath computes the string a (...) subexpression yields when it is a
// plain value or a Join-Path, Resolve-Path or Convert-Path call.
func (ps *psState) evalPath(src string) (string, bool) {
	lx := &psLexer{src: []rune(src), vars: ps.lookup}
	var toks []psTok
	for {
		t, ok := lx.next()
		if !ok {
			break
		}
		if t.kind == tOp {
			return "", false
		}
		toks = append(toks, t)
	}
	if len(toks) == 1 && toks[0].kind == tWord && toks[0].quoted {
		return toks[0].val, toks[0].ok
	}
	if len(toks) == 0 || toks[0].kind != tWord {
		return "", false
	}
	switch strings.ToLower(toks[0].val) {
	case "join-path":
		a := bind(specJoin, toks[1:], ps.evalPath)
		parts := append(a.values("path", 0), a.values("childpath", 1)...)
		for _, extra := range a.positional[min(2, len(a.positional)):] {
			parts = append(parts, extra...)
		}
		if len(parts) < 2 {
			return "", false
		}
		out := ""
		for i, p := range parts {
			if !p.ok {
				return "", false
			}
			if i == 0 {
				out = p.s
			} else {
				out = strings.TrimRight(out, `\/`) + string(filepath.Separator) + strings.TrimLeft(p.s, `\/`)
			}
		}
		return out, true
	case "resolve-path", "rvpa", "convert-path", "cvpa":
		a := bind(specGetItem, toks[1:], ps.evalPath)
		if v := a.values("path", 0); len(v) == 1 && v[0].ok {
			if p := psResolve(v[0].s, ps.cwd); p != "" {
				return p, true
			}
		}
	}
	return "", false
}

// command classifies one pipeline element and returns what it passes on.
func (ps *psState) command(toks []psTok, input *psInput, piped bool) *psInput {
	var redirs []psTok
	var args []psTok
	for _, t := range toks {
		if t.kind == tRedir {
			redirs = append(redirs, t)
		} else {
			args = append(args, t)
		}
	}
	for _, r := range redirs {
		ps.redirect(r)
	}
	if len(args) == 0 {
		return nil
	}
	head := args[0]
	if head.kind == tOp && head.val == "&" || head.kind == tWord && head.raw == "." {
		args = args[1:] // call operator or dot sourcing
		if len(args) == 0 {
			return nil
		}
		head = args[0]
	}
	switch head.kind {
	case tBlock:
		c := ps.child()
		c.run(head.val, input)
		ps.actions = append(ps.actions, c.actions...)
		return nil
	case tParen:
		c := ps.child()
		c.run(head.val, input)
		ps.actions = append(ps.actions, c.actions...)
		return &psInput{}
	case tWord:
	default:
		return nil
	}
	ps.scanDotNet(toks)
	name := psName(head.val)
	rest := args[1:]
	switch name {
	case "remove-item":
		a := bind(specRemove, rest, ps.evalPath)
		ps.subexprs(a)
		if a.has("whatif") || a.has("stream") {
			return nil
		}
		ps.pathAction("Remove-Item", a.values("path", 0), a.named["literalpath"], input, true, "", a.unknown)
	case "move-item", "copy-item":
		sp := specMove
		if name == "copy-item" {
			sp = specCopy
		}
		a := bind(sp, rest, ps.evalPath)
		ps.subexprs(a)
		if a.has("whatif") || (name == "move-item" && !a.has("force")) {
			return nil
		}
		ps.copyMove(name, a, input)
	case "set-content", "clear-content":
		sp := specContent
		if name == "clear-content" {
			sp = specClear
		}
		a := bind(sp, rest, ps.evalPath)
		ps.subexprs(a)
		if a.has("whatif") {
			return nil
		}
		label := map[string]string{"set-content": "Set-Content", "clear-content": "Clear-Content"}[name]
		ps.pathAction(label, a.values("path", 0), a.named["literalpath"], input, false, "overwrites files", a.unknown)
	case "out-file", "tee-object":
		sp := specOutFile
		if name == "tee-object" {
			sp = specTee
		}
		a := bind(sp, rest, ps.evalPath)
		if a.has("append") || a.has("noclobber") || a.has("whatif") {
			return input
		}
		ps.pathAction(map[string]string{"out-file": "Out-File", "tee-object": "Tee-Object"}[name], a.values("filepath", 0), a.named["literalpath"], nil, false, "overwrites files", a.unknown)
		if name == "tee-object" {
			return input
		}
	case "new-item":
		a := bind(specNewItem, rest, ps.evalPath)
		if a.has("force") && !a.has("whatif") {
			paths := a.values("path", 0)
			if n := a.named["name"]; len(n) > 0 && len(paths) > 0 {
				paths = []psValue{{s: paths[0].s + string(filepath.Separator) + n[0].s, ok: paths[0].ok && n[0].ok, literal: true}}
			} else if len(n) > 0 {
				paths = n
			}
			ps.pathAction("New-Item", nil, paths, nil, false, "replaces existing files", false)
		}
	case "set-location", "push-location":
		a := bind(specLoc, rest, ps.evalPath)
		vals := a.values("path", 0)
		if lp := a.named["literalpath"]; len(lp) > 0 {
			vals = lp
		}
		if len(vals) == 0 {
			if h, err := os.UserHomeDir(); err == nil {
				ps.cwd = h
			}
		} else if vals[0].ok {
			if p := psResolve(vals[0].s, ps.cwd); p != "" && isDir(p) {
				ps.cwd = p
			}
		}
	case "get-childitem":
		return ps.childItems(bind(specGCI, rest, ps.evalPath))
	case "get-item", "resolve-path":
		a := bind(specGetItem, rest, ps.evalPath)
		var paths []string
		for _, v := range a.values("path", 0) {
			paths = append(paths, ps.expand(v)...)
		}
		for _, v := range a.named["literalpath"] {
			paths = append(paths, ps.expand(psValue{s: v.s, ok: v.ok, literal: true})...)
		}
		return &psInput{paths: paths, known: !a.unknown}
	case "foreach-object", "where-object":
		a := bind(specGeneric, rest, nil)
		for _, b := range a.blocks {
			c := ps.child()
			c.run(b, input)
			ps.actions = append(ps.actions, c.actions...)
		}
		if name == "where-object" {
			return input // a subset of the input, so the input is a safe superset
		}
		return &psInput{}
	case "select-object", "sort-object", "out-string":
		return input
	case "invoke-expression", "invoke-command":
		a := bind(specGeneric, rest, nil)
		for _, b := range a.blocks {
			c := ps.child()
			c.run(b, input)
			ps.actions = append(ps.actions, c.actions...)
		}
		for _, v := range a.positional {
			if len(v) > 0 && v[0].ok {
				c := ps.child()
				c.run(v[0].s, input)
				ps.actions = append(ps.actions, c.actions...)
			}
		}
	default:
		ps.native(name, rest)
	}
	if piped {
		return &psInput{}
	}
	return nil
}
