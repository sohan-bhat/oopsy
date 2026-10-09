// Command oops-db is the Postgres half of Oopsy. It runs as "oops db".
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	"github.com/sohan-bhat/oopsy/internal/pgproxy"
	"github.com/sohan-bhat/oopsy/internal/snap"
	"github.com/sohan-bhat/oopsy/internal/ui"
)

var version = "dev"

func main() {
	root := dbCmd()
	root.Version = version
	root.SetVersionTemplate("oops-db {{.Version}}\n")
	root.SilenceUsage = true
	root.SilenceErrors = true
	if err := root.Execute(); err != nil {
		ui.Errorf("%v", err)
		os.Exit(1)
	}
}

func warnf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", ui.ErrWarn("warn"), fmt.Sprintf(format, a...))
}

func pad(s string, n int) string {
	if r := []rune(s); len(r) < n {
		return s + strings.Repeat(" ", n-len(r))
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

func dbCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "oops db",
		Short: "Undo destructive SQL on Postgres",
		Long: `oops db proxy sits between your app or agent and Postgres. Before a
destructive statement runs, it copies exactly what is about to be lost into
an "oops" schema inside the same transaction:

  DELETE, UPDATE, MERGE        the rows they touch
  INSERT ... ON CONFLICT       the rows an upsert overwrites
  TRUNCATE                     the whole table
  DROP TABLE, DROP SCHEMA      data, indexes, constraints and views on them
  ALTER TABLE ... DROP COLUMN  the column's values
  DROP VIEW, DROP FUNCTION     the definition (materialized views and
                               procedures too)
  DROP DATABASE                a pg_dump of it, kept in ~/.oops/db/dumps

oops db protect adds triggers to the database itself, so changes that skip
the proxy can be undone too. oops db undo puts things back.`,
	}
	c.AddCommand(dbProxyCmd(), dbWrapCmd(), dbListCmd(), dbUndoCmd(), dbPruneCmd(), dbProtectCmd(), dbUnprotectCmd())
	return c
}

type dbState struct {
	Upstream string `json:"upstream"`
}

func dbStatePath() string { return filepath.Join(snap.Dir(), "db.json") }

func rememberUpstream(u string) {
	_ = os.MkdirAll(snap.Dir(), 0o700)
	b, _ := json.Marshal(dbState{Upstream: u})
	_ = os.WriteFile(dbStatePath(), b, 0o600)
}

// resolveURL finds the database to talk to for list and undo.
func resolveURL(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if v := os.Getenv("OOPS_DATABASE_URL"); v != "" {
		return v, nil
	}
	if b, err := os.ReadFile(dbStatePath()); err == nil {
		var st dbState
		if json.Unmarshal(b, &st) == nil && st.Upstream != "" {
			return st.Upstream, nil
		}
	}
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v, nil
	}
	return "", errors.New("no database given; pass --url postgres://... or set OOPS_DATABASE_URL")
}

func redact(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.User == nil {
		return u
	}
	if _, ok := p.User.Password(); ok {
		p.User = url.UserPassword(p.User.Username(), "xxxxx")
	}
	return p.String()
}

func startProxy(upstream, listen string, logger *log.Logger) (*pgproxy.Proxy, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := pgproxy.Setup(ctx, upstream); err != nil {
		return nil, fmt.Errorf("could not prepare the database: %w", err)
	}
	p, err := pgproxy.New(upstream, logger)
	if err != nil {
		return nil, err
	}
	if err := p.Listen(listen); err != nil {
		return nil, err
	}
	// Warm up the SQL parser so the first query is not slow.
	go pgproxy.Plan("DELETE FROM warmup")
	rememberUpstream(upstream)
	return p, nil
}

// proxyURL is the upstream URL with host and port replaced by the proxy.
func proxyURL(upstream string, addr net.Addr) (string, error) {
	u, err := url.Parse(upstream)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", fmt.Errorf("upstream must be a postgres:// URL")
	}
	u.Host = addr.String()
	q := u.Query()
	q.Set("sslmode", "disable")
	q.Del("host")
	q.Del("port")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func dbProxyCmd() *cobra.Command {
	var upstream, listen string
	c := &cobra.Command{
		Use:   "proxy --upstream postgres://...",
		Short: "Run the snapshotting Postgres proxy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if upstream == "" {
				upstream = os.Getenv("DATABASE_URL")
			}
			if upstream == "" {
				return errors.New("--upstream postgres://user:pass@host:5432/db is required")
			}
			logger := log.New(os.Stderr, "oops db: ", log.Ltime)
			p, err := startProxy(upstream, listen, logger)
			if err != nil {
				return err
			}
			pu, _ := proxyURL(upstream, p.Addr())
			fmt.Fprintf(os.Stderr, "%s listening on %s, forwarding to %s\n", ui.ErrAccent("Oopsy proxy"), p.Addr(), redact(upstream))
			fmt.Fprintf(os.Stderr, "Point your app or agent at: %s\n", redact(pu))
			fmt.Fprintln(os.Stderr, ui.ErrDim("Undo with oops db undo. Stop with Ctrl+C."))
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			go func() { <-sig; p.Close() }()
			return p.Serve()
		},
	}
	c.Flags().StringVar(&upstream, "upstream", "", "the real database URL (defaults to $DATABASE_URL)")
	c.Flags().StringVar(&listen, "listen", "127.0.0.1:6543", "address to listen on")
	return c
}

