package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sohan-bhat/oopsy/internal/cli"
	"github.com/sohan-bhat/oopsy/internal/snap"
	"github.com/sohan-bhat/oopsy/internal/ui"
)

func fail(format string, a ...any) { ui.Errorf(format, a...) }

var sourceLabels = map[string]string{
	"zsh": "zsh", "bash": "bash", "fish": "fish", "shell": "shell", "powershell": "PowerShell", "cmd": "cmd",
	"claude": "Claude Code", "codex": "Codex", "cursor": "Cursor", "gemini": "Gemini CLI", "manual": "manual",
}

func sourceLabel(s string) string {
	if l, ok := sourceLabels[s]; ok {
		return l
	}
	return s
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
	return t.Format("Jan 2 15:04")
}

func parseID(arg string) (int, error) {
	id, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
	if err != nil || id <= 0 {
		return 0, cli.UsageErr("%q is not a snapshot id. oops list shows them; then run for example: oops undo 12", arg)
	}
	return id, nil
}

// loadSnapshot loads a snapshot, explaining a missing one.
func loadSnapshot(id int) (*snap.Snapshot, error) {
	s, err := snap.Load(id)
	if err != nil {
		if _, statErr := os.Stat(snap.SnapDir(id)); os.IsNotExist(statErr) {
			return nil, cli.NotFound("there is no snapshot %d. oops list shows the ones you have (they are kept %d days)", id, snap.LoadConfig().KeepDays)
		}
		return nil, fmt.Errorf("snapshot %d could not be read: %s. Its folder is %s", id, ui.Explain(err), ui.HomeRel(snap.SnapDir(id)))
	}
	return s, nil
}

// warnConfig says when the settings file cannot be read.
func warnConfig() {
	if p := snap.ConfigProblem(); p != "" {
		fmt.Fprintf(os.Stderr, "%s %s\n", ui.ErrWarn("oops:"), p)
	}
}

func undoLatest(yes bool) error {
	warnConfig()
	s, err := snap.Latest()
	if err != nil {
		return err
	}
	if s == nil {
		fmt.Println("Nothing to undo. Oopsy has no unrestored snapshots.")
		if !anyHookInstalled() {
			fmt.Println(ui.Dim("Hooks are not installed yet. Run oops init to start protecting your commands."))
		}
		return nil
	}
	return confirmAndRestore(s, yes)
}

func printHeader(s *snap.Snapshot) {
	fmt.Printf("%s  %s  %s\n", ui.Accent(fmt.Sprintf("Snapshot %d", s.ID)), ui.Dim(ago(s.Time)), ui.Dim("from "+sourceLabel(s.Source)))
	fmt.Printf("  %s %s\n", ui.Dim("$"), ui.Shorten(s.Command, 200))
	fmt.Printf("  %s %s\n", ui.Dim("in"), ui.HomeRel(s.Cwd))
}

func confirmAndRestore(s *snap.Snapshot, yes bool) error {
	printHeader(s)
	plan := snap.Plan(s)
	if len(plan) == 0 {
		fmt.Println()
		fmt.Println("This snapshot has nothing that can be restored.")
		for _, sk := range s.Skipped {
			fmt.Printf("  %s %s: %s\n", ui.Warn("not saved"), ui.HomeRel(sk.Path), sk.Reason)
		}
		for _, n := range s.Notes {
			fmt.Printf("  %s %s\n", ui.Warn("note"), n)
		}
		return nil
	}
	fmt.Println()
	fmt.Println("This will bring back:")
	width := 0
	for _, p := range plan {
		if l := len(ui.HomeRel(p.Path)); l > width && l < 70 {
			width = l
		}
	}
	for _, p := range plan {
		line := fmt.Sprintf("  %-*s  %s", width, ui.HomeRel(p.Path), ui.Dim(p.What))
		if p.Exists {
			line += "  " + ui.Warn("(exists now, both copies are kept)")
		}
		fmt.Println(line)
	}
	for _, sk := range s.Skipped {
		fmt.Printf("  %s %s: %s\n", ui.Warn("not saved"), ui.HomeRel(sk.Path), sk.Reason)
	}
	fmt.Println()
	if !yes {
		if !ui.Interactive() {
			return fmt.Errorf("no terminal to confirm on; run with -y to restore without asking")
		}
		if !ui.Confirm("Restore it?") {
			fmt.Println("Nothing changed.")
			return nil
		}
	}
	outcomes, err := snap.Restore(s)
	printOutcomes(outcomes)
	if err != nil {
		return err
	}
	failed := 0
	for _, o := range outcomes {
		if o.Status == "failed" {
			failed++
		}
	}
	if failed > 0 {
		return cli.Partial("%s could not be restored; the lines above say why. Everything else is back", ui.Plural(failed, "path"))
	}
	fmt.Println(ui.Good(fmt.Sprintf("Restored snapshot %d.", s.ID)))
	return nil
}

