package classify

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// maxListed caps how many paths a wildcard or Get-ChildItem may expand to
// before Oopsy saves the containing folders instead.
const maxListed = 20000

func hasWild(s string) bool { return strings.ContainsAny(s, "*?[") }

// wildMatch matches a single path element against a wildcard pattern,
// ignoring case.
func wildMatch(pattern, name string) bool {
	ok, err := filepath.Match(strings.ToLower(pattern), strings.ToLower(name))
	return err == nil && ok
}

// psGlob expands an absolute pattern one path element at a time.
func psGlob(pattern string) []string {
	vol := filepath.VolumeName(pattern)
	rest := strings.TrimPrefix(pattern[len(vol):], string(filepath.Separator))
	cur := []string{vol + string(filepath.Separator)}
	for _, seg := range strings.Split(rest, string(filepath.Separator)) {
		if seg == "" {
			continue
		}
		var next []string
		for _, dir := range cur {
			if !hasWild(seg) {
				if p := filepath.Join(dir, seg); exists(p) {
					next = append(next, p)
				}
				continue
			}
			ents, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range ents {
				if wildMatch(seg, e.Name()) {
					next = append(next, filepath.Join(dir, e.Name()))
				}
			}
		}
		if len(next) > maxListed {
			return nil
		}
		cur = next
	}
	return cur
}

func listMatching(root string, recurse bool, filters, excludes []string, onlyFiles, onlyDirs bool) ([]string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var out []string
	seen := 0
	ok := true
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return nil
		}
		seen++
		if seen > maxListed || ctx.Err() != nil {
			ok = false
			return filepath.SkipAll
		}
		name := d.Name()
		match := len(filters) == 0
		for _, f := range filters {
			match = match || wildMatch(f, name)
		}
		for _, x := range excludes {
			match = match && !wildMatch(x, name)
		}
		if match && (!onlyFiles || !d.IsDir()) && (!onlyDirs || d.IsDir()) {
			out = append(out, p)
		}
		if d.IsDir() && !recurse {
			return filepath.SkipDir
		}
		return nil
	})
	return out, ok
}

// psResolve turns a PowerShell path into an absolute filesystem path, or ""
// for other providers (Env:, HKLM:, Variable:, Cert: and so on).
func psResolve(p, cwd string) string {
	if p == "" {
		return ""
	}
	if i := strings.Index(p, "::"); i >= 0 && strings.Contains(strings.ToLower(p[:i]), "filesystem") {
		p = p[i+2:]
	}
	if runtime.GOOS != "windows" {
		p = strings.ReplaceAll(p, `\`, "/")
	}
	if m := regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_]*):(.*)$`).FindStringSubmatch(p); m != nil {
		drive, rest := m[1], m[2]
		switch {
		case len(drive) == 1 && runtime.GOOS == "windows":
			if rest == "" || !os.IsPathSeparator(rest[0]) {
				// C: alone or C:foo is relative to that drive's current folder.
				if strings.EqualFold(filepath.VolumeName(cwd), drive+":") {
					return filepath.Clean(filepath.Join(cwd, rest))
				}
				return filepath.Clean(strings.ToUpper(drive) + `:\` + rest)
			}
			return filepath.Clean(p)
		case strings.EqualFold(drive, "temp"):
			return filepath.Join(os.TempDir(), rest)
		default:
			return "" // not the filesystem
		}
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if h, err := os.UserHomeDir(); err == nil {
			p = h + p[1:]
		}
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if runtime.GOOS == "windows" && os.IsPathSeparator(p[0]) {
		return filepath.Clean(filepath.VolumeName(cwd) + p)
	}
	return filepath.Join(cwd, p)
}