func dbWrapCmd() *cobra.Command {
	var upstream string
	c := &cobra.Command{
		Use:   "wrap [--upstream url] -- <command> [args...]",
		Short: "Run a command with DATABASE_URL pointed at a temporary proxy",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if upstream == "" {
				upstream = os.Getenv("DATABASE_URL")
			}
			if upstream == "" {
				return errors.New("set DATABASE_URL or pass --upstream")
			}
			logger := log.New(os.Stderr, "oops db: ", log.Ltime)
			p, err := startProxy(upstream, "127.0.0.1:0", logger)
			if err != nil {
				return err
			}
			go p.Serve()
			defer p.Close()
			pu, err := proxyURL(upstream, p.Addr())
			if err != nil {
				return err
			}
			child := exec.Command(args[0], args[1:]...)
			child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
			host, port, _ := net.SplitHostPort(p.Addr().String())
			env := append(os.Environ(), "DATABASE_URL="+pu, "PGHOST="+host, "PGPORT="+port, "PGSSLMODE=disable")
			if u, err := url.Parse(upstream); err == nil {
				if u.User != nil {
					env = append(env, "PGUSER="+u.User.Username())
					if pw, ok := u.User.Password(); ok {
						env = append(env, "PGPASSWORD="+pw)
					}
				}
				if db := strings.TrimPrefix(u.Path, "/"); db != "" {
					env = append(env, "PGDATABASE="+db)
				}
			}
			child.Env = env
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(sig)
			go func() {
				for s := range sig {
					if child.Process != nil {
						child.Process.Signal(s)
					}
				}
			}()
			err = child.Run()
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				p.Close()
				os.Exit(ee.ExitCode())
			}
			return err
		},
	}
	c.Flags().StringVar(&upstream, "upstream", "", "the real database URL (defaults to $DATABASE_URL)")
	return c
}

func connect(urlFlag string) (*pgx.Conn, error) {
	u, err := resolveURL(urlFlag)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return pgx.Connect(ctx, u)
}

func kindLabel(k string) string {
	return map[string]string{
		"delete": "DELETE", "update": "UPDATE", "update_full": "UPDATE", "truncate": "TRUNCATE",
		"drop_table": "DROP TABLE", "drop_schema": "DROP SCHEMA", "drop_column": "DROP COLUMN",
		"drop_view": "DROP VIEW", "drop_matview": "DROP MAT VIEW", "drop_function": "DROP FUNCTION",
		"upsert": "UPSERT", "audit_delete": "DELETE", "audit_update": "UPDATE",
	}[k]
}

// listRow is one line of oops db list: a change journaled in the database
// or a dump taken before DROP DATABASE.
type listRow struct {
	id, kind, rows, target, stmt string
	created                      time.Time
	restored                     bool
}

