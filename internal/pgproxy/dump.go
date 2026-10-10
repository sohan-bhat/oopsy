package pgproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sohan-bhat/oopsy/internal/snap"
)

// DROP DATABASE cannot run inside a transaction and takes the oops schema
// with it, so the proxy saves the database with pg_dump first and journals
// the dump on local disk instead of in the database.

// Dump is one pg_dump taken right before DROP DATABASE.
type Dump struct {
	ID        int        `json:"id"`
	Database  string     `json:"database"`
	Server    string     `json:"server"`
	Upstream  string     `json:"upstream"`
	File      string     `json:"file"`
	Size      int64      `json:"size"`
	DBSize    int64      `json:"db_size"`
	Statement string     `json:"statement"`
	Created   time.Time  `json:"created"`
	Restored  *time.Time `json:"restored,omitempty"`
}

// Label is how the dump is named on the command line: d plus its number.
func (d Dump) Label() string { return "d" + strconv.Itoa(d.ID) }

type dumpJournal struct {
	Next  int    `json:"next"`
	Dumps []Dump `json:"dumps"`
}

// DumpDir holds the dump files.
func DumpDir() string { return filepath.Join(snap.Dir(), "db", "dumps") }

func dumpJournalPath() string { return filepath.Join(snap.Dir(), "db", "dumps.json") }

// withDumps loads the dump journal under a lock file, lets fn change it and
// writes it back atomically when fn returns nil.
func withDumps(fn func(*dumpJournal) error) error {
	path := dumpJournalPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lock := path + ".lock"
	for i := 0; ; i++ {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			break
		}
		if fi, serr := os.Stat(lock); serr == nil && time.Since(fi.ModTime()) > 30*time.Second {
			os.Remove(lock) // left behind by a process that died
			continue
		}
		if i > 200 {
			return fmt.Errorf("the dump journal is locked by %s", lock)
		}
		time.Sleep(25 * time.Millisecond)
	}
	defer os.Remove(lock)
	var j dumpJournal
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &j); err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
	}
	if err := fn(&j); err != nil {
		return err
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ListDumps returns the journaled dumps, newest first.
func ListDumps() ([]Dump, error) {
	var out []Dump
	err := withDumps(func(j *dumpJournal) error {
		out = append(out, j.Dumps...)
		return errors.New("read only")
	})
	if err != nil && err.Error() != "read only" {
		return nil, err
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID > out[b].ID })
	return out, nil
}

// FindDump returns the dump with the given label such as d3.
func FindDump(label string) (*Dump, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(strings.ToLower(label), "d"))
	if err != nil {
		return nil, fmt.Errorf("%q is not an id (see oops db list)", label)
	}
	list, err := ListDumps()
	if err != nil {
		return nil, err
	}
	for _, d := range list {
		if d.ID == n {
			return &d, nil
		}
	}
	return nil, fmt.Errorf("no database dump %s (see oops db list)", label)
}

func addDump(d Dump) (Dump, error) {
	err := withDumps(func(j *dumpJournal) error {
		j.Next++
		d.ID = j.Next
		j.Dumps = append(j.Dumps, d)
		return nil
	})
	return d, err
}

func markDumpRestored(id int) error {
	return withDumps(func(j *dumpJournal) error {
		now := time.Now()
		for i := range j.Dumps {
			if j.Dumps[i].ID == id {
				j.Dumps[i].Restored = &now
			}
		}
		return nil
	})
}

// PruneDumps deletes dumps older than the cutoff, or already restored when
// restored is set, plus dump files nothing refers to any more.
func PruneDumps(olderThan time.Duration, restored bool) (int, error) {
	n := 0
	err := withDumps(func(j *dumpJournal) error {
		var keep []Dump
		known := map[string]bool{}
		for _, d := range j.Dumps {
			if time.Since(d.Created) > olderThan || (restored && d.Restored != nil) {
				os.Remove(d.File)
				n++
				continue
			}
			keep = append(keep, d)
			known[filepath.Base(d.File)] = true
		}
		j.Dumps = keep
		entries, _ := os.ReadDir(DumpDir())
		for _, e := range entries {
			info, err := e.Info()
			if err == nil && !known[e.Name()] && time.Since(info.ModTime()) > 24*time.Hour {
				os.Remove(filepath.Join(DumpDir(), e.Name()))
			}
		}
		return nil
	})
	return n, err
}

