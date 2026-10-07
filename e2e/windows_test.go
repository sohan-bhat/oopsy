//go:build windows

package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// freeBytes is the free space on the volume holding p.
func freeBytes(t *testing.T, p string) int64 {
	t.Helper()
	u, _ := windows.UTF16PtrFromString(p)
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(u, &free, &total, &totalFree); err != nil {
		t.Fatal(err)
	}
	return int64(totalFree)
}

type snapEntry struct {
	Orig   string `json:"orig"`
	Method string `json:"method"`
	Size   int64  `json:"size"`
	Disk   int64  `json:"disk"`
	Base   string `json:"base"`
}

type snapMeta struct {
	ID      int         `json:"id"`
	Entries []snapEntry `json:"entries"`
	Skipped []struct {
		Path   string `json:"path"`
		Reason string `json:"reason"`
	} `json:"skipped"`
}

// latest returns the newest snapshot as oops list --json reports it.
func (e *env) latest(t *testing.T) snapMeta {
	t.Helper()
	out, _, err := e.oopsCmd("", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var list []snapMeta
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list) == 0 {
		t.Fatalf("list --json: %v %s", err, out)
	}
	return list[0]
}

// psHook runs the PowerShell hook for line the way the profile does.
func (e *env) psHook(t *testing.T, line string) string {
	t.Helper()
	c := exec.Command(bin, "hook", "powershell")
	c.Dir = e.work
	c.Env = append(e.environ(), "OOPSY_LINE="+line, "OOPSY_CWD="+e.work)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("hook: %v %s", err, out)
	}
	return string(out)
}

func pwsh(t *testing.T, dir, script string, env []string) string {
	t.Helper()
	c := exec.Command("pwsh", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
	c.Dir = dir
	c.Env = env
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("pwsh: %v\n%s", err, out)
	}
	return string(out)
}

