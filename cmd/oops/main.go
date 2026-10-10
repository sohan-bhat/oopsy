// Command oops makes destructive commands undoable.
package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/sohan-bhat/oopsy/internal/cli"
	"github.com/sohan-bhat/oopsy/internal/hooks"
	"github.com/sohan-bhat/oopsy/internal/ui"
)

var version = "dev"

func main() {
	// The hook runs before every risky command, so it skips all CLI setup.
	if len(os.Args) >= 3 && os.Args[1] == "hook" {
		args := os.Args[3:]
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		os.Exit(hooks.Run(os.Args[2], args, os.Stdin, os.Stdout, os.Stderr))
	}
	root := rootCmd()
	if err := root.Execute(); err != nil {
		fail("%s", ui.Explain(err))
		os.Exit(cli.ExitCode(err))
	}
}

func rootCmd() *cobra.Command {
	var yes bool
	root := &cobra.Command{
		Use:   "oops",
		Short: "Undo destructive commands, for people and AI coding agents",
		Long: `Oopsy takes an instant snapshot right before a destructive command runs
(rm -rf, git reset --hard, DROP TABLE and friends). Type oops to get it back.

Run oops init once to install the shell and agent hooks.

` + cli.ExitCodesHelp,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return undoLatest(yes)
		},
	}
	root.SetVersionTemplate("oops {{.Version}}\n")
	root.Flags().BoolVarP(&yes, "yes", "y", false, "restore without asking")
	root.AddCommand(listCmd(), undoCmd(), showCmd(), diffCmd(), restoreCmd(), statusCmd(), pruneCmd(),
		doctorCmd(), initCmd(), uninstallCmd(), shellInitCmd(), explainCmd(), configCmd(), dbCmd(), hookCmd(), mirrorCmd(), watchCmd())
	cli.FriendlyArgs(root)
	root.SetFlagErrorFunc(cli.FlagError)
	return root
}

func hookCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "hook <source> [-- command]",
		Short:  "Called by shell and agent hooks before a command runs",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			os.Exit(hooks.Run(args[0], args[1:], os.Stdin, os.Stdout, os.Stderr))
			return nil
		},
	}
}
