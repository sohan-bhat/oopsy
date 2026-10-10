package classify

import "testing"

func TestReadOnly(t *testing.T) {
	ro := []string{
		"ls -la", "cat README.md | grep foo", "cd src && ls", "git status", "git log --oneline | head -5",
		"git diff HEAD~1", "git branch", "git branch -a", "grep -rn TODO . 2>/dev/null", "echo hi",
		"find . -name '*.go'", "oops list", "oops", "git -C repo status", "pwd", "wc -l $(git ls-files)",
		"less notes.md", "tail -f log.txt", "du -sh .", "",
	}
	rw := []string{
		"rm -rf build", "make clean", "npm run reset", "echo hi > f.txt", "cat a >> b", "git branch -D main",
		"git reset --hard", "find . -delete", "find . -exec rm {} +", "python cleanup.py", "./reset.sh",
		"git stash drop", "sort -o out.txt in.txt", "ls; rm x", "cat $(rm -rf x)", "git diff --output=x",
		"env FOO=1 make", "sed -i s/a/b/ f", "git checkout .", "ls && make", "vim notes.md",
	}
	for _, c := range ro {
		if !ReadOnly(c) {
			t.Errorf("ReadOnly(%q) = false, want true", c)
		}
	}
	for _, c := range rw {
		if ReadOnly(c) {
			t.Errorf("ReadOnly(%q) = true, want false", c)
		}
		if QuickReadOnly(c) {
			t.Errorf("shell prefilter skips %q", c)
		}
	}
	// The shell prefilter is narrower: whatever it skips must be read only.
	for _, c := range []string{"ls -la", "git status", "cat a.txt | grep x", "cd src", "git log --oneline", "oops list", "git branch -a"} {
		if !QuickReadOnly(c) {
			t.Errorf("shell prefilter does not skip %q", c)
		}
	}
	for _, c := range []string{"git branch -D x", "git log --output=x", "echo > f", "cat `rm x`", "ls $(touch y)"} {
		if QuickReadOnly(c) {
			t.Errorf("shell prefilter skips %q", c)
		}
	}
}

func TestReadOnlyWindows(t *testing.T) {
	for _, c := range []string{"dir", "Get-ChildItem -Recurse", "git status", "cd ..", "type notes.txt", "Get-Content a.txt | Select-String foo", "oops list"} {
		if !QuickReadOnlyWindows(c) {
			t.Errorf("not skipped: %q", c)
		}
	}
	for _, c := range []string{"del x", "Remove-Item -Recurse build", "echo hi > f.txt", "git branch -D x", "npm run reset", "Get-ChildItem | Remove-Item", "dir & del x", "cd $(rm x)", "Get-Content a; rm b", "gc a | Set-Content b"} {
		if QuickReadOnlyWindows(c) {
			t.Errorf("skipped: %q", c)
		}
	}
}