func writeRandom(t *testing.T, root string, files, size int) {
	t.Helper()
	buf := make([]byte, size)
	for i := 0; i < files; i++ {
		rand.Read(buf)
		p := filepath.Join(root, fmt.Sprintf("d%d", i%4), fmt.Sprintf("f%03d.bin", i))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, buf, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func mb(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/(1<<20)) }

// TestWindowsHardLinkSnapshot deletes a 64 MB tree with Remove-Item through
// the PowerShell hook and checks the snapshot cost no disk.
func TestWindowsHardLinkSnapshot(t *testing.T) {
	e := newEnv(t)
	tree := filepath.Join(e.work, "big")
	writeRandom(t, tree, 64, 1<<20)
	makeTree(t, filepath.Join(tree, "mixed"))
	before := snapshotTree(t, tree, true)

	free := freeBytes(t, e.work)
	start := time.Now()
	msg := e.psHook(t, "Remove-Item -Recurse -Force big")
	took := time.Since(start)
	used := free - freeBytes(t, e.work)
	if !strings.Contains(msg, "saved snapshot 1") {
		t.Fatalf("no snapshot: %s", msg)
	}
	s := e.latest(t)
	if len(s.Entries) != 1 || s.Entries[0].Method != "hardlink" || s.Entries[0].Disk != 0 {
		t.Fatalf("want one hardlink entry with no disk, got %+v", s.Entries)
	}
	t.Logf("hard link snapshot of %s in %v: volume free space changed by %s", mb(s.Entries[0].Size), took.Round(time.Millisecond), mb(used))
	if used > 16<<20 {
		t.Errorf("hard link snapshot used %s of disk", mb(used))
	}
	pwsh(t, e.work, "Remove-Item -Recurse -Force big", e.environ())
	if _, err := os.Stat(tree); err == nil {
		t.Fatal("Remove-Item did not delete the tree")
	}
	e.mustOops("-y")
	compareTrees(t, before, snapshotTree(t, tree, true))
}

// TestWindowsCompressedCopy overwrites a compressible file, which must be
// copied (a hard link would change with it) and stored NTFS compressed.
func TestWindowsCompressedCopy(t *testing.T) {
	e := newEnv(t)
	f := filepath.Join(e.work, "app.log")
	var b bytes.Buffer
	for b.Len() < 32<<20 {
		fmt.Fprintf(&b, "%s INFO request served in %d ms from cache shard %d\n", time.Unix(int64(b.Len()), 0).UTC().Format(time.RFC3339), b.Len()%977, b.Len()%13)
	}
	orig := b.Bytes()
	os.WriteFile(f, orig, 0o644)
	free := freeBytes(t, e.work)
	e.psHook(t, "Set-Content app.log 'cleared'")
	used := free - freeBytes(t, e.work)
	s := e.latest(t)
	if len(s.Entries) != 1 || s.Entries[0].Method != "compressed copy" {
		t.Fatalf("want a compressed copy, got %+v", s.Entries)
	}
	en := s.Entries[0]
	t.Logf("compressed copy of %s: %s on disk by GetCompressedFileSize, volume free space changed by %s", mb(en.Size), mb(en.Disk), mb(used))
	if en.Disk <= 0 || en.Disk > en.Size/3 {
		t.Errorf("compressed copy takes %s for %s", mb(en.Disk), mb(en.Size))
	}
	pwsh(t, e.work, "Set-Content app.log 'cleared'", e.environ())
	os.Remove(f)
	e.mustOops("-y")
	got, _ := os.ReadFile(f)
	if !bytes.Equal(got, orig) {
		t.Fatal("restored file differs")
	}
	u, _ := windows.UTF16PtrFromString(f)
	if a, _ := windows.GetFileAttributes(u); a&windows.FILE_ATTRIBUTE_COMPRESSED != 0 {
		t.Error("the restored file kept the store's compression")
	}
}

func TestWindowsLockedFileIsSkipped(t *testing.T) {
	e := newEnv(t)
	tree := filepath.Join(e.work, "proj")
	makeTree(t, tree)
	locked := filepath.Join(tree, "src", "main.go")
	u, _ := windows.UTF16PtrFromString(locked)
	// Open for writing with no sharing, like a database holding its file.
	h, err := windows.CreateFile(u, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	start := time.Now()
	c := exec.Command(bin, "hook", "powershell")
	c.Dir = e.work
	c.Env = append(e.environ(), "OOPSY_LINE=Copy-Item ..\\x proj -Force; Remove-Item -Recurse -Force proj", "OOPSY_CWD="+e.work)
	out, _ := c.CombinedOutput()
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("hook blocked for %v on a locked file", took)
	}
	if !strings.Contains(string(out), "in use by another program") {
		t.Fatalf("expected a warning about the locked file:\n%s", out)
	}
	s := e.latest(t)
	if len(s.Entries) != 1 {
		t.Fatalf("the rest of the tree was not saved: %+v", s)
	}
}

// TestWindowsJunctionsAreNotFollowed puts a junction to an outside folder in
// the tree. The snapshot must keep the junction, not the outside files.
func TestWindowsJunctionsAreNotFollowed(t *testing.T) {
	e := newEnv(t)
	outside := filepath.Join(e.work, "outside")
	writeRandom(t, outside, 8, 1<<20)
	tree := filepath.Join(e.work, "proj")
	os.MkdirAll(tree, 0o755)
	os.WriteFile(filepath.Join(tree, "keep.txt"), []byte("keep\n"), 0o644)
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(tree, "j"), outside).CombinedOutput(); err != nil {
		t.Fatalf("mklink: %v %s", err, out)
	}
	e.psHook(t, "Remove-Item -Recurse -Force proj")
	s := e.latest(t)
	if len(s.Entries) != 1 || s.Entries[0].Size > 1<<20 {
		t.Fatalf("snapshot followed the junction: %+v", s.Entries)
	}
	pwsh(t, e.work, "Remove-Item -Recurse -Force proj", e.environ())
	if n, _ := os.ReadDir(filepath.Join(outside, "d0")); len(n) == 0 {
		t.Fatal("deleting the tree emptied the junction target")
	}
	e.mustOops("-y")
	target, err := os.Readlink(filepath.Join(tree, "j"))
	if err != nil || !strings.EqualFold(filepath.Clean(target), outside) {
		t.Fatalf("junction not restored: %q %v", target, err)
	}
	if b, _ := os.ReadFile(filepath.Join(tree, "keep.txt")); string(b) != "keep\n" {
		t.Fatal("file not restored")
	}
}

// TestWindowsLongPaths saves and restores a file whose path is far past
// MAX_PATH.
func TestWindowsLongPaths(t *testing.T) {
	for _, m := range []string{"", "copy"} {
		t.Run("method="+m, func(t *testing.T) {
			e := newEnv(t)
			deep := filepath.Join(e.work, "long")
			for len(deep) < 320 {
				deep = filepath.Join(deep, strings.Repeat("segment", 3))
			}
			if err := os.MkdirAll(deep, 0o755); err != nil {
				t.Fatal(err)
			}
			f := filepath.Join(deep, "file.txt")
			os.WriteFile(f, []byte("deep\n"), 0o644)
			c := exec.Command(bin, "hook", "powershell")
			c.Dir = e.work
			c.Env = append(e.environ(), "OOPSY_LINE=Remove-Item -Recurse -Force long", "OOPSY_CWD="+e.work, "OOPS_METHOD="+m)
			if out, err := c.CombinedOutput(); err != nil || !strings.Contains(string(out), "saved snapshot") {
				t.Fatalf("hook: %v %s", err, out)
			}
			os.RemoveAll(filepath.Join(e.work, "long"))
			e.mustOops("-y")
			if b, err := os.ReadFile(f); err != nil || string(b) != "deep\n" {
				t.Fatalf("long path not restored (%d chars): %v", len(f), err)
			}
		})
	}
}

