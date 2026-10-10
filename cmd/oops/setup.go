package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sohan-bhat/oopsy/internal/hooks"
	"github.com/sohan-bhat/oopsy/internal/snap"
	"github.com/sohan-bhat/oopsy/internal/ui"
	"github.com/sohan-bhat/oopsy/internal/watch"
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
		Long: `Installs preexec hooks for zsh, bash and fish, and hooks before and after each
shell command and file edit for Claude Code, Codex CLI, Cursor and Gemini CLI.
Only tools found on this machine
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
					fmt.Printf("  %s %-12s %s\n", ui.Bad("failed"), t.Label, ui.Explain(err))
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
				if runtime.GOOS == "windows" {
					fmt.Println("Open a new PowerShell or Git Bash window so it picks up the hook.")
				} else {
					sh := filepath.Base(os.Getenv("SHELL"))
					fmt.Printf("Open a new terminal (or run %s) so your shell picks up the hook.\n", ui.Bold("exec "+sh))
				}
			}
			if hint := executionPolicyHint(); hint != "" && slices.ContainsFunc(shells, isPowerShell) {
				fmt.Println(ui.Warn(hint))
			}
			if codex {
				fmt.Println("Codex asks you to trust new hooks once: run /hooks inside Codex and approve oopsy.")
			}
			fmt.Printf("Try it: %s then %s\n", ui.Bold("oops explain rm -rf build"), ui.Bold("oops doctor"))
			return nil
		},
	}
	c.Flags().StringSliceVar(&only, "only", nil, "only these targets: zsh, bash, fish, powershell, windowspowershell, claude, codex, cursor, gemini, shells, agents")
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
					fmt.Printf("  %s %-12s %s\n", ui.Bad("failed"), t.Label, ui.Explain(err))
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
			if watch.Installed() {
				if _, err := watch.Off(); err != nil {
					fmt.Printf("  %s %-12s %v\n", ui.Bad("failed"), "watch", err)
				} else {
					fmt.Printf("  %s %-12s %s\n", ui.Good("removed"), "watch", "background service")
				}
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
		Use:       "shell-init <zsh|bash|fish|powershell>",
		Short:     "Print the shell hook (used by the line oops init adds to your rc file)",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"zsh", "bash", "fish", "powershell"},
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
	cfg := snap.LoadConfig()
	limit := "while the disk keeps its free space floor"
	if sp, err := snap.SpaceAt(snap.Dir(), cfg); err == nil {
		limit = fmt.Sprintf("while the disk keeps %s free (%s free now)", ui.Bytes(sp.Floor), ui.Bytes(sp.Free))
	}
	switch probeClone() {
	case "clone":
		return "APFS clones (instant, copy on write)"
	case "reflink":
		return "reflinks (instant, copy on write)"
	case "block clone":
		return "ReFS block clones (instant, copy on write)"
	}
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("hard links for deletes (instant, no extra disk), NTFS compressed copies for overwrites %s", limit)
	}
	return fmt.Sprintf("hard links for deletes (instant, no extra disk), plain copies for overwrites %s", limit)
}

func isPowerShell(name string) bool { return name == "powershell" || name == "windowspowershell" }

// executionPolicyHint explains how to let PowerShell load its profile when
// the execution policy blocks scripts, as it does by default on Windows
// client editions.
func executionPolicyHint() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	out, err := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "Get-ExecutionPolicy").Output()
	if err != nil {
		return ""
	}
	switch p := strings.TrimSpace(string(out)); p {
	case "Restricted", "AllSigned":
		return "PowerShell's execution policy (" + p + ") stops profiles from loading. To allow it, run: Set-ExecutionPolicy -Scope CurrentUser RemoteSigned"
	}
	return ""
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
			if p := snap.ConfigProblem(); p != "" {
				bad("settings", p)
			}
			if err := os.MkdirAll(snap.Dir(), 0o700); err != nil {
				bad("storage", ui.Explain(err))
			} else {
				ok("storage", ui.HomeRel(snap.Dir()))
			}
			switch k := probeClone(); k {
			case "clone", "reflink", "block clone":
				ok("copy on write", storageKind())
			default:
				if runtime.GOOS == "windows" {
					ok("copy on write", "none on this volume (NTFS), so it uses "+storageKind())
				} else {
					warnf("copy on write", storageKind())
				}
			}
			if cwd, err := os.Getwd(); err == nil && !snap.SameDevice(cwd, snap.Dir()) {
				ok("this directory", "is on another volume than "+ui.HomeRel(snap.Dir())+"; hard links and clones go to a private store at that volume's root")
			}
			for _, t := range hooks.Targets() {
				if t.Installed && isPowerShell(t.Name) {
					if hint := executionPolicyHint(); hint != "" {
						warnf("PowerShell", hint)
					}
					break
				}
			}
			if _, err := exec.LookPath("git"); err != nil {
				warnf("git", "not found; git snapshots are disabled")
			} else {
				ok("git", "found")
			}
			fast, slow := hookSpeed()
			label := fmt.Sprintf("median %.1f ms for a harmless command, %.1f ms for git status", fast, slow)
			if fast > 50 {
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