func printOutcomes(outcomes []snap.Outcome) {
	same := 0
	shown := 0
	for _, o := range outcomes {
		switch o.Status {
		case "restored":
			if shown < 30 {
				fmt.Printf("  %s %s\n", ui.Good("restored"), ui.HomeRel(o.Path))
			}
			shown++
		case "kept-both":
			fmt.Printf("  %s %s exists, saved copy put at %s\n", ui.Warn("kept both"), ui.HomeRel(o.Path), ui.HomeRel(o.Detail))
		case "same":
			same++
		case "git":
			fmt.Printf("  %s %s\n", ui.Good("git"), o.Detail)
		case "failed":
			fmt.Printf("  %s %s: %s\n", ui.Bad("failed"), ui.HomeRel(o.Path), o.Detail)
		}
	}
	if shown > 30 {
		fmt.Printf("  %s\n", ui.Dim(fmt.Sprintf("and %d more", shown-30)))
	}
	if same > 0 {
		fmt.Printf("  %s\n", ui.Dim(ui.Plural(same, "file")+" already matched and were left alone"))
	}
	kept := 0
	for _, o := range outcomes {
		if o.Status == "kept-both" {
			kept++
		}
	}
	if kept > 0 {
		fmt.Printf("  %s\n", ui.Dim(fmt.Sprintf("%s existed again with different content, so nothing was overwritten: each saved copy sits next to it with .oops and the snapshot id in its name. Compare them, keep what you want, and delete the other.", ui.Plural(kept, "path"))))
	}
}

func undoCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "undo <id>",
		Short: "Restore a specific snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			s, err := loadSnapshot(id)
			if err != nil {
				return err
			}
			if s.Restored != nil {
				fmt.Println(ui.Dim(fmt.Sprintf("Snapshot %d was already restored %s; restoring again keeps any existing files.", id, ago(*s.Restored))))
			}
			return confirmAndRestore(s, yes)
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "restore without asking")
	return c
}

func listCmd() *cobra.Command {
	var n int
	var asJSON bool
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls", "log"},
		Short:   "Show recent snapshots, newest first",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			warnConfig()
			list, err := snap.List()
			if err != nil {
				return err
			}
			if asJSON {
				b, _ := json.MarshalIndent(list, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			if len(list) == 0 {
				fmt.Println("No snapshots yet.")
				if !anyHookInstalled() {
					fmt.Println(ui.Dim("Run oops init to install the hooks."))
				}
				return nil
			}
			if n > 0 && len(list) > n {
				list = list[:n]
			}
			fmt.Printf("%s  %s  %s  %s  %s\n", ui.Dim(pad("ID", 4)), ui.Dim(pad("WHEN", 11)), ui.Dim(pad("FROM", 11)), ui.Dim(pad("SIZE", 9)), ui.Dim("COMMAND"))
			for _, s := range list {
				id := pad(strconv.Itoa(s.ID), 4)
				if s.Restored == nil {
					id = ui.Accent(id)
				} else {
					id = ui.Dim(id)
				}
				size := ui.Bytes(s.Size)
				if len(s.Entries) == 0 && len(s.Git) > 0 {
					size = "git"
				}
				line := fmt.Sprintf("%s  %s  %s  %s  %s", id, pad(ago(s.Time), 11), pad(sourceLabel(s.Source), 11), pad(size, 9), ui.Shorten(s.Command, 60))
				if s.Restored != nil {
					line += " " + ui.Dim("(restored)")
				}
				fmt.Println(line)
			}
			return nil
		},
	}
	c.Flags().IntVarP(&n, "number", "n", 20, "how many to show (0 for all)")
	c.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return c
}

