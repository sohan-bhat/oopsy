package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// pgTool finds a Postgres client program next to the test server, or skips.
func pgTool(t *testing.T, name string) string {
	t.Helper()
	d := findPGBin()
	p := filepath.Join(d, exe(name))
	if _, err := os.Stat(p); d == "" || err != nil {
		t.Skipf("%s not found (set OOPS_TEST_PG_BIN)", name)
	}
	return p
}

// plainDump renders a whole database, its CREATE DATABASE, settings,
// comments, grants, schema and data, as text that compares state for state.
func plainDump(t *testing.T, url string) string {
	t.Helper()
	out, err := exec.Command(pgTool(t, "pg_dump"), "--create", "--dbname="+url).Output()
	if err != nil {
		t.Fatalf("pg_dump: %v", err)
	}
	var keep []string
	for _, l := range strings.Split(string(out), "\n") {
		// Postgres 17.6 and later add a random key to each plain dump.
		if strings.HasPrefix(l, `\restrict`) || strings.HasPrefix(l, `\unrestrict`) {
			continue
		}
		keep = append(keep, l)
	}
	return strings.Join(keep, "\n")
}

func dbName(url string) string {
	return strings.SplitN(strings.SplitN(url, "/", 4)[3], "?", 2)[0]
}

func noticesConn(t *testing.T, url string) (*pgx.Conn, *[]string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	var notes []string
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) { notes = append(notes, n.Severity+": "+n.Message) }
	c, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c, &notes
}

func TestPGDropDatabaseUndo(t *testing.T) {
	e := newEnv(t)
	db := freshDB(t)
	base := needPG(t)
	t.Setenv("OOPS_PG_BIN", filepath.Dir(pgTool(t, "pg_restore")))
	name := dbName(db)
	ctx := context.Background()
	direct, err := pgx.Connect(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	exec1(t, direct, seed)
	// A table shaped like real app data, so the dump ratio means something.
	exec1(t, direct, `CREATE TABLE accounts (
	    id bigint PRIMARY KEY, email text NOT NULL, name text, plan text, balance numeric(12,2),
	    created_at timestamptz, settings jsonb, api_key text);
	  INSERT INTO accounts SELECT g, 'person' || g || '@example.com', 'Person number ' || g,
	    (ARRAY['free','pro','team'])[1 + g % 3], (g * 7919 % 100000) / 100.0,
	    '2024-01-01'::timestamptz + g * interval '1 minute',
	    jsonb_build_object('theme', CASE WHEN g % 2 = 0 THEN 'dark' ELSE 'light' END, 'beta', g % 5 = 0),
	    md5(g::text)
	  FROM generate_series(1, 200000) g;
	  CREATE INDEX accounts_email ON accounts (email);
	  CREATE INDEX accounts_created ON accounts (created_at);
	  COMMENT ON DATABASE `+name+` IS 'the app';
	  ALTER DATABASE `+name+` SET work_mem = '8MB';
	  SELECT setval('users_id_seq', 777);`)
	exec1(t, direct, "VACUUM ANALYZE accounts")
	size := scalar[int64](t, direct, "SELECT pg_database_size(current_database())")
	direct.Close(ctx)
	before := plainDump(t, db)

	p := e.startProxy(t, base)
	client, notes := noticesConn(t, p.url)
	exec1(t, client, "DROP DATABASE "+name)
	if n := scalar[int](t, client, "SELECT count(*) FROM pg_database WHERE datname = $1", name); n != 0 {
		t.Fatal("database was not dropped")
	}
	if len(*notes) == 0 || !strings.Contains(strings.Join(*notes, "\n"), "saved a") {
		t.Fatalf("expected a notice about the dump, got %v", *notes)
	}
	list, _, _ := e.oopsCmd("", "db", "list", "--url", db)
	if !strings.Contains(list, "DROP DATABASE") || !strings.Contains(list, name) {
		t.Fatalf("db list: %s", list)
	}
	dumps, _ := filepath.Glob(filepath.Join(e.oops, "db", "dumps", "*.dump"))
	if len(dumps) != 1 {
		t.Fatalf("expected one dump file, got %v", dumps)
	}
	fi, _ := os.Stat(dumps[0])
	t.Logf("pg_dump -Fc of a %d byte database: %d bytes, %.1f%% of pg_database_size", size, fi.Size(), 100*float64(fi.Size())/float64(size))

	// The database in --url is gone, so undo falls back to the dump.
	out := e.dbUndo(t, db)
	if !strings.Contains(out, "database "+name+" created again") {
		t.Fatalf("undo output: %s", out)
	}
	if after := plainDump(t, db); after != before {
		a, b := strings.Split(before, "\n"), strings.Split(after, "\n")
		for i := 0; i < len(a) && i < len(b); i++ {
			if a[i] != b[i] {
				t.Fatalf("database differs after restore at line %d:\n%s\n%s", i, a[i], b[i])
			}
		}
		t.Fatalf("database differs after restore: %d lines before, %d after", len(a), len(b))
	}
}

func TestPGDropDatabaseWithoutRoomStillRuns(t *testing.T) {
	e := newEnv(t)
	db := freshDB(t)
	base := needPG(t)
	name := dbName(db)
	// A free space floor no disk can meet: the dump is refused.
	p := e.startProxy(t, base, fmt.Sprintf("OOPS_FREE_FLOOR_BYTES=%d", int64(1)<<60))
	client, notes := noticesConn(t, p.url)
	exec1(t, client, "DROP DATABASE "+name)
	if n := scalar[int](t, client, "SELECT count(*) FROM pg_database WHERE datname = $1", name); n != 0 {
		t.Fatal("database was not dropped")
	}
	if !strings.Contains(strings.Join(*notes, "\n"), "took no dump of database "+name) {
		t.Fatalf("expected a warning, got %v", *notes)
	}
	if !strings.Contains(p.log.String(), "took no dump") {
		t.Fatalf("proxy log: %s", p.log.String())
	}
}

func TestPGDropDatabaseExtendedProtocolFailedDropKeepsNoDump(t *testing.T) {
	e := newEnv(t)
	db := freshDB(t)
	base := needPG(t)
	t.Setenv("OOPS_PG_BIN", filepath.Dir(pgTool(t, "pg_dump")))
	name := dbName(db)
	// An open connection makes the drop fail, so the dump is thrown away.
	hold := mustConnect(t, db, false)
	exec1(t, hold, "SELECT 1")
	p := e.startProxy(t, base)
	client := mustConnect(t, p.url, false)
	if _, err := client.Exec(context.Background(), "DROP DATABASE "+name); err == nil {
		t.Fatal("drop should fail while another session is connected")
	}
	if dumps, _ := filepath.Glob(filepath.Join(e.oops, "db", "dumps", "*")); len(dumps) != 0 {
		t.Fatalf("dump of a database that still exists was kept: %v", dumps)
	}
	if !strings.Contains(p.log.String(), "did not run, so its dump was deleted") {
		t.Fatalf("proxy log: %s", p.log.String())
	}
}