func dbListCmd() *cobra.Command {
	var u string
	var n int
	c := &cobra.Command{
		Use:   "list",
		Short: "Show recent destructive statements that can be undone",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			dumps, derr := pgproxy.ListDumps()
			if derr != nil {
				warnf("could not read the database dumps: %v", derr)
			}
			var rows []listRow
			conn, err := connect(u)
			if err != nil {
				if len(dumps) == 0 {
					return err
				}
				warnf("could not reach the database, showing dumps only: %v", err)
			} else {
				defer conn.Close(ctx)
				pgproxy.PruneAudit(ctx, conn)
				list, err := pgproxy.ListChanges(ctx, conn, n)
				if err != nil {
					return err
				}
				for _, ch := range list {
					rows = append(rows, listRow{id: strconv.FormatInt(ch.ID, 10), kind: kindLabel(ch.Kind()), rows: strconv.FormatInt(ch.Rows(), 10),
						target: strings.Join(ch.Tables(), ", "), stmt: ch.Statement, created: ch.Created, restored: ch.Restored != nil})
				}
			}
			for _, d := range dumps {
				target := fmt.Sprintf("%s on %s (%s dump)", d.Database, d.Server, pgproxy.HumanBytes(d.Size))
				rows = append(rows, listRow{id: d.Label(), kind: "DROP DATABASE", target: target,
					stmt: d.Statement, created: d.Created, restored: d.Restored != nil})
			}
			sort.SliceStable(rows, func(a, b int) bool { return rows[a].created.After(rows[b].created) })
			if len(rows) > n {
				rows = rows[:n]
			}
			if len(rows) == 0 {
				fmt.Println("No database snapshots yet. Run your app or agent through oops db proxy or oops db wrap, or turn on oops db protect.")
				return nil
			}
			fmt.Printf("%s  %s  %s  %s  %s\n", ui.Dim(pad("ID", 5)), ui.Dim(pad("WHEN", 11)), ui.Dim(pad("KIND", 13)), ui.Dim(pad("ROWS", 8)), ui.Dim("TARGET"))
			for _, r := range rows {
				id := pad(r.id, 5)
				if r.restored {
					id = ui.Dim(id)
				} else {
					id = ui.Accent(id)
				}
				line := fmt.Sprintf("%s  %s  %s  %s  %s", id, pad(ago(r.created), 11), pad(r.kind, 13), pad(r.rows, 8), r.target)
				if r.restored {
					line += " " + ui.Dim("(restored)")
				}
				fmt.Println(line)
				fmt.Printf("       %s\n", ui.Dim(ui.Shorten(r.stmt, 90)))
			}
			return nil
		},
	}
	c.Flags().StringVar(&u, "url", "", "database URL (defaults to the last proxied upstream)")
	c.Flags().IntVarP(&n, "number", "n", 20, "how many to show")
	return c
}

func confirmRestore(yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	if !ui.Interactive() {
		return false, errors.New("no terminal to confirm on; run with -y to restore without asking")
	}
	if !ui.Confirm("Restore it?") {
		fmt.Println("Nothing changed.")
		return false, nil
	}
	return true, nil
}

func undoDump(d *pgproxy.Dump, yes bool) error {
	fmt.Printf("%s  %s\n", ui.Accent("Database dump "+d.Label()), ui.Dim(ago(d.Created)))
	fmt.Printf("  %s %s\n\n", ui.Dim(">"), ui.Shorten(d.Statement, 200))
	if d.Restored != nil {
		fmt.Println(ui.Warn("This database was already restored " + ago(*d.Restored) + "."))
	}
	fmt.Println("This will:")
	fmt.Printf("  create database %s on %s again and restore it from the %s dump taken before it was dropped\n\n", d.Database, d.Server, pgproxy.HumanBytes(d.Size))
	if ok, err := confirmRestore(yes); !ok || err != nil {
		return err
	}
	line, err := pgproxy.RestoreDump(context.Background(), *d)
	if err != nil {
		return fmt.Errorf("undo failed: %w", err)
	}
	fmt.Printf("  %s %s\n", ui.Good("done"), line)
	fmt.Println(ui.Good(fmt.Sprintf("Restored database dump %s.", d.Label())))
	return nil
}

func dbUndoCmd() *cobra.Command {
	var u string
	var yes bool
	c := &cobra.Command{
		Use:   "undo [id]",
		Short: "Restore what a statement destroyed (latest by default; d1, d2 are database dumps)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var id int64
			if len(args) == 1 {
				arg := strings.TrimPrefix(args[0], "#")
				if strings.HasPrefix(strings.ToLower(arg), "d") {
					d, err := pgproxy.FindDump(arg)
					if err != nil {
						return err
					}
					return undoDump(d, yes)
				}
				v, err := strconv.ParseInt(arg, 10, 64)
				if err != nil {
					return fmt.Errorf("%q is not an id (see oops db list)", args[0])
				}
				id = v
			}
			// With no id, the newest unrestored change wins, whether it lives
			// in the database or is a dump taken before DROP DATABASE.
			var newestDump *pgproxy.Dump
			if id == 0 {
				dumps, _ := pgproxy.ListDumps()
				for i := range dumps {
					if dumps[i].Restored == nil {
						newestDump = &dumps[i]
						break
					}
				}
			}
			conn, err := connect(u)
			if err != nil {
				if newestDump != nil {
					return undoDump(newestDump, yes)
				}
				return err
			}
			ctx := context.Background()
			defer conn.Close(ctx)
			pgproxy.PruneAudit(ctx, conn)
			ch, err := pgproxy.FindChange(ctx, conn, id)
			if err != nil {
				return err
			}
			if newestDump != nil && (ch == nil || newestDump.Created.After(ch.Created)) {
				return undoDump(newestDump, yes)
			}
			if ch == nil {
				fmt.Println("Nothing to undo.")
				return nil
			}
			fmt.Printf("%s  %s\n", ui.Accent(fmt.Sprintf("Database change %d", ch.ID)), ui.Dim(ago(ch.Created)))
			fmt.Printf("  %s %s\n\n", ui.Dim(">"), ui.Shorten(ch.Statement, 200))
			if ch.Restored != nil {
				fmt.Println(ui.Warn("This change was already restored " + ago(*ch.Restored) + "."))
			}
			fmt.Println("This will:")
			for _, l := range pgproxy.Describe(*ch) {
				fmt.Println("  " + l)
			}
			fmt.Println()
			if ok, err := confirmRestore(yes); !ok || err != nil {
				return err
			}
			res, err := pgproxy.Undo(ctx, conn, *ch)
			if err != nil {
				return fmt.Errorf("undo failed, nothing was changed: %w", err)
			}
			for _, l := range res.Lines {
				fmt.Printf("  %s %s\n", ui.Good("done"), l)
			}
			for _, w := range res.Warnings {
				fmt.Printf("  %s %s\n", ui.Warn("warn"), w)
			}
			fmt.Println(ui.Good(fmt.Sprintf("Restored database change %d.", ch.ID)))
			return nil
		},
	}
	c.Flags().StringVar(&u, "url", "", "database URL (defaults to the last proxied upstream)")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "restore without asking")
	return c
}