func pad(s string, n int) string {
	if r := []rune(s); len(r) < n {
		return s + strings.Repeat(" ", n-len(r))
	}
	return s
}

func showCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show what a snapshot contains",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			s, err := loadSnapshot(id)
			if err != nil {
				return err
			}
			printHeader(s)
			fmt.Printf("  %s %s, %s in %.1f ms\n", ui.Dim("saved"), s.Time.Format("Mon Jan 2 15:04:05 2006"), ui.Bytes(s.Size), s.Millis)
			if s.Restored != nil {
				fmt.Printf("  %s %s\n", ui.Dim("restored"), s.Restored.Format("Mon Jan 2 15:04:05 2006"))
			}
			fmt.Println()
			for _, e := range s.Entries {
				fmt.Printf("%s %s  %s\n", ui.Bold(ui.HomeRel(e.Orig)), ui.Dim(e.Type), ui.Dim(entryStorage(e)))
				if e.Type == "dir" {
					printTree(s.EntryPath(e), 25)
				}
			}
			for _, g := range s.Git {
				fmt.Printf("%s %s\n", ui.Bold("git"), describeGit(g))
			}
			for _, sk := range s.Skipped {
				fmt.Printf("%s %s: %s\n", ui.Warn("not saved"), ui.HomeRel(sk.Path), sk.Reason)
			}
			for _, n := range s.Notes {
				fmt.Printf("%s %s\n", ui.Warn("note"), n)
			}
			return nil
		},
	}
}

// entryStorage says how an entry was saved and what it costs on disk.
func entryStorage(e snap.Entry) string {
	disk := e.Disk
	if e.Method == "copy" && disk == 0 {
		disk = e.Size // snapshots from before disk was recorded
	}
	extra := ui.Bytes(disk) + " extra on disk"
	if disk == 0 {
		extra = "shares the original's data, no extra disk"
	}
	return fmt.Sprintf("%s, %s, %s", e.Method, ui.Bytes(e.Size), extra)
}

func describeGit(g snap.GitRecord) string {
	short := func(s string) string {
		if len(s) > 9 {
			return s[:9]
		}
		return s
	}
	switch g.Kind {
	case "reset-hard":
		return fmt.Sprintf("HEAD was %s in %s", short(g.Head), ui.HomeRel(g.Repo))
	case "branch-delete":
		return fmt.Sprintf("branch %s was at %s", strings.TrimPrefix(g.Ref, "refs/heads/"), short(g.SHA))
	case "stash":
		return fmt.Sprintf("stash %s: %s", short(g.SHA), g.Message)
	case "push":
		return fmt.Sprintf("%s/%s was at %s before the force push", g.Remote, g.Ref, short(g.SHA))
	}
	return g.Kind
}

func printTree(root string, limit int) {
	n := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return nil
		}
		n++
		if n > limit {
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(root, p)
		depth := strings.Count(filepath.ToSlash(rel), "/")
		name := d.Name()
		if d.IsDir() {
			name += "/"
		}
		fmt.Printf("  %s%s\n", strings.Repeat("  ", depth), name)
		return nil
	})
	if n > limit {
		fmt.Printf("  %s\n", ui.Dim("and more"))
	}
}

