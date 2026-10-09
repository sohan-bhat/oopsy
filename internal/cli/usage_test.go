package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func tree() *cobra.Command {
	root := &cobra.Command{Use: "oops", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error { return nil }}
	root.AddCommand(&cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error { return nil }})
	ex := &cobra.Command{Use: "explain", Args: cobra.MinimumNArgs(1), RunE: func(*cobra.Command, []string) error { return nil }}
	root.AddCommand(ex)
	u := &cobra.Command{Use: "undo", Args: cobra.ExactArgs(1), RunE: func(*cobra.Command, []string) error { return nil }}
	u.Flags().BoolP("yes", "y", false, "")
	root.AddCommand(u)
	FriendlyArgs(root)
	root.SetFlagErrorFunc(FlagError)
	root.SilenceErrors, root.SilenceUsage = true, true
	return root
}

func run(args ...string) error {
	r := tree()
	r.SetArgs(args)
	return r.Execute()
}

func TestUsageMessages(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"explain"}, "oops explain needs a command to check. Try: oops explain rm -rf build"},
		{[]string{"lsit"}, "oops lsit is not a command. Did you mean oops list? Run oops --help to see them all."},
		{[]string{"undo"}, "oops undo needs the id of a snapshot (oops list shows them). Try: oops undo 12"},
		{[]string{"undo", "1", "--yse"}, "oops undo does not have a --yse option. Did you mean --yes? Run oops undo --help to see its options."},
		{[]string{"list", "extra"}, `oops list takes no arguments, but got "extra". Run oops list --help for how to use it.`},
	} {
		err := run(c.args...)
		if err == nil || err.Error() != c.want {
			t.Errorf("%v:\n got %v\nwant %s", c.args, err, c.want)
		}
		if ExitCode(err) != ExitUsage {
			t.Errorf("%v: exit code %d", c.args, ExitCode(err))
		}
		if strings.ContainsAny(err.Error(), "\u2013\u2014") {
			t.Errorf("dash in %q", err)
		}
	}
	if ExitCode(errors.New("x")) != ExitFailed || ExitCode(NotFound("no")) != ExitNotFound {
		t.Error("exit codes")
	}
}
