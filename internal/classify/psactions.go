package classify

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// subexprs classifies commands inside (...) arguments, which run first.
func (ps *psState) subexprs(a psArgs) {
	for _, p := range a.parens {
		c := ps.child()
		c.run(p, nil)
		ps.actions = append(ps.actions, c.actions...)
	}
}

// pathAction records the paths a cmdlet will remove or overwrite. Values
// equal to $_ take the pipeline input; with no path at all, the pipeline
// input is the target (Get-ChildItem *.log | Remove-Item).
func (ps *psState) pathAction(program string, vals, literal []psValue, input *psInput, del bool, note string, unresolved bool) {
	var paths []string
	broad := false
	all := append([]psValue{}, vals...)
	for _, v := range literal {
		v.literal = true
		all = append(all, v)
	}
	usesInput := len(all) == 0 && input != nil
	for _, v := range all {
		if strings.Contains(v.s, psItem) {
			usesInput = true
			continue
		}
		if !v.ok {
			broad = true
			continue
		}
		paths = append(paths, ps.expand(v)...)
	}
	if usesInput {
		if input != nil && input.known {
			paths = append(paths, input.paths...)
		} else {
			broad = true
		}
	}
	if !del {
		// Overwrites only matter for files that exist and have content.
		var kept []string
		for _, p := range paths {
			if fi, err := os.Stat(p); err == nil && (!fi.Mode().IsRegular() || fi.Size() > 0) {
				kept = append(kept, p)
			}
		}
		paths = kept
	}
	if len(paths) > 0 {
		ps.actions = append(ps.actions, Action{Program: program, Paths: paths, Delete: del, Note: note})
	}
	if broad || (unresolved && len(paths) == 0) {
		ps.actions = append(ps.actions, Action{Program: program, Paths: []string{ps.cwd}, Broad: true, Delete: del,
			Note: "some targets are only known when the command runs, so the current folder is saved"})
	}
}

func (ps *psState) copyMove(name string, a psArgs, input *psInput) {
	program := map[string]string{"move-item": "Move-Item", "copy-item": "Copy-Item"}[name]
	dest := a.named["destination"]
	srcVals := a.named["path"]
	if len(srcVals) == 0 && len(a.positional) > 0 && len(a.named["literalpath"]) == 0 {
		srcVals = a.positional[0]
	}
	if len(dest) == 0 {
		pos := 1
		if len(a.named["path"]) > 0 || len(a.named["literalpath"]) > 0 {
			pos = 0
		}
		if pos < len(a.positional) {
			dest = a.positional[pos]
		}
	}
	if len(dest) == 0 {
		dest = []psValue{{s: ps.cwd, ok: true, literal: true}}
	}
	if !dest[0].ok {
		ps.actions = append(ps.actions, Action{Program: program, Paths: []string{ps.cwd}, Broad: true, Note: "destination is only known when the command runs"})
		return
	}
	var srcs []string
	for _, v := range append(srcVals, a.named["literalpath"]...) {
		if strings.Contains(v.s, psItem) && input != nil && input.known {
			srcs = append(srcs, input.paths...)
		} else if v.ok {
			srcs = append(srcs, psResolve(v.s, ps.cwd))
		}
	}
	if len(srcVals)+len(a.named["literalpath"]) == 0 && input != nil && input.known {
		srcs = input.paths
	}
	d := psResolve(dest[0].s, ps.cwd)
	if d == "" {
		return
	}
	var paths []string
	if isDir(d) {
		for _, src := range srcs {
			if t := filepath.Join(d, filepath.Base(src)); exists(t) {
				paths = append(paths, t)
			}
		}
		if len(srcs) == 0 {
			paths = append(paths, d) // sources unknown: keep the whole destination
		}
	} else if exists(d) {
		paths = append(paths, d)
	}
	if len(paths) > 0 {
		ps.actions = append(ps.actions, Action{Program: program, Paths: paths, Note: "overwrites existing files"})
	}
}

