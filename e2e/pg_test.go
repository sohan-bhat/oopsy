package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// A throwaway Postgres cluster, started on first use and stopped in TestMain.
var (
	pgOnce   sync.Once
	pgURL    string
	pgErr    error
	pgDir    string
	pgCtlBin string
)

func findPGBin() string {
	if d := os.Getenv("OOPS_TEST_PG_BIN"); d != "" {
		return d
	}
	if p, err := exec.LookPath("pg_ctl"); err == nil {
		return filepath.Dir(p)
	}
	for _, d := range []string{"/usr/local/opt/postgresql@17/bin", "/opt/homebrew/opt/postgresql@17/bin", "/usr/lib/postgresql/17/bin", os.Getenv("PGBIN")} {
		if _, err := os.Stat(filepath.Join(d, exe("pg_ctl"))); d != "" && err == nil {
			return d
		}
	}
	return ""
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 55432
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startPG() {
	if u := os.Getenv("OOPS_TEST_DATABASE_URL"); u != "" {
		pgURL = u
		return
	}
	binDir := findPGBin()
	if binDir == "" {
		pgErr = fmt.Errorf("no Postgres found (install postgresql@17 or set OOPS_TEST_PG_BIN)")
		return
	}
	pgDir, _ = os.MkdirTemp("", "oops-pg")
	data := filepath.Join(pgDir, "data")
	out, err := exec.Command(filepath.Join(binDir, exe("initdb")), "-D", data, "-U", "postgres", "-A", "trust", "-E", "UTF8", "--no-locale").CombinedOutput()
	if err != nil {
		pgErr = fmt.Errorf("initdb: %v %s", err, out)
		return
	}
	port := freePort()
	pgCtlBin = filepath.Join(binDir, exe("pg_ctl"))
	opts := fmt.Sprintf("-p %d -k %s -c listen_addresses=127.0.0.1 -c fsync=off", port, pgDir)
	if onWindows {
		opts = fmt.Sprintf("-p %d -c listen_addresses=127.0.0.1 -c fsync=off", port)
	}
	// The server inherits pg_ctl's output handles, so capturing them in a
	// pipe would wait for the server to exit. Use a file.
	ctlLog, _ := os.Create(filepath.Join(pgDir, "pg_ctl.out"))
	start := exec.Command(pgCtlBin, "-D", data, "-o", opts, "-l", filepath.Join(pgDir, "log"), "-w", "start")
	start.Stdout, start.Stderr = ctlLog, ctlLog
	err = start.Run()
	ctlLog.Close()
	out, _ = os.ReadFile(filepath.Join(pgDir, "pg_ctl.out"))
	if err != nil {
		pgErr = fmt.Errorf("pg_ctl start: %v %s", err, out)
		return
	}
	pgURL = fmt.Sprintf("postgres://postgres@127.0.0.1:%d/postgres?sslmode=disable", port)
}

func stopPG() {
	if pgCtlBin != "" && pgDir != "" {
		exec.Command(pgCtlBin, "-D", filepath.Join(pgDir, "data"), "-m", "immediate", "-w", "stop").Run()
		os.RemoveAll(pgDir)
	}
}

func needPG(t *testing.T) string {
	t.Helper()
	pgOnce.Do(startPG)
	if pgErr != nil {
		t.Skip(pgErr)
	}
	return pgURL
}

// freshDB creates an empty database for one test.
func freshDB(t *testing.T) string {
	t.Helper()
	base := needPG(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	name := fmt.Sprintf("oops_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(ctx, base)
		if err == nil {
			c.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(ctx)
		}
	})
	return strings.Replace(base, "/postgres?", "/"+name+"?", 1)
}

type proxyProc struct {
	cmd *exec.Cmd
	url string
	log *strings.Builder
}

func (e *env) startProxy(t *testing.T, upstream string) *proxyProc {
	t.Helper()
	port := freePort()
	cmd := exec.Command(bin, "db", "proxy", "--upstream", upstream, "--listen", fmt.Sprintf("127.0.0.1:%d", port))
	cmd.Env = e.environ()
	var logb strings.Builder
	cmd.Stderr = &syncWriter{b: &logb}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		if onWindows {
			// Windows has no interrupt signal to send another process.
			cmd.Process.Kill()
		} else {
			cmd.Process.Signal(os.Interrupt)
		}
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			c.Close()
			break
		}
		select {
		case <-exited:
			t.Fatalf("proxy exited: %s", logb.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy did not start: %s", logb.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	u := strings.Replace(upstream, strings.Split(strings.Split(upstream, "@")[1], "/")[0], fmt.Sprintf("127.0.0.1:%d", port), 1)
	return &proxyProc{cmd: cmd, url: u, log: &logb}
}

type syncWriter struct {
	mu sync.Mutex
	b  *strings.Builder
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func mustConnect(t *testing.T, url string, simple bool) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	if simple {
		cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	}
	c, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func exec1(t *testing.T, c *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := c.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func scalar[T any](t *testing.T, c *pgx.Conn, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := c.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

// fingerprint hashes every row of a table in a stable order.
func fingerprint(t *testing.T, c *pgx.Conn, table, order string) string {
	t.Helper()
	return scalar[string](t, c, fmt.Sprintf("SELECT coalesce(md5(string_agg(x::text, '|' ORDER BY %s)), 'empty') FROM %s x", order, table))
}

const seed = `
CREATE TABLE users (
  id serial PRIMARY KEY,
  email text NOT NULL UNIQUE,
  name text DEFAULT 'anon',
  age int CHECK (age >= 0),
  meta jsonb,
  created_at timestamptz NOT NULL DEFAULT '2024-01-01'
);
CREATE INDEX users_name_idx ON users (lower(name));
CREATE TABLE orders (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  user_id int NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  total numeric(10,2) NOT NULL,
  doubled numeric GENERATED ALWAYS AS (total * 2) STORED
);
CREATE TABLE events (kind text, payload text);
CREATE VIEW adults AS SELECT id, email FROM users WHERE age >= 18;
INSERT INTO users (email, name, age, meta)
  SELECT 'user' || g || '@example.com', 'Name ' || g, g % 90, jsonb_build_object('n', g, 'q', 'it''s')
  FROM generate_series(1, 500) g;
INSERT INTO orders (user_id, total) SELECT (g % 500) + 1, g * 1.25 FROM generate_series(1, 1500) g;
INSERT INTO events SELECT 'k' || (g % 7), repeat('x', g % 13) FROM generate_series(1, 300) g;
`

func setupDB(t *testing.T) (*env, string, *proxyProc, *pgx.Conn) {
	t.Helper()
	e := newEnv(t)
	db := freshDB(t)
	direct := mustConnect(t, db, false)
	exec1(t, direct, seed)
	p := e.startProxy(t, db)
	return e, db, p, direct
}

func (e *env) dbUndo(t *testing.T, db string, args ...string) string {
	t.Helper()
	out, errOut, err := e.oopsCmd("", append([]string{"db", "undo", "-y", "--url", db}, args...)...)
	if err != nil {
		t.Fatalf("oops db undo: %v\n%s\n%s", err, out, errOut)
	}
	return out
}

func TestPGDropTableCascadeUndo(t *testing.T) {
	e, db, p, direct := setupDB(t)
	usersFP := fingerprint(t, direct, "users", "id")
	ordersFP := fingerprint(t, direct, "orders", "id")
	client := mustConnect(t, p.url, false)
	exec1(t, client, "DROP TABLE users CASCADE")
	if scalar[bool](t, direct, "SELECT to_regclass('users') IS NULL") != true {
		t.Fatal("drop did not happen")
	}
	if n := scalar[int](t, direct, "SELECT count(*) FROM pg_constraint WHERE conrelid = 'orders'::regclass AND contype = 'f'"); n != 0 {
		t.Fatal("cascade did not drop the foreign key")
	}
	list, _, _ := e.oopsCmd("", "db", "list", "--url", db)
	if !strings.Contains(list, "DROP TABLE") || !strings.Contains(list, "public.users") {
		t.Fatalf("db list: %s", list)
	}
	out := e.dbUndo(t, db)
	if !strings.Contains(out, "recreated with 500 rows") {
		t.Fatalf("undo output: %s", out)
	}
	if got := fingerprint(t, direct, "users", "id"); got != usersFP {
		t.Fatalf("users rows differ after undo")
	}
	if got := fingerprint(t, direct, "orders", "id"); got != ordersFP {
		t.Fatalf("orders changed")
	}
	// Index, unique, check, foreign key and view are back, and the serial
	// sequence continues where it was.
	if n := scalar[int](t, direct, "SELECT count(*) FROM pg_indexes WHERE tablename = 'users'"); n != 3 {
		t.Errorf("expected 3 indexes on users, got %d", n)
	}
	if n := scalar[int](t, direct, "SELECT count(*) FROM pg_constraint WHERE conrelid = 'orders'::regclass AND contype = 'f'"); n != 1 {
		t.Errorf("foreign key from orders not restored")
	}
	if n := scalar[int](t, direct, "SELECT count(*) FROM adults"); n == 0 {
		t.Errorf("view not restored")
	}
	if id := scalar[int](t, direct, "INSERT INTO users (email) VALUES ('new@x') RETURNING id"); id != 501 {
		t.Errorf("sequence did not continue: next id %d", id)
	}
	if _, err := direct.Exec(context.Background(), "INSERT INTO users (email, age) VALUES ('bad@x', -1)"); err == nil {
		t.Errorf("check constraint not restored")
	}
}

func TestPGDeleteWithoutWhere(t *testing.T) {
	e, db, p, direct := setupDB(t)
	fp := fingerprint(t, direct, "orders", "id")
	client := mustConnect(t, p.url, false)
	exec1(t, client, "DELETE FROM orders")
	if n := scalar[int](t, direct, "SELECT count(*) FROM orders"); n != 0 {
		t.Fatal("delete did not run")
	}
	e.dbUndo(t, db)
	if got := fingerprint(t, direct, "orders", "id"); got != fp {
		t.Fatal("orders not restored exactly")
	}
	// Identity keeps counting past restored rows.
	if id := scalar[int64](t, direct, "INSERT INTO orders (user_id, total) VALUES (1, 1) RETURNING id"); id != 1501 {
		t.Errorf("identity next value %d", id)
	}
}

func TestPGDeleteWithParamsExtendedProtocol(t *testing.T) {
	e, db, p, direct := setupDB(t)
	fp := fingerprint(t, direct, "users", "id")
	client := mustConnect(t, p.url, false)
	tag, err := client.Exec(context.Background(), "DELETE FROM users WHERE age > $1 AND name LIKE $2", 40, "Name 1%")
	if err != nil {
		t.Fatal(err)
	}
	deleted := tag.RowsAffected()
	if deleted == 0 {
		t.Fatal("nothing deleted")
	}
	// Reuse the prepared statement with other values: each run gets its own snapshot.
	if _, err := client.Exec(context.Background(), "DELETE FROM users WHERE age > $1 AND name LIKE $2", 80, "Name 2%"); err != nil {
		t.Fatal(err)
	}
	list, _, _ := e.oopsCmd("", "db", "list", "--url", db)
	if strings.Count(list, "DELETE") < 2 {
		t.Fatalf("expected two changes: %s", list)
	}
	e.dbUndo(t, db) // newest first
	e.dbUndo(t, db)
	if got := fingerprint(t, direct, "users", "id"); got != fp {
		t.Fatal("users not restored")
	}
	if n := scalar[int](t, direct, "SELECT count(*) FROM orders"); n != 1500 {
		t.Fatalf("orders cascade rows not handled: %d", n)
	}
}

func TestPGUpdate(t *testing.T) {
	e, db, p, direct := setupDB(t)
	fp := fingerprint(t, direct, "users", "id")
	client := mustConnect(t, p.url, false)
	exec1(t, client, "UPDATE users SET name = $1, meta = NULL WHERE age < $2", "oops", 30)
	if n := scalar[int](t, direct, "SELECT count(*) FROM users WHERE name = 'oops'"); n == 0 {
		t.Fatal("update did not run")
	}
	e.dbUndo(t, db)
	if got := fingerprint(t, direct, "users", "id"); got != fp {
		t.Fatal("old values not restored")
	}
}

func TestPGUpdateWithoutKeySimpleProtocol(t *testing.T) {
	e, db, p, direct := setupDB(t)
	fp := fingerprint(t, direct, "events", "kind, payload")
	client := mustConnect(t, p.url, true)
	exec1(t, client, "UPDATE events SET payload = 'gone' WHERE kind = 'k3'")
	exec1(t, client, "INSERT INTO events VALUES ('later', 'row')")
	out := e.dbUndo(t, db)
	if !strings.Contains(out, "reset to the") {
		t.Fatalf("expected whole table restore: %s", out)
	}
	if got := fingerprint(t, direct, "events", "kind, payload"); got != fp {
		t.Fatal("events not restored")
	}
}

func TestPGTruncateCascade(t *testing.T) {
	e, db, p, direct := setupDB(t)
	ufp := fingerprint(t, direct, "users", "id")
	ofp := fingerprint(t, direct, "orders", "id")
	client := mustConnect(t, p.url, true)
	exec1(t, client, "TRUNCATE users CASCADE")
	if n := scalar[int](t, direct, "SELECT count(*) FROM orders"); n != 0 {
		t.Fatal("truncate cascade did not run")
	}
	e.dbUndo(t, db)
	if fingerprint(t, direct, "users", "id") != ufp || fingerprint(t, direct, "orders", "id") != ofp {
		t.Fatal("tables not restored after TRUNCATE CASCADE")
	}
}

func TestPGDropColumn(t *testing.T) {
	e, db, p, direct := setupDB(t)
	before := fingerprint(t, direct, "(SELECT id, email FROM users) ", "id")
	client := mustConnect(t, p.url, false)
	exec1(t, client, "ALTER TABLE users DROP COLUMN email CASCADE")
	if scalar[int](t, direct, "SELECT count(*) FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'email'") != 0 {
		t.Fatal("column not dropped")
	}
	e.dbUndo(t, db)
	if got := fingerprint(t, direct, "(SELECT id, email FROM users) ", "id"); got != before {
		t.Fatal("column values not restored")
	}
	if scalar[bool](t, direct, "SELECT attnotnull FROM pg_attribute WHERE attrelid = 'users'::regclass AND attname = 'email'") != true {
		t.Error("NOT NULL not restored")
	}
	if _, err := direct.Exec(context.Background(), "INSERT INTO users (email) VALUES ('user1@example.com')"); err == nil {
		t.Error("unique constraint on the column not restored")
	}
	if n := scalar[int](t, direct, "SELECT count(*) FROM adults"); n == 0 {
		t.Error("dependent view not restored")
	}
}

func TestPGDropSchema(t *testing.T) {
	e, db, p, direct := setupDB(t)
	exec1(t, direct, "CREATE SCHEMA app; CREATE TABLE app.a (id int PRIMARY KEY, v text); CREATE TABLE app.b (id int REFERENCES app.a(id), w text); INSERT INTO app.a SELECT g, 'v'||g FROM generate_series(1,50) g; INSERT INTO app.b SELECT g, 'w' FROM generate_series(1,50) g")
	afp := fingerprint(t, direct, "app.a", "id")
	bfp := fingerprint(t, direct, "app.b", "id")
	client := mustConnect(t, p.url, true)
	exec1(t, client, "DROP SCHEMA app CASCADE")
	e.dbUndo(t, db)
	if fingerprint(t, direct, "app.a", "id") != afp || fingerprint(t, direct, "app.b", "id") != bfp {
		t.Fatal("schema tables not restored")
	}
	if n := scalar[int](t, direct, "SELECT count(*) FROM pg_constraint WHERE conrelid = 'app.b'::regclass AND contype = 'f'"); n != 1 {
		t.Error("foreign key inside schema not restored")
	}
}

func TestPGRollbackDiscardsSnapshot(t *testing.T) {
	e, db, p, direct := setupDB(t)
	client := mustConnect(t, p.url, false)
	ctx := context.Background()
	tx, err := client.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM events"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "DROP TABLE orders"); err != nil {
		t.Fatal(err)
	}
	tx.Rollback(ctx)
	if n := scalar[int](t, direct, "SELECT count(*) FROM oops.journal"); n != 0 {
		t.Fatalf("rolled back statements left %d journal rows", n)
	}
	if n := scalar[int](t, direct, "SELECT count(*) FROM pg_tables WHERE schemaname = 'oops' AND tablename <> 'journal'"); n != 0 {
		t.Fatalf("rolled back statements left %d snapshot tables", n)
	}
	// A committed transaction keeps its snapshot.
	tx, _ = client.Begin(ctx)
	tx.Exec(ctx, "DELETE FROM events WHERE kind = 'k1'")
	tx.Commit(ctx)
	if n := scalar[int](t, direct, "SELECT count(*) FROM oops.journal"); n != 1 {
		t.Fatalf("committed delete should have 1 journal row, got %d", n)
	}
	_ = e
	_ = db
}

func TestPGMultiStatementSimpleQueryHidesSnapshotResults(t *testing.T) {
	e, db, p, direct := setupDB(t)
	fp := fingerprint(t, direct, "events", "kind, payload")
	client := mustConnect(t, p.url, true)
	results, err := client.PgConn().Exec(context.Background(), "SELECT 1; DELETE FROM events WHERE kind = 'k2'; SELECT 'after'").ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("client saw %d results, want 3", len(results))
	}
	if string(results[2].Rows[0][0]) != "after" || !strings.HasPrefix(results[1].CommandTag.String(), "DELETE") {
		t.Fatalf("results out of order: %v %v", results[1].CommandTag, results[2].Rows)
	}
	e.dbUndo(t, db)
	if fingerprint(t, direct, "events", "kind, payload") != fp {
		t.Fatal("events not restored")
	}
}

func TestPGErrorsKeepSessionUsable(t *testing.T) {
	_, _, p, _ := setupDB(t)
	for _, simple := range []bool{true, false} {
		client := mustConnect(t, p.url, simple)
		_, err := client.Exec(context.Background(), "DELETE FROM no_such_table WHERE id = 1")
		if err == nil {
			t.Fatal("expected an error")
		}
		if v := scalar[int](t, client, "SELECT 41 + 1"); v != 42 {
			t.Fatal("session broken after error")
		}
		_, err = client.Exec(context.Background(), "DELETE FROM events WHERE kind = $1", "k5")
		if err != nil {
			t.Fatalf("delete after error: %v", err)
		}
	}
}

func TestPGWrap(t *testing.T) {
	e := newEnv(t)
	db := freshDB(t)
	direct := mustConnect(t, db, false)
	exec1(t, direct, seed)
	fp := fingerprint(t, direct, "events", "kind, payload")
	// The wrapped command is the test binary's helper mode: a tiny Go program
	// would need building, so use the oops binary's own db undo path instead:
	// run a shell that connects through DATABASE_URL with pgx via "oops db list".
	helper := filepath.Join(e.home, "client.go")
	os.WriteFile(helper, []byte(`package main
import ("context";"os";"github.com/jackc/pgx/v5")
func main(){c,err:=pgx.Connect(context.Background(),os.Getenv("DATABASE_URL"));if err!=nil{panic(err)};_,err=c.Exec(context.Background(),"DELETE FROM events");if err!=nil{panic(err)}}
`), 0o644)
	helperBin := filepath.Join(e.home, exe("client"))
	build := exec.Command("go", "build", "-o", helperBin, helper)
	build.Dir, _ = os.Getwd()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v %s", err, out)
	}
	c := exec.Command(bin, "db", "wrap", "--upstream", db, "--", helperBin)
	c.Env = e.environ()
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("wrap: %v %s", err, out)
	}
	if n := scalar[int](t, direct, "SELECT count(*) FROM events"); n != 0 {
		t.Fatal("wrapped command did not run")
	}
	e.dbUndo(t, db)
	if fingerprint(t, direct, "events", "kind, payload") != fp {
		t.Fatal("events not restored after wrap")
	}
}
