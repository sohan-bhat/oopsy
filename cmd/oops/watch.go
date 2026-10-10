package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sohan-bhat/oopsy/internal/snap"
	"github.com/sohan-bhat/oopsy/internal/ui"
	"github.com/sohan-bhat/oopsy/internal/watch"
)

func watchCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "watch",
		Short: "Keep the previous version of files you save (optional background mode)",
		Long: `oops watch keeps every project Oopsy mirrors up to date in the background.
When a file changes while no command is running (an editor save, an agent's
edit tool, a delete from a file manager or cmd.exe), its previous version is
kept as a watch snapshot. oops list --watch shows them and oops undo <id>
restores one.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return watchStatus(false) },
	}
	on := &cobra.Command{
		Use:   "on",
		Short: "Start watch mode now and at every login",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			started, note, err := watch.On()
			if err != nil {
				return fmt.Errorf("could not start watch mode: %v", err)
			}
			if note != "" {
				fmt.Println(ui.Warn("note") + " " + note)
			}
			st, ok := watch.WaitRunning(10 * time.Second)
			if !ok {
				return fmt.Errorf("watch mode was set up (%s) but has not reported in yet; check oops watch status in a moment", started.Manager)
			}
			fmt.Printf("%s watch mode is on (%s), watching %s.\n", ui.Good("Done:"), managerLabel(started.Manager), ui.Plural(len(st.Projects), "project"))
			if len(st.Projects) == 0 {
				fmt.Println(ui.Dim("Projects are added as Oopsy mirrors them, the first time you run a command in one."))
			}
			fmt.Println(ui.Dim(fmt.Sprintf("Edits are kept for files up to %d MB. oops list --watch shows them.", snap.WatchMaxFile()>>20)))
			return nil
		},
	}
	off := &cobra.Command{
		Use:   "off",
		Short: "Stop watch mode and remove its service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			had, err := watch.Off()
			if err != nil {
				return err
			}
			if !had {
				fmt.Println("Watch mode was not on.")
				return nil
			}
			fmt.Println(ui.Good("Done:") + " watch mode is off and its service is removed. Watch snapshots you already have are kept.")
			return nil
		},
	}
	var asJSON bool
	status := &cobra.Command{
		Use:   "status",
		Short: "Show whether watch mode runs and what it has done",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return watchStatus(asJSON) },
	}
	status.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	var home string
	runc := &cobra.Command{
		Use:    "run",
		Short:  "Run the watch daemon in the foreground",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if home != "" {
				os.Setenv("OOPS_HOME", home)
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return watch.Run(ctx, watch.Options{})
		},
	}
	runc.Flags().StringVar(&home, "home", "", "Oopsy store to use")
	c.AddCommand(on, off, status, runc)
	return c
}

func managerLabel(m string) string {
	switch m {
	case "launchd":
		return "a launchd agent"
	case "systemd":
		return "a systemd user service"
	case "task":
		return "a scheduled task at logon"
	case "pid":
		return "a background process"
	}
	return m
}

func watchStatus(asJSON bool) error {
	st, running := watch.Running()
	svc := watch.Service()
	if asJSON {
		out := map[string]any{"running": running, "status": st}
		if svc != nil {
			out["started_by"] = svc.Manager
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	if !running {
		if svc != nil {
			fmt.Printf("Watch mode is set up (%s) but not running. Run oops watch on to start it again.\n", managerLabel(svc.Manager))
		} else {
			fmt.Println("Watch mode is off. Run oops watch on to keep the previous version of files you save.")
		}
		return nil
	}
	how := "started by hand"
	if svc != nil {
		how = managerLabel(svc.Manager)
	}
	fmt.Printf("Watch mode is %s, %s, pid %d, up %s.\n", ui.Good("running"), how, st.PID, strings.TrimSuffix(ago(st.Started), " ago"))
	row := func(k, v string) { fmt.Printf("  %-10s %s\n", ui.Dim(k), v) }
	row("events", st.Mechanism)
	projects := ui.Plural(len(st.Projects), "project")
	if !st.Ready {
		projects += " (first sync in progress)"
	}
	row("projects", projects)
	for _, p := range st.Projects {
		fmt.Printf("  %-10s %s\n", "", ui.HomeRel(p))
	}
	if len(st.Polled) > 0 {
		row("polled", fmt.Sprintf("%s checked every 30 s (no events available)", ui.Plural(len(st.Polled), "project")))
	}
	row("handled", fmt.Sprintf("%d events, %d syncs, %s", st.Events, st.Syncs, ui.Plural(int(st.Snapshots), "snapshot")))
	if st.LastSnapshot > 0 {
		row("last", fmt.Sprintf("snapshot %d, saved %.0f ms after the change", st.LastSnapshot, st.LatencyMs))
	}
	row("memory", fmt.Sprintf("%s resident, %s Go heap", ui.Bytes(int64(st.RSS)), ui.Bytes(int64(st.Heap))))
	if st.BaseFiles > 0 {
		row("stored", fmt.Sprintf("%s kept compressed in %s, for in place writes on hard link disks", ui.Plural(st.BaseFiles, "file"), ui.Bytes(st.BaseBytes)))
	}
	for _, n := range st.Notes {
		row("note", n)
	}
	if st.LastError != "" {
		row("error", st.LastError)
	}
	return nil
}