func (ps *psState) redirect(r psTok) {
	if r.appendRedir || r.val == "" {
		return
	}
	if !r.ok {
		return
	}
	p := psResolve(r.val, ps.cwd)
	if p == "" {
		return
	}
	if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
		ps.actions = append(ps.actions, Action{Program: ">", Paths: []string{p}, Note: "redirect overwrites file"})
	}
}

var dotNetFile = regexp.MustCompile(`(?i)\[(?:system\.)?io\.(file|directory)\]::(delete|writealltext|writeallbytes|writealllines|copy|move|replace)\(\s*(?:'([^']*)'|"([^"$` + "`" + `]*)")\s*(?:,\s*(?:'([^']*)'|"([^"$` + "`" + `]*)"))?`)

// scanDotNet finds [IO.File]::Delete('x') style calls with literal paths.
func (ps *psState) scanDotNet(toks []psTok) {
	for _, t := range toks {
		if t.kind != tWord && t.kind != tParen {
			continue
		}
		text := t.raw
		if t.kind == tParen {
			text = t.val
		}
		for _, m := range dotNetFile.FindAllStringSubmatch(text, -1) {
			op := strings.ToLower(m[2])
			target := m[3] + m[4]
			if op == "copy" || op == "move" || op == "replace" {
				target = m[5] + m[6] // the destination is what gets overwritten
			}
			if p := psResolve(target, ps.cwd); p != "" && exists(p) {
				ps.actions = append(ps.actions, Action{Program: "[IO." + m[1] + "]::" + m[2], Paths: []string{p}, Delete: op == "delete"})
			}
		}
	}
}

// expand resolves a path value and expands PowerShell wildcards (* ? [ab]),
// which match case insensitively and include hidden files.
func (ps *psState) expand(v psValue) []string {
	p := psResolve(v.s, ps.cwd)
	if p == "" {
		return nil
	}
	if v.literal || !strings.ContainsAny(v.s, "*?[") {
		return []string{p}
	}
	out := psGlob(p)
	if exists(p) {
		out = append(out, p) // a literal name with brackets in it
	}
	return out
}

// childItems lists what Get-ChildItem would emit, for piping into
// Remove-Item and friends. Without filters the direct children are enough,
// since deleting them deletes everything below too.
func (ps *psState) childItems(a psArgs) *psInput {
	if a.unknown {
		return &psInput{}
	}
	var roots []string
	for _, v := range a.values("path", 0) {
		roots = append(roots, ps.expand(v)...)
	}
	for _, v := range a.named["literalpath"] {
		roots = append(roots, ps.expand(psValue{s: v.s, ok: true, literal: true})...)
	}
	if len(a.values("path", 0))+len(a.named["literalpath"]) == 0 {
		roots = []string{ps.cwd}
	}
	var filters, excludes []string
	if f := a.values("filter", 1); len(f) > 0 {
		filters = append(filters, f[0].s)
	}
	for _, v := range a.named["include"] {
		filters = append(filters, v.s)
	}
	for _, v := range a.named["exclude"] {
		excludes = append(excludes, v.s)
	}
	onlyFiles, onlyDirs := a.has("file"), a.has("directory")
	recurse := a.has("recurse") || len(a.named["depth"]) > 0
	plain := len(filters) == 0 && len(excludes) == 0 && !onlyFiles && !onlyDirs

	var out []string
	for _, root := range roots {
		fi, err := os.Lstat(root)
		if err != nil {
			continue
		}
		if !fi.IsDir() {
			out = append(out, root)
			continue
		}
		if plain {
			ents, _ := os.ReadDir(root)
			for _, e := range ents {
				out = append(out, filepath.Join(root, e.Name()))
			}
			continue
		}
		matched, ok := listMatching(root, recurse, filters, excludes, onlyFiles, onlyDirs)
		if !ok {
			out = append(out, root) // too many to list: save the whole root
			continue
		}
		out = append(out, matched...)
	}
	return &psInput{paths: out, known: true}
}
