package hooks

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// autoRunValue is the AutoRun registry value as found.
type autoRunValue struct {
	Exists bool   `json:"exists"`
	Value  string `json:"value"`
	Expand bool   `json:"expand"` // REG_EXPAND_SZ rather than REG_SZ
}

// autoRunStore reads and writes AutoRun. Tests swap in a fake.
type autoRunStore interface {
	get() (autoRunValue, error)
	set(autoRunValue) error
}

// cmdState is what install saw, so uninstall can put it back exactly.
type cmdState struct {
	Before autoRunValue `json:"before"`
	Clink  string       `json:"clink,omitempty"` // the clink program that registered our scripts folder
}

func cmdEntry() string { return cmdAutoRunEntry(cmdAutoRunFile()) }

func cmdInstalled() bool {
	v, err := autoRun.get()
	return err == nil && strings.Contains(v.Value, cmdEntry())
}

// writeCmdFiles writes the batch files, the macro file and, when Clink is
// installed, the Lua script. It is run again on every oops init so the
// files follow the oops binary if it moves.
func writeCmdFiles(bin string, clink bool) error {
	files := map[string]string{
		cmdAutoRunFile(): cmdAutoRunBatch(cmdMacroFile()),
		cmdHookFile():    cmdHookBatch(bin, false),
		cmdCallFile():    cmdHookBatch(bin, true),
		cmdMacroFile():   cmdMacros(cmdHookFile(), cmdCallFile()),
	}
	if clink {
		files[filepath.Join(cmdClinkDir(), "oopsy.lua")] = clinkScript(bin)
	}
	for p, body := range files {
		if err := writeKeepMode(p, []byte(body)); err != nil {
			return err
		}
	}
	return nil
}

func readCmdState() (cmdState, bool) {
	var st cmdState
	b, err := os.ReadFile(cmdStateFile())
	if err != nil || json.Unmarshal(b, &st) != nil {
		return st, false
	}
	return st, true
}

func writeCmdState(st cmdState) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	return writeKeepMode(cmdStateFile(), append(b, '\n'))
}

// installCmd adds the cmd.exe hook: files under ~/.oops/cmd, the Clink
// script when Clink is installed, and our entry at the front of AutoRun.
func installCmd(bin string) (string, error) {
	st, haveState := readCmdState()
	clink := findClink()
	if err := writeCmdFiles(bin, clink != ""); err != nil {
		return "", err
	}
	if clink != "" && st.Clink == "" {
		if err := clinkScripts(clink, true, cmdClinkDir()); err == nil {
			st.Clink = clink
		}
	}
	cur, err := autoRun.get()
	if err != nil {
		return "", err
	}
	if strings.Contains(cur.Value, cmdEntry()) {
		if haveState {
			return "", writeCmdState(st)
		}
		return "", nil
	}
	st.Before = cur
	if err := writeCmdState(st); err != nil {
		return "", err
	}
	bk, err := backup(cmdStateFile())
	if err != nil {
		return "", err
	}
	next := autoRunValue{Exists: true, Value: chainAutoRun(cur.Value, cmdEntry()), Expand: cur.Expand}
	return bk, autoRun.set(next)
}

// uninstallCmd takes our entry out of AutoRun. If nothing else changed
// AutoRun since install, the value goes back exactly as it was, or is
// deleted if there was none; otherwise only our entry is removed.
func uninstallCmd() (string, error) {
	st, haveState := readCmdState()
	cur, err := autoRun.get()
	if err != nil {
		return "", err
	}
	// Keep a copy of the value as it is now, like every other target.
	bk := ""
	if b, err := json.MarshalIndent(cur, "", "  "); err == nil {
		now := filepath.Join(cmdDir(), "autorun-at-uninstall.json")
		if writeKeepMode(now, append(b, '\n')) == nil {
			bk, _ = backup(now)
		}
	}
	ours := cmdEntry()
	switch {
	case haveState && cur.Value == chainAutoRun(st.Before.Value, ours):
		err = autoRun.set(st.Before)
	case strings.Contains(cur.Value, ours):
		rest, _ := unchainAutoRun(cur.Value, ours)
		next := autoRunValue{Exists: true, Value: rest, Expand: cur.Expand}
		if strings.TrimSpace(rest) == "" && (!haveState || !st.Before.Exists) {
			next = autoRunValue{}
		}
		err = autoRun.set(next)
	}
	if err != nil {
		return bk, err
	}
	if st.Clink != "" {
		_ = clinkScripts(st.Clink, false, cmdClinkDir())
	}
	if err := os.RemoveAll(cmdDir()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return bk, err
	}
	return bk, nil
}
