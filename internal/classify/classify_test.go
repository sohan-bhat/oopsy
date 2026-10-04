package classify

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fixture builds a directory tree the commands under test run in.
func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dir, _ = filepath.EvalSymlinks(dir)
	files := []string{"a.txt", "b.txt", "c.log", "d.log", ".env", "space file.txt", "dir1/x", "dir1/y", "dir1/.hidden",
		"sub/deeper/file.go", "src/main.go", "dst/main.go", "notes.md", "it's.txt"}
	for _, f := range files {
		p := filepath.Join(dir, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("content of "+f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Join(dir, "emptydir"), 0o755)
	os.WriteFile(filepath.Join(dir, "empty.txt"), nil, 0o644)
	os.Symlink("a.txt", filepath.Join(dir, "link"))
	os.Symlink("dir1", filepath.Join(dir, "dirlink"))
	return dir
}

type tc struct {
	cmd   string
	want  bool
	paths []string // expected paths relative to the fixture (subset check)
	git   string   // expected git op kind
	broad bool
}

func check(t *testing.T, dir string, env []string, c tc) {
	t.Helper()
	res := Classify(c.cmd, dir, env)
	if res.Destructive() != c.want {
		t.Errorf("%q: destructive=%v, want %v (actions %+v)", c.cmd, res.Destructive(), c.want, res.Actions)
		return
	}
	got := map[string]bool{}
	gitKinds := map[string]bool{}
	broad := false
	for _, a := range res.Actions {
		for _, p := range a.Paths {
			rel, err := filepath.Rel(dir, p)
			if err != nil || strings.HasPrefix(rel, "..") {
				rel = p
			}
			got[rel] = true
		}
		if a.Git != nil {
			gitKinds[a.Git.Kind] = true
		}
		broad = broad || a.Broad
	}
	for _, p := range c.paths {
		if !got[p] {
			keys := make([]string, 0, len(got))
			for k := range got {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			t.Errorf("%q: missing path %q (got %v)", c.cmd, p, keys)
		}
	}
	if c.git != "" && !gitKinds[c.git] {
		t.Errorf("%q: missing git op %q (got %v)", c.cmd, c.git, gitKinds)
	}
	if c.broad && !broad {
		t.Errorf("%q: expected a broad snapshot", c.cmd)
	}
}

func baseEnv(dir string) []string {
	home, _ := os.UserHomeDir()
	return []string{"HOME=" + home, "PATH=/usr/bin:/bin", "BUILD=dir1", "EMPTY=", "TARGET=" + filepath.Join(dir, "b.txt")}
}

func TestRmForms(t *testing.T) {
	dir := fixture(t)
	env := baseEnv(dir)
	cases := []tc{
		{cmd: "rm a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "rm -f a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "rm -rf dir1", want: true, paths: []string{"dir1"}},
		{cmd: "rm -r dir1", want: true, paths: []string{"dir1"}},
		{cmd: "rm -R dir1", want: true, paths: []string{"dir1"}},
		{cmd: "rm -fr dir1", want: true, paths: []string{"dir1"}},
		{cmd: "rm -r -f dir1", want: true, paths: []string{"dir1"}},
		{cmd: "rm --recursive --force dir1", want: true, paths: []string{"dir1"}},
		{cmd: "rm -rfv dir1 a.txt", want: true, paths: []string{"dir1", "a.txt"}},
		{cmd: "rm dir1/x dir1/y", want: true, paths: []string{"dir1/x", "dir1/y"}},
		{cmd: "rm -- -weird a.txt", want: true, paths: []string{"a.txt", "-weird"}},
		{cmd: "rm *.log", want: true, paths: []string{"c.log", "d.log"}},
		{cmd: "rm -f *.txt", want: true, paths: []string{"a.txt", "b.txt", "space file.txt", "empty.txt", "it's.txt"}},
		{cmd: "rm -rf *", want: true, paths: []string{"a.txt", "dir1", "sub", "src"}},
		{cmd: "rm -rf ./*", want: true, paths: []string{"a.txt", "dir1"}},
		{cmd: "rm -rf .[!.]*", want: true, paths: []string{".env"}},
		{cmd: "rm -rf dir1/*", want: true, paths: []string{"dir1/x", "dir1/y"}},
		{cmd: "rm -rf dir1/.*", want: true, paths: []string{"dir1/.hidden"}},
		{cmd: "rm -rf {a,b}.txt", want: true, paths: []string{"a.txt", "b.txt"}},
		{cmd: "rm -rf dir{1,2}", want: true, paths: []string{"dir1"}},
		{cmd: "rm '?.txt'", want: true, paths: []string{"?.txt"}},
		{cmd: `rm "space file.txt"`, want: true, paths: []string{"space file.txt"}},
		{cmd: `rm space\ file.txt`, want: true, paths: []string{"space file.txt"}},
		{cmd: `rm 'space file.txt'`, want: true, paths: []string{"space file.txt"}},
		{cmd: `rm "it's.txt"`, want: true, paths: []string{"it's.txt"}},
		{cmd: `rm it\'s.txt`, want: true, paths: []string{"it's.txt"}},
		{cmd: "rm -rf $BUILD", want: true, paths: []string{"dir1"}},
		{cmd: "rm -rf ${BUILD}/x", want: true, paths: []string{"dir1/x"}},
		{cmd: `rm -rf "$BUILD"`, want: true, paths: []string{"dir1"}},
		{cmd: "rm $TARGET", want: true, paths: []string{"b.txt"}},
		{cmd: "X=dir1; rm -rf $X", want: true, paths: []string{"dir1"}},
		{cmd: "X=sub rm -rf $X", want: true},
		{cmd: "export D=dir1 && rm -rf $D/y", want: true, paths: []string{"dir1/y"}},
		{cmd: "rm -rf $UNSET_THING/build", want: true, broad: true},
		{cmd: "rm -rf $(cat list.txt)", want: true, broad: true},
		{cmd: "rm -rf `cat list.txt`", want: true, broad: true},
		{cmd: "rm " + filepath.Join(dir, "a.txt"), want: true, paths: []string{"a.txt"}},
		{cmd: "rm ../" + filepath.Base(dir) + "/a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "rm ./dir1/../a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "rm link", want: true, paths: []string{"link"}},
		{cmd: "rm -rf dirlink/", want: true, paths: []string{"dirlink"}},
		{cmd: "rm dir1", want: false},
		{cmd: "rm -d emptydir", want: true, paths: []string{"emptydir"}},
		{cmd: "rm -i a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "/bin/rm a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "/usr/bin/env rm a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: `\rm a.txt`, want: true, paths: []string{"a.txt"}},
		{cmd: "grm -rf dir1", want: true, paths: []string{"dir1"}},
		{cmd: "srm a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "rm -rf ~/definitely-not-here-oopsy", want: true},
		{cmd: "rm notexist", want: true, paths: []string{"notexist"}},
		{cmd: "rm", want: false},
		{cmd: "rm -rf", want: false},
	}
	for _, c := range cases {
		check(t, dir, env, c)
	}
}

func TestWrappersAndChains(t *testing.T) {
	dir := fixture(t)
	env := baseEnv(dir)
	base := []struct {
		cmd  string
		path string
	}{
		{"rm -rf dir1", "dir1"},
		{"rm a.txt", "a.txt"},
		{"mv b.txt a.txt", "a.txt"},
		{"cp b.txt a.txt", "a.txt"},
		{"truncate -s 0 a.txt", "a.txt"},
		{"shred -u a.txt", "a.txt"},
		{"unlink a.txt", "a.txt"},
		{"sed -i 's/a/b/' a.txt", "a.txt"},
		{"echo hi > a.txt", "a.txt"},
		{"dd if=/dev/zero of=a.txt count=1", "a.txt"},
	}
	wrappers := []string{
		"%s",
		"sudo %s",
		"sudo -u root %s",
		"sudo -E %s",
		"sudo -- %s",
		"env FOO=1 %s",
		"env -i %s",
		"command %s",
		"nohup %s",
		"time %s",
		"nice -n 10 %s",
		"timeout 5 %s",
		"exec %s",
		"doas %s",
		"true && %s",
		"false || %s",
		"ls; %s",
		"%s; ls",
		"%s && echo done",
		"(%s)",
		"{ %s; }",
		"if true; then %s; fi",
		"if false; then :; else %s; fi",
		"while false; do %s; done",
		"echo $(%s)",
		"echo `%s`",
		"cat <(%s)",
		"%s | cat",
		"echo x | %s",
		"%s &",
		"! %s",
		"f() { %s; }",
		"case x in x) %s;; esac",
		"for i in 1 2; do %s; done",
	}
	for _, b := range base {
		for _, w := range wrappers {
			cmd := strings.ReplaceAll(w, "%s", b.cmd)
			check(t, dir, env, tc{cmd: cmd, want: true, paths: []string{b.path}})
		}
		for _, sh := range []string{"bash -c %q", "sh -c %q", "zsh -c %q", "bash -lc %q", "eval %q", "sudo sh -c %q"} {
			cmd := strings.Replace(sh, "%q", "'"+strings.ReplaceAll(b.cmd, "'", `'\''`)+"'", 1)
			check(t, dir, env, tc{cmd: cmd, want: true, paths: []string{b.path}})
		}
	}
}

func TestCdTracking(t *testing.T) {
	dir := fixture(t)
	env := baseEnv(dir)
	for _, c := range []tc{
		{cmd: "cd dir1 && rm x", want: true, paths: []string{"dir1/x"}},
		{cmd: "cd dir1; rm -f *", want: true, paths: []string{"dir1/x", "dir1/y"}},
		{cmd: "cd sub/deeper && rm file.go", want: true, paths: []string{"sub/deeper/file.go"}},
		{cmd: "(cd dir1 && rm x); rm a.txt", want: true, paths: []string{"dir1/x", "a.txt"}},
		{cmd: "pushd dir1 && rm y", want: true, paths: []string{"dir1/y"}},
		{cmd: "git -C sub status", want: false},
		{cmd: "env -C dir1 rm x", want: true, paths: []string{"dir1/x"}},
	} {
		check(t, dir, env, c)
	}
}

func TestMvCpLn(t *testing.T) {
	dir := fixture(t)
	env := baseEnv(dir)
	for _, c := range []tc{
		{cmd: "mv a.txt b.txt", want: true, paths: []string{"b.txt"}},
		{cmd: "mv a.txt new.txt", want: false},
		{cmd: "mv -n a.txt b.txt", want: false},
		{cmd: "mv --no-clobber a.txt b.txt", want: false},
		{cmd: "mv -f a.txt b.txt", want: true, paths: []string{"b.txt"}},
		{cmd: "mv src/main.go dst", want: true, paths: []string{"dst/main.go"}},
		{cmd: "mv src/main.go dst/", want: true, paths: []string{"dst/main.go"}},
		{cmd: "mv -t dst src/main.go", want: true, paths: []string{"dst/main.go"}},
		{cmd: "mv --target-directory=dst src/main.go", want: true, paths: []string{"dst/main.go"}},
		{cmd: "mv a.txt dir1", want: false},
		{cmd: "mv a.txt b.txt c.log dir1", want: false},
		{cmd: "mv -T src dst", want: true, paths: []string{"dst"}},
		{cmd: "cp a.txt b.txt", want: true, paths: []string{"b.txt"}},
		{cmd: "cp a.txt fresh.txt", want: false},
		{cmd: "cp -n a.txt b.txt", want: false},
		{cmd: "cp -r src dst", want: false},
		{cmd: "cp -r src/ dst", want: true, paths: []string{"dst"}},
		{cmd: "cp src/main.go dst/", want: true, paths: []string{"dst/main.go"}},
		{cmd: "cp -a src/main.go dst/main.go", want: true, paths: []string{"dst/main.go"}},
		{cmd: "cp --update=none a.txt b.txt", want: false},
		{cmd: "gcp a.txt b.txt", want: true, paths: []string{"b.txt"}},
		{cmd: "install -m 644 a.txt b.txt", want: true, paths: []string{"b.txt"}},
		{cmd: "install -d newdir", want: false},
		{cmd: "ln -sf a.txt b.txt", want: true, paths: []string{"b.txt"}},
		{cmd: "ln -s a.txt b.txt", want: false},
		{cmd: "ln -sfn dir1 dirlink", want: true, paths: []string{"dirlink"}},
		{cmd: "ln -f a.txt brandnew", want: false},
		{cmd: "cp *.log dir1", want: false},
		{cmd: "cp *.txt dst", want: false},
	} {
		check(t, dir, env, c)
	}
}

func TestRedirects(t *testing.T) {
	dir := fixture(t)
	env := baseEnv(dir)
	for _, c := range []tc{
		{cmd: "echo hi > a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "echo hi >a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "echo hi 1> a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "make 2> a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "make &> a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "echo hi >| a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "echo hi > \"space file.txt\"", want: true, paths: []string{"space file.txt"}},
		{cmd: "cat b.txt > $BUILD/x", want: true, paths: []string{"dir1/x"}},
		{cmd: "echo hi >> a.txt", want: false},
		{cmd: "echo hi > new.txt", want: false},
		{cmd: "echo hi > empty.txt", want: false},
		{cmd: "echo hi > /dev/null", want: false},
		{cmd: "ls 2>&1", want: false},
		{cmd: "ls > /dev/null 2>&1", want: false},
		{cmd: "cat < a.txt", want: false},
		{cmd: "sort a.txt > a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "echo '>' a.txt", want: false},
		{cmd: "echo \"x > a.txt\"", want: false},
		{cmd: "tee a.txt < b.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "echo x | tee -a a.txt", want: false},
		{cmd: "echo x | sudo tee a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "echo x | tee new.txt", want: false},
		{cmd: "{ echo a; } > a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "for f in x; do echo; done > a.txt", want: true, paths: []string{"a.txt"}},
	} {
		check(t, dir, env, c)
	}
}

func TestEditorsAndMisc(t *testing.T) {
	dir := fixture(t)
	env := baseEnv(dir)
	for _, c := range []tc{
		{cmd: "sed -i 's/a/b/' a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "sed -i '' 's/a/b/' a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "sed -i.bak 's/a/b/' a.txt b.txt", want: true, paths: []string{"a.txt", "b.txt"}},
		{cmd: "sed --in-place -e 's/a/b/' a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "sed -ni 's/a/b/p' a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "sed -e 's/a/b/' -i a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "gsed -i s/x/y/ *.log", want: true, paths: []string{"c.log", "d.log"}},
		{cmd: "sed 's/a/b/' a.txt", want: false},
		{cmd: "sed -n p a.txt", want: false},
		{cmd: "perl -pi -e 's/a/b/' a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "perl -i.bak -pe 's/a/b/' a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "perl -pe 's/a/b/' a.txt", want: false},
		{cmd: "perl -e 'print 1'", want: false},
		{cmd: "truncate -s 0 a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "truncate --size=0 a.txt b.txt", want: true, paths: []string{"a.txt", "b.txt"}},
		{cmd: "truncate -s 10 new.txt", want: false},
		{cmd: "dd if=a.txt of=b.txt", want: true, paths: []string{"b.txt"}},
		{cmd: "dd if=a.txt of=new.bin", want: false},
		{cmd: "dd if=/dev/zero of=/dev/disk9", want: true},
		{cmd: "dd if=a.txt", want: false},
		{cmd: "shred a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "shred -n 3 -u a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "unlink a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "rmdir emptydir", want: true, paths: []string{"emptydir"}},
		{cmd: "rmdir -p sub/deeper", want: true, paths: []string{"sub/deeper", "sub"}},
		{cmd: "find . -name '*.log' -delete", want: true, paths: []string{"c.log", "d.log"}},
		{cmd: "find dir1 -type f -delete", want: true, paths: []string{"dir1/x", "dir1/y"}},
		{cmd: "find . -name '*.log' -exec rm {} \\;", want: true, paths: []string{"c.log", "d.log"}},
		{cmd: "find . -name '*.log' -exec rm -f {} +", want: true, paths: []string{"c.log", "d.log"}},
		{cmd: "find . -name '*.log' -execdir rm {} \\;", want: true, paths: []string{"c.log"}},
		{cmd: "find . -name '*.log' -exec sh -c 'rm $0' {} \\;", want: true, broad: true},
		{cmd: "find . -name '*.log'", want: false},
		{cmd: "find . -name '*.log' -print", want: false},
		{cmd: "find . -name '*.log' -exec grep x {} \\;", want: false},
		{cmd: "find . -name '*.log' | xargs rm", want: true, broad: true},
		{cmd: "find . -name '*.log' -print0 | xargs -0 rm -f", want: true, broad: true},
		{cmd: "ls | xargs -I{} rm {}", want: true, broad: true},
		{cmd: "echo a.txt | xargs cat", want: false},
		{cmd: "rsync -a --delete src/ dst/", want: true, paths: []string{"dst"}},
		{cmd: "rsync -av --delete-after src/ dst", want: true, paths: []string{"dst"}},
		{cmd: "rsync -a src/main.go dst/", want: true, paths: []string{"dst/main.go"}},
		{cmd: "rsync -a src/ newdir/", want: false},
		{cmd: "rsync -a --remove-source-files src/ newdir/", want: true, paths: []string{"src"}},
		{cmd: "rsync -a --delete src/ host:/backup/", want: true},
		{cmd: "rsync -a src/ host:/backup/", want: false},
	} {
		check(t, dir, env, c)
	}
}

func TestHarmless(t *testing.T) {
	dir := fixture(t)
	env := baseEnv(dir)
	harmless := []string{
		"ls", "ls -la", "pwd", "cat a.txt", "echo hello", "echo rm -rf /", "echo 'rm -rf dir1'", "printf 'rm %s' x",
		"grep -r rm .", "grep 'git reset --hard' notes.md", "git status", "git log --oneline", "git diff", "git add .",
		"git commit -m 'rm -rf everything'", "git commit -m \"git reset --hard\"", "git fetch", "git pull", "git push",
		"git push origin main", "git reset", "git reset --soft HEAD~1", "git reset HEAD a.txt", "git checkout -b feature",
		"git checkout main", "git switch main", "git switch -c new", "git branch new", "git branch -a", "git stash",
		"git stash list", "git stash show", "git clean -n", "git clean --dry-run -fd", "git restore --staged a.txt",
		"git rm --cached a.txt", "git mv a.txt moved.txt", "git tag v1", "git remote -v", "git rebase main",
		"npm install", "go build ./...", "make", "cargo test", "python3 -c 'print(1)'", "node -e 1", "vim a.txt",
		"mkdir newdir", "mkdir -p a/b/c", "touch new.txt", "chmod +x a.txt", "chown me a.txt", "cd dir1", "cd ..",
		"history", "which rm", "type rm", "man rm", "rm --help", "rm --version", "alias rm='rm -i'", "export X=1",
		"diff a.txt b.txt", "head a.txt", "tail -f c.log", "less a.txt", "wc -l a.txt", "sort a.txt", "uniq a.txt",
		"tar czf out.tgz dir1", "zip -r out.zip dir1", "curl -O https://example.com/x", "ssh host ls",
		"docker ps", "kubectl get pods", "brew install x", "open .", "code .", "ps aux | grep rm",
		"echo $((1 + 2))", "test -f a.txt && echo yes", "[ -d dir1 ] || echo no", "true", ":", "",
		"mv", "cp", "cp a.txt", "ln -s a.txt newlink", "ls rmdir", "cat rm.txt", "./rm-script-not-real --help",
		"cp -n a.txt b.txt", "cat a.txt | tee -a b.txt", "find . -type f", "rsync -n -a --delete src/ dst/x/y/z/nonexistent",
		"sed -n '1p' a.txt", "awk '{print}' a.txt", "git show HEAD", "git blame a.txt", "git cherry-pick abc",
		"echo > /dev/stderr", "date >> c.log", "true > /dev/null", "git branch --list", "git config --get user.name",
	}
	for _, h := range harmless {
		check(t, dir, env, tc{cmd: h, want: false})
	}
}

func gitFixture(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := fixture(t)
	run := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	run("add", ".")
	run("commit", "-qm", "init")
	return dir
}

func TestGit(t *testing.T) {
	dir := gitFixture(t)
	env := baseEnv(dir)
	for _, c := range []tc{
		{cmd: "git reset --hard", want: true, git: "reset-hard"},
		{cmd: "git reset --hard HEAD~1", want: true, git: "reset-hard"},
		{cmd: "git reset --hard origin/main", want: true, git: "reset-hard"},
		{cmd: "git reset --merge", want: true, git: "reset-hard"},
		{cmd: "git -C sub reset --hard", want: true, git: "reset-hard"},
		{cmd: "git --no-pager reset --hard", want: true, git: "reset-hard"},
		{cmd: "git -c core.x=y reset --hard", want: true, git: "reset-hard"},
		{cmd: "git checkout -- a.txt", want: true, git: "checkout-paths"},
		{cmd: "git checkout a.txt", want: true, git: "checkout-paths"},
		{cmd: "git checkout .", want: true, git: "checkout-paths"},
		{cmd: "git checkout HEAD -- a.txt b.txt", want: true, git: "checkout-paths"},
		{cmd: "git checkout HEAD~2 a.txt", want: true, git: "checkout-paths"},
		{cmd: "git checkout -f main", want: true, git: "reset-hard"},
		{cmd: "git checkout --force main", want: true, git: "reset-hard"},
		{cmd: "git restore a.txt", want: true, git: "checkout-paths"},
		{cmd: "git restore .", want: true, git: "checkout-paths"},
		{cmd: "git restore --source=HEAD~1 a.txt", want: true, git: "checkout-paths"},
		{cmd: "git restore --staged --worktree a.txt", want: true, git: "checkout-paths"},
		{cmd: "git switch -f main", want: true, git: "reset-hard"},
		{cmd: "git switch --discard-changes main", want: true, git: "reset-hard"},
		{cmd: "git clean -fd", want: true, git: "clean"},
		{cmd: "git clean -f", want: true, git: "clean"},
		{cmd: "git clean -fdx", want: true, git: "clean"},
		{cmd: "git clean -xdf", want: true, git: "clean"},
		{cmd: "git clean --force -d", want: true, git: "clean"},
		{cmd: "git clean -ffdx", want: true, git: "clean"},
		{cmd: "git branch -D feature", want: true, git: "branch-delete"},
		{cmd: "git branch -d feature", want: true, git: "branch-delete"},
		{cmd: "git branch --delete --force a b", want: true, git: "branch-delete"},
		{cmd: "git branch -rd origin/x", want: true, git: "branch-delete"},
		{cmd: "git stash drop", want: true, git: "stash-drop"},
		{cmd: "git stash drop stash@{2}", want: true, git: "stash-drop"},
		{cmd: "git stash pop", want: true, git: "stash-drop"},
		{cmd: "git stash clear", want: true, git: "stash-clear"},
		{cmd: "git push --force", want: true, git: "push-force"},
		{cmd: "git push -f origin main", want: true, git: "push-force"},
		{cmd: "git push --force-with-lease origin main", want: true, git: "push-force"},
		{cmd: "git push --force-with-lease=main:abc origin main", want: true, git: "push-force"},
		{cmd: "git push origin +main", want: true, git: "push-force"},
		{cmd: "git push origin :old-branch", want: true, git: "push-force"},
		{cmd: "git push --delete origin old", want: true, git: "push-force"},
		{cmd: "git push --mirror", want: true, git: "push-force"},
		{cmd: "git rm a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "git rm -r dir1", want: true, paths: []string{"dir1"}},
		{cmd: "git mv -f a.txt b.txt", want: true, paths: []string{"b.txt"}},
		{cmd: "git worktree remove --force ../wt", want: true},
		{cmd: "cd sub && git reset --hard", want: true, git: "reset-hard"},
		{cmd: "git add . && git reset --hard", want: true, git: "reset-hard"},
		{cmd: "sudo git clean -fdx", want: true, git: "clean"},
		{cmd: "bash -c 'git reset --hard'", want: true, git: "reset-hard"},
	} {
		check(t, dir, env, c)
	}
}

func TestLoops(t *testing.T) {
	dir := fixture(t)
	env := baseEnv(dir)
	for _, c := range []tc{
		{cmd: `for f in *.log; do rm "$f"; done`, want: true, paths: []string{"c.log", "d.log"}},
		{cmd: `for f in a b; do rm "$f.txt"; done`, want: true, paths: []string{"a.txt", "b.txt"}},
		{cmd: `for f in $(ls); do rm "$f"; done`, want: true, broad: true},
		{cmd: `while read f; do rm "$f"; done < list`, want: true, broad: true},
		{cmd: `for ((i=0;i<3;i++)); do rm a.txt; done`, want: true, paths: []string{"a.txt"}},
	} {
		check(t, dir, env, c)
	}
}

func TestFishAndOddSyntax(t *testing.T) {
	dir := fixture(t)
	env := baseEnv(dir)
	for _, c := range []tc{
		{cmd: "ls; and rm a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "true; or rm -rf dir1", want: true, paths: []string{"dir1"}},
		{cmd: "rm -rf (echo dir1)", want: true},
		{cmd: "begin; rm a.txt; end", want: true, paths: []string{"a.txt"}},
		{cmd: "rm -rf 'unterminated", want: true},
	} {
		check(t, dir, env, c)
	}
}

func TestQuickFilterNeverMisses(t *testing.T) {
	for _, c := range []string{"rm x", "sudo rm x", "/bin/rm x", "x && rm y", "echo > f", "git reset --hard", "gsed -i x f",
		"truncate -s0 f", "find . -delete", "rsync --delete a b", "dd of=x", "shred x", "unlink x", "rmdir x", "mv a b", "cp a b",
		"ln -sf a b", "tee f", "perl -pi -e x f", "xargs rm", "(rm x)", "$(rm x)", "`rm x`", "{rm,x}"} {
		if !MaybeDestructive(c) {
			t.Errorf("prefilter skipped %q", c)
		}
	}
	for _, c := range []string{"ls -la", "cat file", "npm test", "go build", "make", "cd src", "echo hi", "docker ps", "terraform plan", "remote", "information", "firmware", "digit", "github"} {
		if MaybeDestructive(c) {
			t.Errorf("prefilter flagged %q", c)
		}
	}
}

func BenchmarkClassifyHarmless(b *testing.B) {
	cwd, _ := os.Getwd()
	env := os.Environ()
	for i := 0; i < b.N; i++ {
		Classify("git status --short && ls -la | grep foo", cwd, env)
	}
}

func BenchmarkClassifyPrefilterOnly(b *testing.B) {
	cwd, _ := os.Getwd()
	env := os.Environ()
	for i := 0; i < b.N; i++ {
		Classify("npm run test -- --watch", cwd, env)
	}
}

func BenchmarkClassifyDestructive(b *testing.B) {
	cwd, _ := os.Getwd()
	env := os.Environ()
	for i := 0; i < b.N; i++ {
		Classify("cd /tmp && rm -rf ./build *.log", cwd, env)
	}
}