func dbProtectCmd() *cobra.Command {
	var u string
	c := &cobra.Command{
		Use:   "protect",
		Short: "Add triggers so changes made without the proxy can be undone too",
		Long: `oops db protect installs triggers in the database itself, for changes that do
not go through the proxy: a psql session, a migration tool, a cron job.

Before DELETE, UPDATE and TRUNCATE on any table (and tables created later),
the old rows are kept in the oops schema. Before DROP TABLE, DROP VIEW, DROP
MATERIALIZED VIEW, DROP FUNCTION, DROP PROCEDURE and DROP SCHEMA, the data or
definition is saved. Each transaction becomes one entry in oops db list, and
oops db undo reverses it. Saved changes are kept for 7 days.

It needs a superuser connection, adds some write overhead, and does not cover
INSERT, COPY or DROP DATABASE. oops db unprotect removes the triggers again.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			url, err := resolveURL(u)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			n, err := pgproxy.Protect(ctx, url)
			if err != nil {
				return err
			}
			rememberUpstream(url)
			fmt.Printf("%s Protected %s. Tables created later will be protected too.\n", ui.Good("Done."), ui.Plural(n, "table"))
			fmt.Println(ui.Dim("Changes made without the proxy now show up in oops db list. Turn it off with oops db unprotect."))
			return nil
		},
	}
	c.Flags().StringVar(&u, "url", "", "database URL (defaults to the last proxied upstream)")
	return c
}

func dbUnprotectCmd() *cobra.Command {
	var u string
	c := &cobra.Command{
		Use:   "unprotect",
		Short: "Remove the triggers oops db protect added",
		Long: `oops db unprotect removes the triggers, event triggers and functions that
oops db protect added. Changes saved so far stay in the oops schema and can
still be undone until oops db prune drops them.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			url, err := resolveURL(u)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := pgproxy.Unprotect(ctx, url); err != nil {
				return err
			}
			fmt.Println(ui.Good("Done.") + " The protect triggers are gone. Changes saved so far can still be undone.")
			return nil
		},
	}
	c.Flags().StringVar(&u, "url", "", "database URL (defaults to the last proxied upstream)")
	return c
}

func dbPruneCmd() *cobra.Command {
	var u string
	var days int
	c := &cobra.Command{
		Use:   "prune",
		Short: "Drop snapshots and database dumps that were restored or are older than --days",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			age := time.Duration(days) * 24 * time.Hour
			nd, err := pgproxy.PruneDumps(age, true)
			if err != nil {
				warnf("could not prune database dumps: %v", err)
			}
			conn, err := connect(u)
			if err != nil {
				if nd > 0 {
					fmt.Printf("Deleted %s.\n", ui.Plural(nd, "database dump"))
				}
				return err
			}
			defer conn.Close(context.Background())
			pgproxy.PruneAudit(context.Background(), conn)
			n, err := pgproxy.Prune(context.Background(), conn, age, true)
			if err != nil {
				return err
			}
			fmt.Printf("Dropped %s and deleted %s.\n", ui.Plural(n, "snapshot"), ui.Plural(nd, "database dump"))
			return nil
		},
	}
	c.Flags().StringVar(&u, "url", "", "database URL (defaults to the last proxied upstream)")
	c.Flags().IntVar(&days, "days", 7, "drop snapshots older than this many days")
	return c
}
