package classify

import (
	"encoding/base64"
	"path/filepath"
	"testing"
	"unicode/utf16"
)

func psEnv(dir string) []string {
	return append(baseEnv(dir), "OOPS_TEST_DIR="+filepath.Join(dir, "dir1"))
}

func encodePS(script string) string {
	var b []byte
	for _, u := range utf16.Encode([]rune(script)) {
		b = append(b, byte(u), byte(u>>8))
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestPowerShellRemoveItem(t *testing.T) {
	dir := fixture(t)
	env := psEnv(dir)
	for _, c := range []tc{
		{cmd: "Remove-Item dir1 -Recurse -Force", want: true, paths: []string{"dir1"}},
		{cmd: "Remove-Item -Path dir1 -Recurse", want: true, paths: []string{"dir1"}},
		{cmd: "remove-item -recurse -force -path dir1", want: true, paths: []string{"dir1"}},
		{cmd: "Remove-Item -LiteralPath 'space file.txt'", want: true, paths: []string{"space file.txt"}},
		{cmd: "Remove-Item -Path:a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "rm -r -fo dir1", want: true, paths: []string{"dir1"}},
		{cmd: "rm -rf dir1", want: true, paths: []string{"dir1"}},
		{cmd: "ri dir1 -r", want: true, paths: []string{"dir1"}},
		{cmd: "del a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "erase a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "rd dir1 -Recurse", want: true, paths: []string{"dir1"}},
		{cmd: "rmdir emptydir", want: true, paths: []string{"emptydir"}},
		{cmd: "Microsoft.PowerShell.Management\\Remove-Item a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "Remove-Item *.log", want: true, paths: []string{"c.log", "d.log"}},
		{cmd: "Remove-Item *.LOG", want: true, paths: []string{"c.log", "d.log"}},
		{cmd: "Remove-Item dir1\\*", want: true, paths: []string{"dir1/x", "dir1/y", "dir1/.hidden"}},
		{cmd: "Remove-Item a.txt, b.txt", want: true, paths: []string{"a.txt", "b.txt"}},
		{cmd: "Remove-Item -Path a.txt,b.txt -Force", want: true, paths: []string{"a.txt", "b.txt"}},
		{cmd: "Remove-Item 'dir1\\x'", want: true, paths: []string{"dir1/x"}},
		{cmd: "Remove-Item .\\dir1 -Recurse -Force -ErrorAction SilentlyContinue", want: true, paths: []string{"dir1"}},
		{cmd: "Remove-Item 'it''s.txt'", want: true, paths: []string{"it's.txt"}},
		{cmd: `Remove-Item "it's.txt"`, want: true, paths: []string{"it's.txt"}},
		{cmd: "Remove-Item a.txt # tidy up", want: true, paths: []string{"a.txt"}},
		{cmd: "<# tidy #> Remove-Item a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "Remove-Item `\n  a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "Remove-Item dir1 -Recurse; Remove-Item a.txt", want: true, paths: []string{"dir1", "a.txt"}},
		{cmd: "Remove-Item a.txt && Remove-Item b.txt", want: true, paths: []string{"a.txt", "b.txt"}},
		{cmd: "Remove-Item ~/definitely-not-here-oopsy", want: true},
		{cmd: "Remove-Item dir1 -WhatIf", want: false},
		{cmd: "Remove-Item Env:OOPS_TEST_DIR", want: false},
		{cmd: "Remove-Item HKCU:\\Software\\Oopsy -Recurse", want: false},
	} {
		checkWith(t, ClassifyPowerShell, dir, env, c)
	}
}

func TestPowerShellVariablesAndPipelines(t *testing.T) {
	dir := fixture(t)
	env := psEnv(dir)
	for _, c := range []tc{
		{cmd: "$d = 'dir1'; Remove-Item $d -Recurse", want: true, paths: []string{"dir1"}},
		{cmd: `$d = "dir1"; Remove-Item "$d\x"`, want: true, paths: []string{"dir1/x"}},
		{cmd: "Remove-Item $env:OOPS_TEST_DIR\\x", want: true, paths: []string{"dir1/x"}},
		{cmd: "Remove-Item ${env:OOPS_TEST_DIR}/y", want: true, paths: []string{"dir1/y"}},
		{cmd: "Remove-Item (Join-Path $env:OOPS_TEST_DIR 'x')", want: true, paths: []string{"dir1/x"}},
		{cmd: "Remove-Item -Path (Join-Path -Path $env:OOPS_TEST_DIR -ChildPath y)", want: true, paths: []string{"dir1/y"}},
		{cmd: "Remove-Item $somethingUnknown -Recurse", want: true, broad: true},
		{cmd: "Remove-Item (Get-Content list.txt)", want: true, broad: true},
		{cmd: "Get-Content list.txt | Remove-Item", want: true, broad: true},
		{cmd: "Get-ChildItem -Filter *.log | Remove-Item", want: true, paths: []string{"c.log", "d.log"}},
		{cmd: "Get-ChildItem *.log | Remove-Item -Force", want: true, paths: []string{"c.log", "d.log"}},
		{cmd: "gci -Recurse -Include *.go | rm", want: true, paths: []string{"sub/deeper/file.go", "src/main.go", "dst/main.go"}},
		{cmd: "ls dir1 | Remove-Item -Recurse", want: true, paths: []string{"dir1/x", "dir1/y"}},
		{cmd: "Get-ChildItem -Recurse -File | Where-Object { $_.Length -gt 0 } | Remove-Item", want: true, paths: []string{"a.txt", "sub/deeper/file.go"}},
		{cmd: "Get-ChildItem *.log | ForEach-Object { Remove-Item $_.FullName }", want: true, paths: []string{"c.log", "d.log"}},
		{cmd: "gci *.log | % { $_ | Remove-Item }", want: true, broad: true},
		{cmd: "foreach ($f in (Get-ChildItem *.log)) { Remove-Item $f }", want: true, paths: []string{"c.log", "d.log"}},
		{cmd: "foreach ($f in $(gci -Filter *.log)) { Remove-Item $f.FullName -Force }", want: true, paths: []string{"c.log", "d.log"}},
		{cmd: "Set-Location dir1; Remove-Item x", want: true, paths: []string{"dir1/x"}},
		{cmd: "cd dir1; del y", want: true, paths: []string{"dir1/y"}},
		{cmd: "Push-Location sub; Remove-Item deeper -Recurse", want: true, paths: []string{"sub/deeper"}},
		{cmd: "if (Test-Path dir1) { Remove-Item dir1 -Recurse -Force }", want: true, paths: []string{"dir1"}},
		{cmd: "try { Remove-Item a.txt -ErrorAction Stop } catch { Write-Host $_ }", want: true, paths: []string{"a.txt"}},
		{cmd: "& { Remove-Item a.txt }", want: true, paths: []string{"a.txt"}},
		{cmd: "Invoke-Expression 'Remove-Item a.txt'", want: true, paths: []string{"a.txt"}},
		{cmd: "iex \"Remove-Item b.txt\"", want: true, paths: []string{"b.txt"}},
	} {
		checkWith(t, ClassifyPowerShell, dir, env, c)
	}
}

func TestPowerShellOverwritesAndOthers(t *testing.T) {
	dir := fixture(t)
	env := psEnv(dir)
	for _, c := range []tc{
		{cmd: "Move-Item b.txt a.txt -Force", want: true, paths: []string{"a.txt"}},
		{cmd: "mv b.txt a.txt -fo", want: true, paths: []string{"a.txt"}},
		{cmd: "Move-Item b.txt a.txt", want: false},
		{cmd: "Copy-Item b.txt a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "Copy-Item -Path src/main.go -Destination dst", want: true, paths: []string{"dst/main.go"}},
		{cmd: "cp src\\main.go dst\\", want: true, paths: []string{"dst/main.go"}},
		{cmd: "Copy-Item a.txt brand-new.txt", want: false},
		{cmd: "Set-Content a.txt 'x'", want: true, paths: []string{"a.txt"}},
		{cmd: "Set-Content -Path notes.md -Value ''", want: true, paths: []string{"notes.md"}},
		{cmd: "Set-Content -Path empty.txt -Value x", want: false},
		{cmd: "Clear-Content a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "'x' | Out-File a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "'x' | Out-File -FilePath a.txt -Encoding utf8", want: true, paths: []string{"a.txt"}},
		{cmd: "'x' | Out-File a.txt -Append", want: false},
		{cmd: "'x' | Tee-Object -FilePath a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "'x' > a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "Get-Process 2> a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: "'x' >> a.txt", want: false},
		{cmd: "dir > $null", want: false},
		{cmd: "git status 2>&1 > $null", want: false},
		{cmd: "New-Item a.txt -ItemType File -Force", want: true, paths: []string{"a.txt"}},
		{cmd: "New-Item a.txt -ItemType File", want: false},
		{cmd: "[IO.File]::Delete('a.txt')", want: true, paths: []string{"a.txt"}},
		{cmd: `[System.IO.Directory]::Delete("dir1", $true)`, want: true, paths: []string{"dir1"}},
		{cmd: "[IO.File]::WriteAllText('b.txt', 'gone')", want: true, paths: []string{"b.txt"}},
		{cmd: "git reset --hard", want: true, git: "reset-hard"},
		{cmd: "git clean -fdx", want: true, git: "clean"},
		{cmd: "git branch -D feature", want: true, git: "branch-delete"},
		{cmd: "git.exe stash clear", want: true, git: "stash-clear"},
		{cmd: "cmd /c del a.txt", want: true, paths: []string{"a.txt"}},
		{cmd: `cmd.exe /c "rd /s /q dir1"`, want: true, paths: []string{"dir1"}},
		{cmd: `pwsh -NoProfile -Command "Remove-Item dir1 -Recurse"`, want: true, paths: []string{"dir1"}},
		{cmd: "powershell -NoProfile -EncodedCommand " + encodePS("Remove-Item a.txt"), want: true, paths: []string{"a.txt"}},
		{cmd: "bash -c 'rm -rf dir1'", want: true, paths: []string{"dir1"}},
	} {
		checkWith(t, ClassifyPowerShell, dir, env, c)
	}
}

func TestPowerShellHarmless(t *testing.T) {
	dir := fixture(t)
	env := psEnv(dir)
	for _, cmd := range []string{
		"Get-ChildItem", "Get-Content a.txt", "Write-Host 'Remove-Item a.txt'", "git status", "git log --oneline",
		"Remove-Variable foo", "Test-Path a.txt", "Select-String rm a.txt", "'x' | Add-Content a.txt",
		"Get-ChildItem -Recurse | Measure-Object", "Rename-Item a.txt z.txt", "npm run build", "Remove-Item dir1 -WhatIf",
	} {
		checkWith(t, ClassifyPowerShell, dir, env, tc{cmd: cmd, want: false})
	}
}

func TestPowerShellDeleteFlags(t *testing.T) {
	dir := fixture(t)
	env := psEnv(dir)
	for cmd, want := range map[string]bool{
		"Remove-Item dir1 -Recurse":                        true,
		"Get-ChildItem *.log | Remove-Item":                true,
		"[IO.File]::Delete('a.txt')":                       true,
		"Copy-Item b.txt a.txt":                            false,
		"Set-Content a.txt x":                              false,
		"'x' > a.txt":                                      false,
		"Remove-Item dir1 -Recurse; Copy-Item b.txt a.txt": false,
	} {
		res := ClassifyPowerShell(cmd, dir, env)
		del := len(res.Actions) > 0
		for _, a := range res.Actions {
			del = del && a.Delete
		}
		if del != want {
			t.Errorf("%q: all delete=%v, want %v (%+v)", cmd, del, want, res.Actions)
		}
	}
}

func TestQuickFilterWindows(t *testing.T) {
	for _, cmd := range []string{"Remove-Item x", "REMOVE-ITEM x", "rm x", "del x", "rd /s /q x", "Move-Item a b -Force",
		"Copy-Item a b", "Set-Content f x", "'x' | Out-File f", "x > f", "[IO.File]::Delete('x')", "(Get-Item x).Delete()",
		"git reset --hard", "robocopy a b /MIR", "Clear-Content f", "iex $s", "powershell -enc AAAA"} {
		if !MaybeDestructiveWindows(cmd) {
			t.Errorf("prefilter missed %q", cmd)
		}
	}
	for _, cmd := range []string{"Get-ChildItem", "npm test", "Write-Output hello", "Get-Process", "cd C:\\code"} {
		if MaybeDestructiveWindows(cmd) {
			t.Errorf("prefilter flagged harmless %q", cmd)
		}
	}
}
