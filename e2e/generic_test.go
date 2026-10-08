package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// project builds a small repository with source, generated output, a
// node_modules folder and a build folder.
func (e *env) project(t *testing.T) string {
	t.Helper()
	root := filepath.Join(e.work, "app")
	files := map[string]string{
		"src/main.c":                 "int main(void) { return 0; }\n",
		"src/util.c":                 "int util(void) { return 1; }\n",
		"Makefile":                   "all:\n\tmkdir -p obj && cp src/main.c obj/main.o && cp src/util.c obj/util.o\nclean:\n\trm -rf obj app.bin\n",
		"obj/main.o":                 "compiled main\n",
		"obj/util.o":                 "compiled util\n",
		"app.bin":                    "\x7fELF pretend binary\x00\x01\x02",
		"notes.md":                   "# notes\nuncommitted thoughts\n",
		"node_modules/left/index.js": "module.exports = 'left'\n",
		"node_modules/left/package.json": `{"name":"left","version":"1.0.0"}` + "\n",
		"node_modules/.bin/left":     "#!/bin/sh\necho left\n",
		"build/bundle.js":            "console.log('bundle')\n",
		"build/assets/logo.svg":      "<svg/>\n",
		"package.json": `{"name":"app","scripts":{"reset":"node scripts/reset.js","prereset":"node -e \"\""}}` + "\n",
		"scripts/reset.js": "const fs = require('fs');\nfor (const d of ['node_modules', 'build']) fs.rmSync(d, { recursive: true, force: true });\n",
		"tools/cleanup.py": "import os, shutil\nshutil.rmtree('obj')\nos.remove('notes.md')\nopen('src/main.c', 'w').write('int main(void) { return 42; }\\n')\n",
	}
	base := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	i := 0
	for name, data := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(data), 0o644)
		setTimes(p, base.Add(time.Duration(i)*time.Minute))
		i++
	}
	os.Chmod(filepath.Join(root, "node_modules", ".bin", "left"), 0o755)
	e.git("init", "-q", root)
	return root
}

func (e *env) inDir(dir string) *env {
	c := *e
	c.work = dir
	return &c
}

// generic runs cmd in dir between the pre and post hooks, like a shell.
func (e *env) generic(t *testing.T, dir, cmd string) string {
	t.Helper()
	d := e.inDir(dir)
	_, pre, _ := d.oopsCmd("", "hook", "zsh", "--", cmd)
	c := exec.Command(posixShell(), "-c", cmd)
	c.Dir = dir
	c.Env = e.environ()
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", cmd, err, out)
	}
	_, post, _ := d.oopsCmd("", "hook", "zsh-post")
	return pre + post
}

func needTool(t *testing.T, name string) {
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s is not installed", name)
	}
}

func TestGenericMakeClean(t *testing.T) {
	needTool(t, "make")
	e := newEnv(t)
	root := e.project(t)
	e.mustOops("mirror", "build", root)
	before := snapshotTree(t, root, false)
	delete(before, ".git")
	msg := e.generic(t, root, "make clean")
	if _, err := os.Stat(filepath.Join(root, "obj")); err == nil {
		t.Fatal("make clean did nothing")
	}
	if !strings.Contains(msg, "saved snapshot") {
		t.Fatalf("no snapshot reported: %q", msg)
	}
	out := e.inDir(root).mustOops("-y")
	if !strings.Contains(out, "make clean") {
		t.Fatalf("undid the wrong thing: %s", out)
	}
	compareTrees(t, without(before, ".git"), without(snapshotTree(t, root, false), ".git"))
}

func TestGenericNpmRunReset(t *testing.T) {
	needTool(t, "npm")
	e := newEnv(t)
	root := e.project(t)
	e.mustOops("mirror", "build", root)
	before := without(snapshotTree(t, root, false), ".git")
	e.generic(t, root, "npm run reset")
	for _, d := range []string{"node_modules", "build"} {
		if _, err := os.Stat(filepath.Join(root, d)); err == nil {
			t.Fatalf("npm run reset left %s", d)
		}
	}
	e.inDir(root).mustOops("-y")
	compareTrees(t, before, without(snapshotTree(t, root, false), ".git"))
}

func TestGenericUnknownScript(t *testing.T) {
	py := "python3"
	if _, err := exec.LookPath(py); err != nil {
		py = "python"
		needTool(t, py)
	}
	e := newEnv(t)
	root := e.project(t)
	e.mustOops("mirror", "build", root)
	before := without(snapshotTree(t, root, false), ".git")
	// Run it through a wrapper name so nothing about it is recognizable.
	e.generic(t, root, py+" tools/cleanup.py")
	if _, err := os.Stat(filepath.Join(root, "notes.md")); err == nil {
		t.Fatal("the script did not run")
	}
	out := e.inDir(root).mustOops("-y")
	if strings.Contains(out, "not saved") {
		t.Logf("partial: %s", out)
	}
	got := without(snapshotTree(t, root, false), ".git")
	main := filepath.Join("src", "main.c")
	if inPlaceLost(e) {
		// Hard link mirrors cannot keep a file rewritten in place; that is
		// covered by the preload library and by watch mode instead.
		delete(before, main)
		delete(got, main)
	}
	compareTrees(t, before, got)
}

// inPlaceLost reports a mirror that uses hard links (NTFS, ext4).
func inPlaceLost(e *env) bool {
	out, _, _ := e.oopsCmd("", "mirror")
	return strings.Contains(out, "hardlink")
}

func without(m map[string]node, prefix string) map[string]node {
	out := map[string]node{}
	for k, v := range m {
		if k == prefix || strings.HasPrefix(k, prefix+string(filepath.Separator)) {
			continue
		}
		out[k] = v
	}
	return out
}
