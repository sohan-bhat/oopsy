package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sohan-bhat/oopsy/internal/classify"
	"github.com/sohan-bhat/oopsy/internal/snap"
)

// Rebuildable folders (node_modules, target, build and friends) are not in
// the project mirror. A command gets them saved whole, right before it runs,
// when it is about to touch them: it names them, or a script it runs does.
// Oopsy reads npm, yarn, pnpm and bun scripts from package.json, asks make
// for its plan with make -n, and reads script files the command runs.

var rebuildable = []string{"node_modules", "target", ".venv", "venv", "dist", "build", ".next", "__pycache__", ".gradle"}

// cleanTargets are tools whose clean step removes known folders without
// naming them.
var cleanTargets = map[string][]string{
	"cargo":   {"target"},
	"mvn":     {"target"},
	"gradle":  {"build", ".gradle"},
	"gradlew": {"build", ".gradle"},
}

// lazyTargets returns the rebuildable folders the command may touch.
func lazyTargets(cmd, cwd, root string) []string {
	cands := candidates(cwd, root)
	if len(cands) == 0 {
		return nil
	}
	words := classify.Words(cmd, cwd)
	texts := []string{cmd}
	texts = append(texts, scriptTexts(words, cwd, root)...)
	for i, w := range words {
		if tg, ok := cleanTargets[filepath.Base(w)]; ok && i+1 < len(words) && contains(words[i+1:], "clean") {
			texts = append(texts, strings.Join(tg, " "))
		}
	}
	all := strings.Join(texts, "\n")
	var out []string
	for _, c := range cands {
		name := filepath.Base(c)
		re := regexp.MustCompile(`(^|[^A-Za-z0-9_.-])` + regexp.QuoteMeta(name) + `($|[^A-Za-z0-9_-])`)
		if re.MatchString(all) {
			out = append(out, c)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// candidates lists rebuildable folders in cwd, the project root and the
// folders between them.
func candidates(cwd, root string) []string {
	seen := map[string]bool{}
	var out []string
	dirs := []string{cwd}
	if root != "" {
		for d := cwd; d != root && len(dirs) < 20; {
			parent := filepath.Dir(d)
			if parent == d {
				break
			}
			d = parent
			dirs = append(dirs, d)
		}
	}
	for _, d := range dirs {
		for _, name := range rebuildable {
			p := filepath.Join(d, name)
			if fi, err := os.Lstat(p); err == nil && fi.IsDir() && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// scriptTexts returns the text of what the command will really run:
// package.json scripts, make's plan and script files.
func scriptTexts(words []string, cwd, root string) []string {
	if len(words) == 0 {
		return nil
	}
	var out []string
	prog := filepath.Base(words[0])
	switch prog {
	case "npm", "yarn", "pnpm", "bun":
		if s := packageScripts(words[1:], cwd, root); s != "" {
			out = append(out, s)
		}
	case "make", "gmake":
		out = append(out, makePlan(words, cwd))
	}
	// Script files the command runs, and files those scripts run in turn.
	seen := map[string]bool{}
	todo := append([]string{}, words...)
	for _, t := range out {
		todo = append(todo, classify.Words(t, cwd)...)
	}
	for _, w := range todo {
		p := classify.AbsWord(w, cwd, home())
		if seen[p] {
			continue
		}
		seen[p] = true
		if text := scriptText(p); text != "" {
			out = append(out, text)
		}
	}
	return out
}

// scriptText reads a small text file that looks like a script.
func scriptText(p string) string {
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 1<<20 {
		return ""
	}
	switch strings.ToLower(filepath.Ext(p)) {
	case ".sh", ".bash", ".zsh", ".py", ".js", ".mjs", ".cjs", ".ts", ".rb", ".pl", ".ps1", ".bat", ".cmd", "":
	default:
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil || bytes.Contains(b[:min(len(b), 8000)], []byte{0}) {
		return ""
	}
	return string(b)
}

// packageScripts expands npm run style invocations, following scripts that
// call other scripts.
func packageScripts(args []string, cwd, root string) string {
	pkg := findUp(cwd, root, "package.json")
	if pkg == "" {
		return ""
	}
	b, err := os.ReadFile(pkg)
	if err != nil {
		return ""
	}
	var p struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(b, &p) != nil || len(p.Scripts) == 0 {
		return ""
	}
	name := ""
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if a == "run" || a == "run-script" {
			if i+1 < len(args) {
				name = args[i+1]
			}
			break
		}
		name = a // yarn reset, npm test, pnpm build
		break
	}
	var out []string
	var expand func(n string, depth int)
	seen := map[string]bool{}
	runRe := regexp.MustCompile(`(?:npm|yarn|pnpm|bun)\s+(?:run\s+)?([A-Za-z0-9:_.-]+)`)
	expand = func(n string, depth int) {
		if depth > 4 || seen[n] {
			return
		}
		seen[n] = true
		for _, k := range []string{"pre" + n, n, "post" + n} {
			s, ok := p.Scripts[k]
			if !ok {
				continue
			}
			out = append(out, s)
			for _, m := range runRe.FindAllStringSubmatch(s, -1) {
				expand(m[1], depth+1)
			}
		}
	}
	if name != "" {
		expand(name, 0)
	}
	return strings.Join(out, "\n")
}

func findUp(dir, root, name string) string {
	for {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
		if dir == root || root == "" {
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// makePlan asks make what it would run, without running it.
func makePlan(words []string, cwd string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	args := append([]string{"-n"}, words[1:]...)
	c := exec.CommandContext(ctx, words[0], args...)
	c.Dir = cwd
	out, _ := c.Output()
	return string(out)
}

// lazyActions turns targets into snapshot actions.
func lazyActions(cmd, cwd string) []classify.Action {
	root := snap.MirrorRoot(cwd)
	targets := lazyTargets(cmd, cwd, root)
	if len(targets) == 0 {
		return nil
	}
	return []classify.Action{{Program: "rebuildable", Paths: targets, Delete: true, Rebuildable: true}}
}