func diffCmd() *cobra.Command {
	var noContent bool
	c := &cobra.Command{
		Use:   "diff <id>",
		Short: "Compare a snapshot with what is on disk now",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			s, err := loadSnapshot(id)
			if err != nil {
				return err
			}
			printHeader(s)
			fmt.Println()
			var missing, changed, added, same int
			budget := 400
			for _, e := range s.Entries {
				root := s.EntryPath(e)
				_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
					if err != nil {
						return nil
					}
					rel, _ := filepath.Rel(root, p)
					cur := e.Orig
					if rel != "." {
						cur = filepath.Join(e.Orig, rel)
					}
					cfi, cerr := os.Lstat(cur)
					if cerr != nil {
						missing++
						fmt.Printf("%s %s\n", ui.Bad("missing"), ui.HomeRel(cur))
						if d.IsDir() {
							return filepath.SkipDir
						}
						return nil
					}
					if d.IsDir() {
						if cfi.IsDir() {
							ents, _ := os.ReadDir(cur)
							for _, ce := range ents {
								if _, err := os.Lstat(filepath.Join(p, ce.Name())); err != nil {
									added++
									fmt.Printf("%s %s\n", ui.Good("new    "), ui.HomeRel(filepath.Join(cur, ce.Name())))
								}
							}
						}
						return nil
					}
					if filesEqual(p, cur) {
						same++
						return nil
					}
					changed++
					fmt.Printf("%s %s\n", ui.Warn("changed"), ui.HomeRel(cur))
					if !noContent && budget > 0 {
						budget -= showUnified(p, cur, budget)
					}
					return nil
				})
			}
			for _, g := range s.Git {
				fmt.Printf("%s %s\n", ui.Accent("git    "), describeGit(g))
			}
			fmt.Println()
			fmt.Printf("%d missing, %d changed, %d new, %d unchanged\n", missing, changed, added, same)
			return nil
		},
	}
	c.Flags().BoolVar(&noContent, "names-only", false, "list paths without showing content changes")
	return c
}

func filesEqual(a, b string) bool {
	x, err1 := os.Lstat(a)
	y, err2 := os.Lstat(b)
	if err1 != nil || err2 != nil || x.Mode().Type() != y.Mode().Type() {
		return false
	}
	if x.Mode()&os.ModeSymlink != 0 {
		l1, _ := os.Readlink(a)
		l2, _ := os.Readlink(b)
		return l1 == l2
	}
	if x.Size() != y.Size() {
		return false
	}
	if x.Size() > 64<<20 {
		return x.ModTime().Equal(y.ModTime())
	}
	ba, _ := os.ReadFile(a)
	bb, _ := os.ReadFile(b)
	return bytes.Equal(ba, bb)
}

// showUnified prints a unified diff for small text files and returns the
// number of lines printed.
func showUnified(old, cur string, limit int) int {
	fi, err := os.Stat(old)
	if err != nil || fi.Size() > 1<<20 {
		return 0
	}
	b, _ := os.ReadFile(old)
	if bytes.IndexByte(b, 0) >= 0 {
		return 0
	}
	out, _ := exec.Command("diff", "-u", "--label", "snapshot", "--label", "now", old, cur).Output()
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) > limit {
		lines = append(lines[:limit], "...")
	}
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++"):
			fmt.Println("    " + ui.Good(l))
		case strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "---"):
			fmt.Println("    " + ui.Bad(l))
		default:
			fmt.Println("    " + ui.Dim(l))
		}
	}
	return len(lines)
}

func restoreCmd() *cobra.Command {
	var to string
	c := &cobra.Command{
		Use:   "restore <id> --to <dir>",
		Short: "Copy a snapshot's files into another directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if to == "" {
				return cli.UsageErr("oops restore copies a snapshot into another folder, so it needs --to. Try: oops restore %s --to ~/recovered (or oops undo %s to restore in place)", args[0], args[0])
			}
			id, err := parseID(args[0])
			if err != nil {
				return err
			}
			s, err := loadSnapshot(id)
			if err != nil {
				return err
			}
			dir, _ := filepath.Abs(to)
			outcomes, err := snap.RestoreTo(s, dir)
			printOutcomes(outcomes)
			if err != nil {
				return err
			}
			fmt.Println(ui.Good(fmt.Sprintf("Copied snapshot %d into %s.", id, ui.HomeRel(dir))))
			return nil
		},
	}
	c.Flags().StringVar(&to, "to", "", "directory to restore into")
	return c
}

func pruneCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "prune",
		Short: "Delete old snapshots (keeps 7 days and 5 GB by default)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := snap.LoadConfig()
			if all && ui.Interactive() && !ui.Confirm("Delete every snapshot?") {
				fmt.Println("Nothing changed.")
				return nil
			}
			res, err := snap.Prune(cfg, all)
			if err != nil {
				return err
			}
			n, total := snap.Usage()
			fmt.Printf("Removed %s (%s). %s left, %s.\n", ui.Plural(res.Removed, "snapshot"), ui.Bytes(res.Bytes), ui.Plural(n, "snapshot"), ui.Bytes(total))
			return nil
		},
	}
	c.Flags().BoolVar(&all, "all", false, "delete every snapshot")
	return c
}

func explainCmd() *cobra.Command {
	var cwd string
	c := &cobra.Command{
		Use:   "explain <command>",
		Short: "Show what Oopsy would save for a command, without running it",
		Example: `  oops explain rm -rf build
  oops explain git reset --hard`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cwd == "" {
				cwd, _ = os.Getwd()
			}
			explainLine(os.Stdout, strings.Join(args, " "), cwd)
			return nil
		},
	}
	c.Flags().StringVar(&cwd, "cwd", "", "directory the command would run in")
	c.Flags().SetInterspersed(false)
	return c
}

func configCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config [key value]",
		Short: "Show or change settings (keep_days, max_gb, copy_cap_gb, quiet)",
		Args:  cobra.RangeArgs(0, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			warnConfig()
			cfg := snap.LoadConfig()
			if len(args) == 0 {
				fmt.Printf("keep_days    %d\n", cfg.KeepDays)
				fmt.Printf("max_gb       %.1f\n", float64(cfg.MaxBytes)/(1<<30))
				fmt.Printf("copy_cap_gb  %.1f\n", float64(cfg.CopyCapBytes)/(1<<30))
				fmt.Printf("quiet        %v\n", cfg.Quiet)
				fmt.Println(ui.Dim("Stored in " + ui.HomeRel(snap.ConfigPath())))
				return nil
			}
			if len(args) != 2 {
				return cli.UsageErr("oops config %s needs a value too. Try: oops config keep_days 14, or oops config with nothing to see every setting", args[0])
			}
			v := args[1]
			f, ferr := strconv.ParseFloat(v, 64)
			switch args[0] {
			case "keep_days":
				if ferr != nil || f < 1 {
					return cli.UsageErr("keep_days must be a number of days, for example: oops config keep_days 14")
				}
				cfg.KeepDays = int(f)
			case "max_gb":
				if ferr != nil || f <= 0 {
					return cli.UsageErr("max_gb must be a positive number, for example: oops config max_gb 10")
				}
				cfg.MaxBytes = int64(f * (1 << 30))
			case "copy_cap_gb":
				if ferr != nil || f <= 0 {
					return cli.UsageErr("copy_cap_gb must be a positive number, for example: oops config copy_cap_gb 2")
				}
				cfg.CopyCapBytes = int64(f * (1 << 30))
			case "quiet":
				cfg.Quiet = v == "true" || v == "1" || v == "yes"
			default:
				return cli.UsageErr("%q is not a setting. The settings are keep_days, max_gb, copy_cap_gb and quiet; oops config shows them all", args[0])
			}
			if err := snap.SaveConfig(cfg); err != nil {
				return err
			}
			fmt.Printf("%s %s = %s\n", ui.Good("saved"), args[0], v)
			return nil
		},
	}
	return c
}
