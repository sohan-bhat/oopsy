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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	"github.com/sohan-bhat/oopsy/internal/cli"
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
	cli.FriendlyArgs(root)
	root.SetFlagErrorFunc(cli.FlagError)
	if err := root.Execute(); err != nil {
		ui.Errorf("%s", ui.Explain(err))
		var nf *pgproxy.NotFoundError
		if errors.As(err, &nf) {
			os.Exit(cli.ExitNotFound)
		}
		os.Exit(cli.ExitCode(err))
	}
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
		Long: `oops db proxy sits between your app or agent and Postgres. Before DROP TABLE,
DROP SCHEMA, TRUNCATE, DELETE, UPDATE or ALTER TABLE ... DROP COLUMN runs, it
copies exactly the affected data into an "oops" schema inside the same
transaction. oops db undo puts it back.`,
	}
	c.AddCommand(dbProxyCmd(), dbWrapCmd(), dbListCmd(), dbUndoCmd(), dbPruneCmd())
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
	return "", cli.UsageErr("Oopsy does not know which database to use. Pass --url postgres://user:pass@host:5432/db, or set OOPS_DATABASE_URL")
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
		return nil, fmt.Errorf("could not prepare the database: %w", dbError(err, upstream))
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
	conn, err := pgx.Connect(ctx, u)
	return conn, dbError(err, u)
}

func kindLabel(k string) string {
	return map[string]string{
		"delete": "DELETE", "update": "UPDATE", "update_full": "UPDATE", "truncate": "TRUNCATE",
		"drop_table": "DROP TABLE", "drop_schema": "DROP SCHEMA", "drop_column": "DROP COLUMN",
		"drop_view": "DROP VIEW", "drop_matview": "DROP MAT VIEW", "drop_function": "DROP FUNCTION",
	}[k]
}

func dbListCmd() *cobra.Command {
	var u string
	var n int
	c := &cobra.Command{
		Use:   "list",
		Short: "Show recent destructive statements that can be undone",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			conn, err := connect(u)
			if err != nil {
				return err
			}
			defer conn.Close(context.Background())
			list, err := pgproxy.ListChanges(context.Background(), conn, n)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				fmt.Println("No database snapshots yet. Run your app or agent through oops db proxy or oops db wrap.")
				return nil
			}
			fmt.Printf("%s  %s  %s  %s  %s\n", ui.Dim(pad("ID", 5)), ui.Dim(pad("WHEN", 11)), ui.Dim(pad("KIND", 12)), ui.Dim(pad("ROWS", 8)), ui.Dim("TARGET"))
			for _, ch := range list {
				id := pad(strconv.FormatInt(ch.ID, 10), 5)
				if ch.Restored == nil {
					id = ui.Accent(id)
				} else {
					id = ui.Dim(id)
				}
				line := fmt.Sprintf("%s  %s  %s  %s  %s", id, pad(ago(ch.Created), 11), pad(kindLabel(ch.Kind()), 12), pad(strconv.FormatInt(ch.Rows(), 10), 8), strings.Join(ch.Tables(), ", "))
				if ch.Restored != nil {
					line += " " + ui.Dim("(restored)")
				}
				fmt.Println(line)
				fmt.Printf("       %s\n", ui.Dim(ui.Shorten(ch.Statement, 90)))
			}
			return nil
		},
	}
	c.Flags().StringVar(&u, "url", "", "database URL (defaults to the last proxied upstream)")
	c.Flags().IntVarP(&n, "number", "n", 20, "how many to show")
	return c
}

func dbUndoCmd() *cobra.Command {
	var u string
	var yes bool
	c := &cobra.Command{
		Use:   "undo [id]",
		Short: "Restore the data a statement destroyed (latest by default)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var id int64
			if len(args) == 1 {
				v, err := strconv.ParseInt(strings.TrimPrefix(args[0], "#"), 10, 64)
				if err != nil {
					return cli.UsageErr("%q is not a change id. oops db list shows them; then run for example: oops db undo 42", args[0])
				}
				id = v
			}
			conn, err := connect(u)
			if err != nil {
				return err
			}
			ctx := context.Background()
			defer conn.Close(ctx)
			ch, err := pgproxy.FindChange(ctx, conn, id)
			if err != nil {
				return err
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
			if !yes {
				if !ui.Interactive() {
					return errors.New("no terminal to confirm on; run with -y to restore without asking")
				}
				if !ui.Confirm("Restore it?") {
					fmt.Println("Nothing changed.")
					return nil
				}
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

func dbPruneCmd() *cobra.Command {
	var u string
	var days int
	c := &cobra.Command{
		Use:   "prune",
		Short: "Drop database snapshots that were restored or are older than --days",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			conn, err := connect(u)
			if err != nil {
				return err
			}
			defer conn.Close(context.Background())
			n, err := pgproxy.Prune(context.Background(), conn, time.Duration(days)*24*time.Hour, true)
			if err != nil {
				return err
			}
			fmt.Printf("Dropped %s.\n", ui.Plural(n, "snapshot"))
			return nil
		},
	}
	c.Flags().StringVar(&u, "url", "", "database URL (defaults to the last proxied upstream)")
	c.Flags().IntVar(&days, "days", 7, "drop snapshots older than this many days")
	return c
}