// FindPGTool finds a Postgres client program such as pg_dump. $OOPS_PG_BIN
// wins. Otherwise the newest version among $PATH and the usual install
// places is used, because pg_dump refuses servers newer than itself.
func FindPGTool(name string) (string, error) {
	exe := name
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if d := os.Getenv("OOPS_PG_BIN"); d != "" {
		p := filepath.Join(d, exe)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	var cands []string
	if p, err := exec.LookPath(name); err == nil {
		cands = append(cands, p)
	}
	var globs []string
	switch runtime.GOOS {
	case "windows":
		globs = []string{`C:\Program Files\PostgreSQL\*\bin`}
		if d := os.Getenv("PGBIN"); d != "" {
			globs = append(globs, d)
		}
	case "darwin":
		globs = []string{"/opt/homebrew/opt/postgresql@*/bin", "/usr/local/opt/postgresql@*/bin", "/Applications/Postgres.app/Contents/Versions/*/bin", "/opt/homebrew/bin", "/usr/local/bin"}
	default:
		globs = []string{"/usr/lib/postgresql/*/bin", "/usr/pgsql-*/bin", "/usr/local/pgsql/bin"}
	}
	for _, g := range globs {
		dirs, _ := filepath.Glob(g)
		for _, d := range dirs {
			p := filepath.Join(d, exe)
			if _, err := os.Stat(p); err == nil {
				cands = append(cands, p)
			}
		}
	}
	best, bestV := "", -1
	for _, c := range cands {
		if v := toolVersion(c); v > bestV {
			best, bestV = c, v
		}
	}
	if best == "" {
		return "", fmt.Errorf("%s is not installed; install the Postgres client tools or set OOPS_PG_BIN to their folder", name)
	}
	return best, nil
}

var versionRe = regexp.MustCompile(`\(PostgreSQL\)\s+(\d+)`)

func toolVersion(p string) int {
	out, err := exec.Command(p, "--version").Output()
	if err != nil {
		return -1
	}
	m := versionRe.FindSubmatch(out)
	if m == nil {
		return 0
	}
	v, _ := strconv.Atoi(string(m[1]))
	return v
}

// toolConn turns a connection string into one for dbname that libpq tools
// accept, moving a URL password into PGPASSWORD so it stays off the
// command line.
func toolConn(upstream, dbname string) (conn string, env []string, err error) {
	if strings.HasPrefix(upstream, "postgres://") || strings.HasPrefix(upstream, "postgresql://") {
		u, err := url.Parse(upstream)
		if err != nil {
			return "", nil, err
		}
		if u.User != nil {
			if pw, ok := u.User.Password(); ok {
				env = append(env, "PGPASSWORD="+pw)
			}
			u.User = url.User(u.User.Username())
		}
		u.Path = "/" + dbname
		u.RawPath = ""
		return u.String(), env, nil
	}
	esc := strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(dbname)
	return upstream + " dbname='" + esc + "'", nil, nil
}

func runTool(ctx context.Context, tool string, env []string, args ...string) error {
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s: %s", filepath.Base(tool), shorten(msg, 400))
	}
	return nil
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// dumpDatabase saves database name with pg_dump in the custom format, which
// compresses table data and keeps indexes as definitions only. It returns
// the pending dump, or an explanation of why no dump was taken.
func dumpDatabase(ctx context.Context, upstream, name, stmt string) (*Dump, string, error) {
	conn, err := pgx.Connect(ctx, upstream)
	if err != nil {
		return nil, "", fmt.Errorf("could not connect to read its size: %w", err)
	}
	var size int64
	err = conn.QueryRow(ctx, "SELECT pg_database_size(oid) FROM pg_database WHERE datname = $1", name).Scan(&size)
	server := fmt.Sprintf("%s:%d", conn.Config().Host, conn.Config().Port)
	conn.Close(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", nil // nothing to drop, so nothing to save
	}
	if err != nil {
		return nil, "", fmt.Errorf("could not read its size: %w", err)
	}
	tool, err := FindPGTool("pg_dump")
	if err != nil {
		return nil, "", err
	}
	dir := DumpDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	// The dump is usually a fraction of pg_database_size, since indexes are
	// left out and data is compressed, but already compressed or random data
	// does not shrink. Planning for the full size never fills the disk.
	if err := snap.CheckSpace(dir, size); err != nil {
		return nil, "", err
	}
	connStr, env, err := toolConn(upstream, name)
	if err != nil {
		return nil, "", err
	}
	file := filepath.Join(dir, fmt.Sprintf("%s-%s.dump", time.Now().Format("20060102-150405.000"), unsafeName.ReplaceAllString(name, "_")))
	part := file + ".part"
	if err := runTool(ctx, tool, env, "--format=custom", "--file="+part, "--dbname="+connStr); err != nil {
		os.Remove(part)
		return nil, "", err
	}
	if err := os.Rename(part, file); err != nil {
		os.Remove(part)
		return nil, "", err
	}
	fi, err := os.Stat(file)
	if err != nil {
		return nil, "", err
	}
	d := &Dump{Database: name, Server: server, Upstream: upstream, File: file, Size: fi.Size(), DBSize: size, Statement: stmt, Created: time.Now()}
	return d, "", nil
}

// RestoreDump creates the database again from its dump with pg_restore
// --create, which also brings back its encoding, locale, owner, settings,
// comments and grants.
func RestoreDump(ctx context.Context, d Dump) (string, error) {
	maint := "postgres"
	cfg, err := pgx.ParseConfig(d.Upstream)
	if err != nil {
		return "", err
	}
	if cfg.Database != "" && cfg.Database != d.Database {
		maint = cfg.Database
	}
	cfg.Database = maint
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return "", fmt.Errorf("could not connect to the server: %w", err)
	}
	var exists bool
	err = conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", d.Database).Scan(&exists)
	conn.Close(ctx)
	if err != nil {
		return "", err
	}
	if exists {
		return "", fmt.Errorf("database %s exists again; rename or drop it first so nothing gets overwritten", d.Database)
	}
	if _, err := os.Stat(d.File); err != nil {
		return "", fmt.Errorf("the dump file is gone: %w", err)
	}
	tool, err := FindPGTool("pg_restore")
	if err != nil {
		return "", err
	}
	connStr, env, err := toolConn(d.Upstream, maint)
	if err != nil {
		return "", err
	}
	if err := runTool(ctx, tool, env, "--create", "--exit-on-error", "--dbname="+connStr, d.File); err != nil {
		return "", err
	}
	if err := markDumpRestored(d.ID); err != nil {
		return "", err
	}
	return fmt.Sprintf("database %s created again and restored from its %s dump", d.Database, HumanBytes(d.Size)), nil
}

// HumanBytes formats a size for people.
func HumanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}
