package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/sohan-bhat/oopsy/internal/snap"
	"github.com/sohan-bhat/oopsy/internal/ui"
)

func mirrorCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "mirror",
		Short: "Show or build the project mirrors that let oops undo any command",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			list := snap.Mirrors()
			if len(list) == 0 {
				fmt.Println("No project mirrors yet. Oopsy makes one the first time you run a command in a project.")
				return nil
			}
			for _, m := range list {
				fmt.Printf("%s  %s, %s, index %s, synced %s\n", ui.Bold(ui.HomeRel(m.Root)), ui.Plural(m.Files, "file"), m.Method, ui.Bytes(m.Bytes), ago(m.Synced))
			}
			return nil
		},
	}
	build := &cobra.Command{
		Use:   "build [folder]",
		Short: "Mirror a project now (oops does this by itself the first time)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, _ := os.Getwd()
			if len(args) == 1 {
				dir, _ = filepath.Abs(args[0])
			}
			root := snap.MirrorRoot(dir)
			if root == "" {
				return fmt.Errorf("%s is too broad to mirror (a home folder, a disk root or a system folder); run it inside a project", ui.HomeRel(dir))
			}
			res, err := snap.BuildMirror(root)
			if err != nil {
				return err
			}
			fmt.Printf("Mirrored %s: %s in %.0f ms.\n", ui.HomeRel(root), ui.Plural(res.Files, "file"), res.Millis)
			return nil
		},
	}
	c.AddCommand(build)
	return c
}
