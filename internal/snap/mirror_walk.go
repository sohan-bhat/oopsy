package snap

import (
	"bufio"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// node is one entry of a project as the mirror last saw it.
type node struct {
	rel   string // slash separated, relative to the project root
	kind  byte   // 'f' file, 'd' folder, 'l' link, 'r' rebuildable folder (not descended)
	size  int64
	mtime int64
	mode  uint32 // permission bits on Unix, file attributes on Windows
}

func (n node) stamp() Stamp { return Stamp{Size: n.size, Mtime: n.mtime} }

// Rebuildable folders are not mirrored file by file. They are saved whole,
// only when a command is about to touch them, and kept for one day.
var rebuildableNames = map[string]bool{
	"node_modules": true, "target": true, ".venv": true, "venv": true, "dist": true, "build": true,
	".next": true, "__pycache__": true, ".gradle": true,
}

// ignoreRules are patterns from a project's .oopsignore: a bare name matches
// at any depth, a pattern with a slash matches from the root.
type ignoreRules struct {
	names []string
	paths []string
}

func loadIgnore(root string) ignoreRules {
	var r ignoreRules
	f, err := os.Open(filepath.Join(root, ".oopsignore"))
	if err != nil {
		return r
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSuffix(line, "/")
		if strings.Contains(strings.TrimPrefix(line, "/"), "/") || strings.HasPrefix(line, "/") {
			r.paths = append(r.paths, strings.TrimPrefix(line, "/"))
		} else {
			r.names = append(r.names, line)
		}
	}
	return r
}

func (r ignoreRules) match(rel, name string) bool {
	for _, p := range r.names {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	for _, p := range r.paths {
		if ok, _ := path.Match(p, rel); ok {
			return true
		}
	}
	return false
}

// walkResult is a full stat walk of a project.
type walkResult struct {
	nodes []node // sorted by rel
	files int
}

// errTooBig stops a walk that passes the file limit.
type errTooBig struct{ limit int }

func (e errTooBig) Error() string { return "too many files" }

// walkProject stats everything under root in parallel. Rebuildable and
// ignored folders become a single 'r' node, the store itself is skipped.
func walkProject(root string, limit int) (walkResult, error) {
	ign := loadIgnore(root)
	// Oopsy's own stores are skipped wherever they are inside the project.
	skip := map[string]bool{pathKey(filepath.Clean(Dir())): true, pathKey(filepath.Clean(StoreDir())): true}
	if vr := volumeRoot(root); vr != "" {
		if top := volumeTop(vr); top != "" {
			skip[pathKey(filepath.Clean(top))] = true
		}
	}
	var (
		mu    sync.Mutex
		out   []node
		wg    sync.WaitGroup
		count int
		over  bool
	)
	sem := make(chan struct{}, 64)
	var walk func(dir, rel string)
	walk = func(dir, rel string) {
		defer wg.Done()
		sem <- struct{}{}
		ents, err := listDir(dir)
		if err != nil {
			<-sem
			return
		}
		local := make([]node, 0, len(ents))
		for _, e := range ents {
			name := e.name
			r := name
			if rel != "" {
				r = rel + "/" + name
			}
			n := node{rel: r, mtime: e.mtime, mode: e.mode}
			switch e.kind {
			case 'd':
				full := filepath.Join(dir, name)
				if skip[pathKey(full)] {
					continue
				}
				if rebuildableNames[name] || ign.match(r, name) {
					n.kind = 'r'
					local = append(local, n)
					continue
				}
				n.kind = 'd'
				local = append(local, n)
				wg.Add(1)
				go walk(full, r)
			case 'f':
				if ign.match(r, name) {
					continue
				}
				n.kind, n.size = 'f', e.size
				local = append(local, n)
			case 'l':
				n.kind = 'l'
				local = append(local, n)
			}
		}
		<-sem
		mu.Lock()
		out = append(out, local...)
		count += len(local)
		if limit > 0 && count > limit {
			over = true
		}
		mu.Unlock()
	}
	wg.Add(1)
	go walk(root, "")
	wg.Wait()
	if over {
		return walkResult{}, errTooBig{limit}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	files := 0
	for _, n := range out {
		if n.kind == 'f' {
			files++
		}
	}
	return walkResult{nodes: out, files: files}, nil
}

// dirEntry is what a walk needs to know about one entry.
type dirEntry struct {
	name  string
	kind  byte // 'f', 'd', 'l', or 0 for anything else
	size  int64
	mtime int64
	mode  uint32
}

// relParent returns the parent of a slash separated relative path, "" for
// top level entries.
func relParent(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return ""
}
