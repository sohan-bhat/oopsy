//go:build !windows

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func freeUnix(t *testing.T, p string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		t.Fatal(err)
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// TestDiskUseByMethod logs what a delete and an overwrite of 64 MB cost on
// this machine's filesystem (APFS clones on macOS, hard links and copies on
// ext4), measured as free space before and after the snapshot.
func TestDiskUseByMethod(t *testing.T) {
	e := newEnv(t)
	writeData := func(p string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		b := bytes.Repeat([]byte("0123456789abcdef"), 4<<20) // 64 MB
		for i := 0; i < len(b); i += 4096 {
			b[i] = byte(i >> 12) // keep blocks distinct
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ name, cmd string }{
		{"delete", "rm -rf tree"},
		{"overwrite", "echo x > big.bin"},
	} {
		writeData(filepath.Join(e.work, "tree", "data.bin"))
		writeData(filepath.Join(e.work, "big.bin"))
		syscall.Sync()
		free := freeUnix(t, e.work)
		e.oopsCmd("", "hook", "zsh", "--", c.cmd)
		syscall.Sync()
		used := free - freeUnix(t, e.work)
		out, _, _ := e.oopsCmd("", "list", "--json")
		var list []struct {
			Entries []struct {
				Method string `json:"method"`
				Disk   int64  `json:"disk"`
			} `json:"entries"`
		}
		json.Unmarshal([]byte(out), &list)
		method := "?"
		if len(list) > 0 && len(list[0].Entries) > 0 {
			method = list[0].Entries[0].Method
		}
		t.Logf("%s of 64 MB: method %s, free space changed by %.1f MB", c.name, method, float64(used)/(1<<20))
		if c.name == "delete" && method != "copy" && used > 16<<20 {
			t.Errorf("%s snapshot used %s", method, fmt.Sprint(used))
		}
		if strings.Contains(method, "copy") && used < 32<<20 {
			t.Logf("copy measured below its size; the filesystem may compress or dedupe")
		}
		e.mustOops("prune", "--all")
	}
}
