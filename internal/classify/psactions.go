package classify

import (
	"os"
	"path/filepath"
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
