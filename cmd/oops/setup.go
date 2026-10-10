package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sohan-bhat/oopsy/internal/hooks"
	"github.com/sohan-bhat/oopsy/internal/snap"
	"github.com/sohan-bhat/oopsy/internal/ui"
)

func anyHookInstalled() bool {
	for _, t := range hooks.Targets() {
		if t.Installed {
			return true
		}
	}
	return false
}

func pick(only []string, all bool) []hooks.Target {
	want := map[string]bool{}
	for _, o := range only {
		for _, x := range strings.Split(o, ",") {
			want[strings.TrimSpace(strings.ToLower(x))] = true
		}
	}
	var out []hooks.Target
	for _, t := range hooks.Targets() {
		switch {
		case len(want) > 0:
			if want[t.Name] || want[t.Kind+"s"] || want[t.Kind] {
				out = append(out, t)
			}
		case all || t.Detected:
			out = append(out, t)
		}
	}
	return out
}

func initCmd() *cobra.Command {
	var only []string
	var all bool
	c := &cobra.Command{
		Use:   "init",
		Short: "Install the shell and AI agent hooks",
		Long: `Installs preexec hooks for zsh, bash and fish, and PreToolUse style hooks for
Claude Code, Codex CLI, Cursor and Gemini CLI. Only tools found on this machine
are set up unless you pass --all or --only. Every file is backed up to
~/.oops/backups first, and oops uninstall reverses it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			bin := hooks.Bin()
			targets := pick(only, all)
			if len(targets) == 0 {
				return fmt.Errorf("no supported shells or agents found; use --only zsh,claude to choose")
			}
			fmt.Println(ui.Accent("Setting up Oopsy"))
			var shells []string
			codex := false
			for _, t := range targets {
				bk, err := hooks.Install(t, bin)
				if err != nil {
					fmt.Printf("  %s %-12s %v\n", ui.Bad("failed"), t.Label, err)
					continue
				}
				detail := ui.HomeRel(t.Path)
				if bk != "" {
					detail += ui.Dim("  (backup: " + ui.HomeRel(bk) + ")")
				}
				state := ui.Good("added ")
				if bk == "" && t.Installed && t.Kind == "shell" {
					state = ui.Dim("exists")
				}
				fmt.Printf("  %s %-12s %s\n", state, t.Label, detail)
				if t.Kind == "shell" {
					shells = append(shells, t.Name)
				}
				if t.Name == "codex" {
					codex = true
				}
			}
			_ = os.MkdirAll(snap.Dir(), 0o700)
			fmt.Println()
			if len(shells) > 0 {
				sh := filepath.Base(os.Getenv("SHELL"))
				fmt.Printf("Open a new terminal (or run %s) so your shell picks up the hook.\n", ui.Bold("exec "+sh))
			}
			if codex {
				fmt.Println("Codex asks you to trust new hooks once: run /hooks inside Codex and approve oopsy.")
			}
			fmt.Printf("Try it: %s then %s\n", ui.Bold("oops explain rm -rf build"), ui.Bold("oops doctor"))
			return nil
		},
	}
	c.Flags().StringSliceVar(&only, "only", nil, "only these targets: zsh, bash, fish, claude, codex, cursor, gemini, shells, agents")
	c.Flags().BoolVar(&all, "all", false, "set up every target, even ones not detected")
	return c
}

func uninstallCmd() *cobra.Command {
	var purge bool
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove every hook Oopsy installed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			removed := 0
			for _, t := range hooks.Targets() {
				if !t.Installed {
					continue
				}
				bk, err := hooks.Uninstall(t)
				if err != nil {
					fmt.Printf("  %s %-12s %v\n", ui.Bad("failed"), t.Label, err)
					continue
				}
				removed++
				detail := ui.HomeRel(t.Path)
				if bk != "" {
					detail += ui.Dim("  (backup: " + ui.HomeRel(bk) + ")")
				}
				fmt.Printf("  %s %-12s %s\n", ui.Good("removed"), t.Label, detail)
			}
			if removed == 0 {
				fmt.Println("No Oopsy hooks are installed.")
			}
			if purge {
				if err := os.RemoveAll(filepath.Join(snap.Dir(), "snapshots")); err != nil {
					_, _ = snap.Prune(snap.LoadConfig(), true)
				}
				_ = os.RemoveAll(snap.Dir())
				fmt.Printf("  %s %s\n", ui.Good("deleted"), ui.HomeRel(snap.Dir()))
			} else {
				n, total := snap.Usage()
				if n > 0 {
					fmt.Println(ui.Dim(fmt.Sprintf("Kept %s (%s) in %s. Run oops uninstall --purge to delete them.", ui.Plural(n, "snapshot"), ui.Bytes(total), ui.HomeRel(snap.Dir()))))
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&purge, "purge", false, "also delete all snapshots and settings in ~/.oops")
	return c
}

func shellInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:       "shell-init <zsh|bash|fish>",
		Short:     "Print the shell hook (used by the line oops init adds to your rc file)",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"zsh", "bash", "fish"},
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := hooks.ShellInit(args[0], hooks.Bin())
			if err != nil {
				return err
			}
			fmt.Print(s)
			return nil
		},
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show which hooks are active and how much is stored",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println(ui.Accent("Hooks"))
			for _, t := range hooks.Targets() {
				if !t.Detected && !t.Installed {
					continue
				}
				state := ui.Dim("not installed")
				if t.Installed {
					state = ui.Good("on")
				}
				fmt.Printf("  %-12s %s\n", t.Label, state)
			}
			cfg := snap.LoadConfig()
			list, _ := snap.List()
			var total int64
			pending := 0
			for _, s := range list {
				total += s.Size
				if s.Restored == nil {
					pending++
				}
			}
			fmt.Println()
			fmt.Println(ui.Accent("Snapshots"))
			fmt.Printf("  %s, %s, stored in %s\n", ui.Plural(len(list), "snapshot"), ui.Bytes(total), ui.HomeRel(snap.Dir()))
			if len(list) > 0 {
				s := list[0]
				fmt.Printf("  latest: %d, %s, %s\n", s.ID, ago(s.Time), ui.Shorten(s.Command, 50))
			}
			fmt.Printf("  keeping %d days and up to %s\n", cfg.KeepDays, ui.Bytes(cfg.MaxBytes))
			fmt.Printf("  storage: %s\n", storageKind())
			return nil
		},
	}
}

func storageKind() string {
	switch probeClone() {
	case "clone":
		return "APFS clones (instant, copy on write)"
	case "reflink":
		return "reflinks (instant, copy on write)"
	default:
		return fmt.Sprintf("plain copies, capped at %s per snapshot (this filesystem has no copy on write)", ui.Bytes(snap.LoadConfig().CopyCapBytes))
	}
}

func probeClone() string {
	return snap.ProbeClone(snap.Dir())
}

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check hooks, filesystem support and hook speed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			problems := 0
			ok := func(label, detail string) { fmt.Printf("  %s %-22s %s\n", ui.Good("ok  "), label, ui.Dim(detail)) }
			warnf := func(label, detail string) {
				fmt.Printf("  %s %-22s %s\n", ui.Warn("warn"), label, detail)
				problems++
			}
			bad := func(label, detail string) {
				fmt.Printf("  %s %-22s %s\n", ui.Bad("fail"), label, detail)
				problems++
			}
			self, _ := os.Executable()
			ok("oops "+version, self)
			if p, err := exec.LookPath("oops"); err != nil {
				warnf("on PATH", "oops is not on your PATH; hooks use the absolute path "+hooks.Bin())
			} else {
				ok("on PATH", p)
			}
			anyHook := false
			for _, t := range hooks.Targets() {
				switch {
				case t.Installed:
					anyHook = true
					ok(t.Label+" hook", ui.HomeRel(t.Path))
				case t.Detected:
					warnf(t.Label+" hook", "not installed (run oops init)")
				}
			}
			if !anyHook {
				bad("hooks", "nothing is protected yet; run oops init")
			}
			if sh := os.Getenv("OOPSY_SHELL_HOOK"); sh != "" {
				ok("this shell", sh+" hook is loaded")
			} else if anyHook {
				warnf("this shell", "hook not loaded in this terminal; open a new one")
			}
			if err := os.MkdirAll(snap.Dir(), 0o700); err != nil {
				bad("storage", err.Error())
			} else {
				ok("storage", ui.HomeRel(snap.Dir()))
			}
			switch k := probeClone(); k {
			case "clone", "reflink":
				ok("copy on write", storageKind())
			default:
				warnf("copy on write", storageKind())
			}
			if cwd, err := os.Getwd(); err == nil && !snap.SameDevice(cwd, snap.Dir()) {
				warnf("this directory", "is on a different volume than ~/.oops, so snapshots here are plain copies (capped)")
			}
			if _, err := exec.LookPath("git"); err != nil {
				warnf("git", "not found; git snapshots are disabled")
			} else {
				ok("git", "found")
			}
			fast, slow := hookSpeed()
			label := fmt.Sprintf("median %.1f ms for a harmless command, %.1f ms for git status", fast, slow)
			if fast > 20 {
				warnf("hook speed", label)
			} else {
				ok("hook speed", label)
			}
			fmt.Println()
			if problems == 0 {
				fmt.Println(ui.Good("Everything looks good."))
			} else {
				fmt.Printf("%s to look at.\n", ui.Plural(problems, "thing"))
			}
			return nil
		},
	}
}

// hookSpeed measures the full hook process for commands that pass the shell
// prefilter but are not destructive.
func hookSpeed() (float64, float64) {
	self, err := os.Executable()
	if err != nil {
		return 0, 0
	}
	measure := func(command string) float64 {
		var ds []float64
		for i := 0; i < 15; i++ {
			start := time.Now()
			c := exec.Command(self, "hook", "zsh", "--", command)
			_ = c.Run()
			ds = append(ds, float64(time.Since(start).Microseconds())/1000)
		}
		sort.Float64s(ds)
		return ds[len(ds)/2]
	}
	return measure("ls -la > /dev/null"), measure("git status")
}