// TestWindowsSecondVolume deletes a tree on another drive than the profile
// (D: on GitHub runners), which goes to a private store at that drive's root.
func TestWindowsSecondVolume(t *testing.T) {
	other := os.Getenv("RUNNER_TEMP")
	e := newEnv(t)
	if other == "" || strings.EqualFold(filepath.VolumeName(other), filepath.VolumeName(e.oops)) {
		t.Skip("needs RUNNER_TEMP on another drive than TEMP")
	}
	work, _ := os.MkdirTemp(other, "oops-vol")
	defer os.RemoveAll(work)
	tree := filepath.Join(work, "proj")
	writeRandom(t, tree, 16, 1<<20)
	before := snapshotTree(t, tree, true)
	c := exec.Command(bin, "hook", "powershell")
	c.Dir = work
	c.Env = append(e.environ(), "OOPSY_LINE=Remove-Item -Recurse -Force proj", "OOPSY_CWD="+work)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	s := e.latest(t)
	en := s.Entries[0]
	vol := filepath.VolumeName(work)
	if en.Method != "hardlink" || !strings.HasPrefix(strings.ToLower(en.Base), strings.ToLower(vol+`\.oops\`)) {
		t.Fatalf("want a hard link in %s\\.oops, got %+v", vol, en)
	}
	acl, _ := exec.Command("icacls", filepath.Dir(filepath.Dir(en.Base))).CombinedOutput()
	t.Logf("store %s, ACL:\n%s", en.Base, acl)
	for _, broad := range []string{"BUILTIN\\Users", "Everyone", "Authenticated Users"} {
		if strings.Contains(string(acl), broad) {
			t.Errorf("volume store is readable by %s", broad)
		}
	}
	os.RemoveAll(tree)
	e.mustOops("-y")
	compareTrees(t, before, snapshotTree(t, tree, true))
	e.mustOops("prune", "--all")
	if _, err := os.Stat(en.Base); err == nil {
		t.Error("prune left the volume store data behind")
	}
}

// TestWindowsReFSBlockClone runs on a ReFS volume when CI provides one.
func TestWindowsReFSBlockClone(t *testing.T) {
	root := os.Getenv("OOPS_TEST_REFS")
	if root == "" {
		t.Skip("set OOPS_TEST_REFS to a folder on a ReFS or Dev Drive volume")
	}
	e := newEnv(t)
	e.oops = filepath.Join(root, fmt.Sprintf("oops-home-%d", time.Now().UnixNano()))
	e.work = filepath.Join(root, fmt.Sprintf("work-%d", time.Now().UnixNano()))
	os.MkdirAll(e.work, 0o755)
	defer forceRemove(e.oops)
	defer forceRemove(e.work)
	f := filepath.Join(e.work, "data.bin")
	writeRandom(t, filepath.Join(e.work, "tree"), 32, 1<<20)
	big := make([]byte, 64<<20)
	rand.Read(big)
	os.WriteFile(f, big, 0o644)
	for _, c := range []struct{ line, path string }{
		{"Set-Content data.bin 'x'", f},
		{"Remove-Item -Recurse -Force tree", filepath.Join(e.work, "tree")},
	} {
		free := freeBytes(t, e.work)
		e.psHook(t, c.line)
		used := free - freeBytes(t, e.work)
		s := e.latest(t)
		if len(s.Entries) != 1 || s.Entries[0].Method != "block clone" {
			t.Fatalf("%s: want a block clone, got %+v %+v", c.line, s.Entries, s.Skipped)
		}
		t.Logf("ReFS block clone for %q of %s: volume free space changed by %s", c.line, mb(s.Entries[0].Size), mb(used))
		if used > 8<<20 {
			t.Errorf("block clone used %s", mb(used))
		}
	}
	os.WriteFile(f, []byte("x"), 0o644)
	os.Remove(f)
	e.mustOops("undo", "1", "-y")
	if got, _ := os.ReadFile(f); !bytes.Equal(got, big) {
		t.Fatal("block cloned file restored with different bytes")
	}
}

// TestWindowsAgentPayloads sends Claude Code payloads with Git Bash and
// PowerShell commands that name Windows paths.
func TestWindowsAgentPayloads(t *testing.T) {
	e := newEnv(t)
	vol := filepath.VolumeName(e.work)
	posix := "/" + strings.ToLower(vol[:1]) + filepath.ToSlash(e.work[len(vol):])
	for i, c := range []struct{ tool, cmd string }{
		{"Bash", "rm -rf " + posix + "/app0"},
		{"Bash", "rm -rf '" + filepath.Join(e.work, "app1") + "'"},
		{"Bash", "cd " + filepath.ToSlash(e.work) + " && rm -rf app2"},
		{"PowerShell", "Remove-Item -Recurse -Force " + filepath.Join(e.work, "app3")},
		{"Bash", "powershell -NoProfile -Command \"Remove-Item -Recurse -Force app4\""},
		{"Bash", "cmd /c rd /s /q app5"},
	} {
		dir := filepath.Join(e.work, fmt.Sprintf("app%d", i))
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644)
		payload, _ := json.Marshal(map[string]any{"cwd": e.work, "hook_event_name": "PreToolUse", "tool_name": c.tool,
			"tool_input": map[string]any{"command": c.cmd}})
		out, stderr, err := e.oopsCmd(string(payload), "hook", "claude")
		if err != nil {
			t.Fatalf("%q: %v %s", c.cmd, err, stderr)
		}
		var reply map[string]any
		if json.Unmarshal([]byte(out), &reply) != nil || !strings.Contains(fmt.Sprint(reply["systemMessage"]), "saved snapshot") {
			t.Errorf("%q: reply %q", c.cmd, out)
			continue
		}
		os.RemoveAll(dir)
		e.mustOops("-y")
		if _, err := os.Stat(filepath.Join(dir, "main.go")); err != nil {
			t.Errorf("%q: not restored", c.cmd)
		}
	}
}

// TestWindowsHookOverhead measures what the hooks cost on Windows: the
// PowerShell prefilter for every command, and the oops process for lines
// that pass it.
func TestWindowsHookOverhead(t *testing.T) {
	e := newEnv(t)
	var ds []time.Duration
	for i := 0; i < 40; i++ {
		c := exec.Command(bin, "hook", "powershell")
		c.Dir = e.work
		c.Env = append(e.environ(), "OOPSY_LINE=git status", "OOPSY_CWD="+e.work)
		start := time.Now()
		if err := c.Run(); err != nil {
			t.Fatal(err)
		}
		ds = append(ds, time.Since(start))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	t.Logf("oops hook process for a line that passes the prefilter (git status): median %s, p90 %s", ms(ds[len(ds)/2]), ms(ds[len(ds)*9/10]))
	if ds[len(ds)/2] > 500*time.Millisecond {
		t.Errorf("hook too slow: %s", ms(ds[len(ds)/2]))
	}
	if _, err := exec.LookPath("pwsh"); err != nil {
		return
	}
	init, _, _ := e.oopsCmd("", "shell-init", "powershell")
	script := init + `
$n = 20000
$t = Measure-Command { for ($i = 0; $i -lt $n; $i++) { __OopsyCheck 'npm run build -- --watch' } }
[math]::Round($t.TotalMilliseconds * 1000 / $n, 2)`
	out := pwsh(t, e.work, script, e.environ())
	t.Logf("PowerShell prefilter for a harmless line: %s microseconds per command, no process started", strings.TrimSpace(out))
}

// TestWindowsPowerShell51 runs the hook in Windows PowerShell 5.1, which
// ships with Windows.
func TestWindowsPowerShell51(t *testing.T) {
	e := newEnv(t)
	tree := filepath.Join(e.work, "project")
	writeRandom(t, tree, 4, 64<<10)
	os.WriteFile(filepath.Join(tree, "ro.txt"), []byte("ro\n"), 0o444)
	before := snapshotTree(t, tree, true)
	e.mustOops("init", "--only", "windowspowershell")
	out := e.runPS(t, "powershell.exe", e.psProfile("windowspowershell"), "Remove-Item -Recurse -Force project")
	if !strings.Contains(out, "saved snapshot 1") {
		t.Fatalf("Windows PowerShell hook did not fire:\n%s", out)
	}
	e.mustOops("-y")
	compareTrees(t, before, snapshotTree(t, tree, true))
}
