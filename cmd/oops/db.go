package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"
)

// The Postgres proxy lives in a separate oops-db binary so the shell hook,
// which runs before every risky command, stays small and starts fast.
func dbCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "db",
		Short: "Undo destructive SQL on Postgres (proxy, wrap, list, undo, prune)",
		Long: `oops db proxy sits between your app or agent and Postgres. Before DROP TABLE,
DROP SCHEMA, TRUNCATE, DELETE, UPDATE or ALTER TABLE ... DROP COLUMN runs, it
copies exactly the affected data into an "oops" schema inside the same
transaction. oops db undo puts it back.

Commands:
  oops db proxy --upstream postgres://... [--listen 127.0.0.1:6543]
  oops db wrap [--upstream url] -- <command>
  oops db list
  oops db undo [id] [-y]
  oops db prune [--days 7]`,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			bin, err := findDBBinary()
			if err != nil {
				return err
			}
			return execDB(bin, args)
		},
	}
}

func findDBBinary() (string, error) {
	if self, err := os.Executable(); err == nil {
		name := "oops-db"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		for _, s := range []string{self, resolve(self)} {
			p := filepath.Join(filepath.Dir(s), name)
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p, nil
			}
		}
	}
	if p, err := exec.LookPath("oops-db"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("oops-db is not installed next to oops; reinstall Oopsy to get the Postgres tools")
}

func resolve(p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return r
}
