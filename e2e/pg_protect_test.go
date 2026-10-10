package e2e

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func protectDB(t *testing.T) (*env, string, *pgx.Conn) {
	t.Helper()
	e := newEnv(t)
	db := freshDB(t)
	direct := mustConnect(t, db, false)
	exec1(t, direct, seed)
	out := e.mustOops("db", "protect", "--url", db)
	if !strings.Contains(out, "Protected 3 tables") {
		t.Fatalf("protect: %s", out)
	}
	return e, db, direct
}

func allState(t *testing.T, c *pgx.Conn) string {
	t.Helper()
	return fingerprint(t, c, "users", "id") + fingerprint(t, c, "orders", "id") + fingerprint(t, c, "events", "kind, payload") +
		scalar[string](t, c, "SELECT count(*)::text || pg_get_viewdef('adults') FROM adults")
}

func TestPGProtectUndoesChangesThatBypassTheProxy(t *testing.T) {
	e, db, direct := protectDB(t)
	before := allState(t, direct)
	// A separate session, like psql or a migration tool, with no proxy.
	other := mustConnect(t, db, true)
	steps := []struct{ name, sql, expect string }{
		{"delete with cascade", "DELETE FROM users WHERE age > 50", "rows put back into public.orders"},
		{"update", "UPDATE users SET name = 'x', meta = '{}' WHERE id < 100", "rows in public.users set back"},
		{"update without a key", "UPDATE events SET payload = 'gone' WHERE kind = 'k3'", "matched on every column"},
		{"truncate", "TRUNCATE events", "rows put back into public.events"},
		{"drop table", "DROP TABLE orders", "table public.orders recreated with 1500 rows"},
		{"drop view", "DROP VIEW adults", "view public.adults recreated"},
		{"one transaction", "BEGIN; UPDATE users SET age = 1 WHERE id = 5; UPDATE users SET age = 2 WHERE id = 5; DELETE FROM users WHERE id IN (5, 6); COMMIT", "rows put back into public.users"},
		{"several statements", "DELETE FROM events WHERE kind = 'k1'; DROP TABLE orders; UPDATE users SET name = NULL", "table public.orders recreated"},
	}
	for _, s := range steps {
		exec1(t, other, s.sql)
		var n int
		if direct.QueryRow(context.Background(), "SELECT count(*) FROM pg_class WHERE relname IN ('orders', 'adults')").Scan(&n); n == 2 && allState(t, direct) == before {
			t.Fatalf("%s: nothing changed", s.name)
		}
		list, _, _ := e.oopsCmd("", "db", "list", "--url", db)
		out := e.dbUndo(t, db)
		if !strings.Contains(out, s.expect) {
			t.Fatalf("%s: undo output: %s\nlist: %s", s.name, out, list)
		}
		if got := allState(t, direct); got != before {
			t.Fatalf("%s: not restored exactly\n%s", s.name, out)
		}
	}
	// Undo itself is not saved as a change, and nothing is left to undo.
	if out := e.dbUndo(t, db); !strings.Contains(out, "Nothing to undo") {
		t.Fatalf("undo saved its own changes: %s", out)
	}
	if id := scalar[int64](t, direct, "INSERT INTO orders (user_id, total) VALUES (1, 1) RETURNING id"); id != 1501 {
		t.Errorf("identity next value %d after restoring a dropped table", id)
	}
}

func TestPGProtectCoversTablesCreatedLaterAndFunctions(t *testing.T) {
	e, db, direct := protectDB(t)
	other := mustConnect(t, db, false)
	exec1(t, other, "CREATE TABLE later (id int PRIMARY KEY, v text)")
	exec1(t, other, "INSERT INTO later SELECT g, 'v' || g FROM generate_series(1, 100) g")
	exec1(t, other, "CREATE FUNCTION twice(int) RETURNS int LANGUAGE sql AS 'SELECT $1 * 2'")
	fp := fingerprint(t, direct, "later", "id")
	def := scalar[string](t, direct, "SELECT pg_get_functiondef(to_regprocedure('twice(int)'))")
	exec1(t, other, "DELETE FROM later WHERE id % 3 = 0")
	e.dbUndo(t, db)
	if fingerprint(t, direct, "later", "id") != fp {
		t.Fatal("rows of a table created after protect were not restored")
	}
	exec1(t, other, "DROP FUNCTION twice(int)")
	out := e.dbUndo(t, db)
	if !strings.Contains(out, "function public.twice(integer) recreated") {
		t.Fatalf("undo output: %s", out)
	}
	if scalar[string](t, direct, "SELECT pg_get_functiondef(to_regprocedure('twice(int)'))") != def {
		t.Fatal("function definition differs")
	}
	// A comment that mentions DROP TABLE must not confuse it.
	exec1(t, other, "/* DROP TABLE users */ DROP TABLE IF EXISTS no_such_table, later")
	out = e.dbUndo(t, db)
	if !strings.Contains(out, "table public.later recreated with 100 rows") {
		t.Fatalf("undo output: %s", out)
	}
	if n := scalar[int](t, direct, "SELECT count(*) FROM oops.journal WHERE rel_name = 'users' AND kind = 'drop_table'"); n != 0 {
		t.Fatal("a table that was not dropped was saved")
	}
}

func TestPGProtectKeepsChangesForSevenDays(t *testing.T) {
	e, db, direct := protectDB(t)
	other := mustConnect(t, db, false)
	exec1(t, other, "DELETE FROM events WHERE kind = 'k1'")
	exec1(t, other, "DELETE FROM events WHERE kind = 'k2'")
	exec1(t, direct, "UPDATE oops.journal SET created_at = now() - interval '8 days' WHERE statement LIKE '%k1%'")
	list := e.mustOops("db", "list", "--url", db)
	if strings.Contains(list, "'k1'") || !strings.Contains(list, "'k2'") {
		t.Fatalf("db list: %s", list)
	}
	if n := scalar[int](t, direct, "SELECT count(*) FROM oops.audit WHERE jid NOT IN (SELECT id FROM oops.journal)"); n != 0 {
		t.Fatalf("%d saved rows outlived their journal entries", n)
	}
	if n := scalar[int](t, direct, "SELECT count(*) FROM oops.audit"); n == 0 {
		t.Fatal("the recent change lost its saved rows")
	}
}

func TestPGProtectWithProxySavesOnce(t *testing.T) {
	e, db, direct := protectDB(t)
	p := e.startProxy(t, db)
	client := mustConnect(t, p.url, false)
	exec1(t, client, "DELETE FROM events WHERE kind = 'k2'")
	exec1(t, client, "UPDATE users SET name = 'y' WHERE id < 10")
	exec1(t, client, "DROP TABLE orders")
	if n := scalar[int](t, direct, "SELECT count(DISTINCT grp) FROM oops.journal"); n != 3 {
		t.Fatalf("expected 3 saved changes, got %d", n)
	}
	if n := scalar[int](t, direct, "SELECT count(*) FROM oops.journal WHERE grp LIKE 'tx%'"); n != 0 {
		t.Fatalf("protect saved %d changes the proxy had saved already", n)
	}
}

func oopsObjects(t *testing.T, c *pgx.Conn) []string {
	t.Helper()
	rows, err := c.Query(context.Background(), `
	  SELECT 'function ' || p.oid::regprocedure::text FROM pg_proc p WHERE p.pronamespace = 'oops'::regnamespace
	  UNION ALL SELECT 'event trigger ' || evtname FROM pg_event_trigger
	  UNION ALL SELECT 'trigger ' || tgname || ' on ' || tgrelid::regclass::text FROM pg_trigger WHERE NOT tgisinternal
	  UNION ALL SELECT 'function ' || p.oid::regprocedure::text FROM pg_proc p
	    JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname NOT IN ('pg_catalog', 'information_schema', 'oops')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func TestPGUnprotectRemovesEverythingItAdded(t *testing.T) {
	e := newEnv(t)
	db := freshDB(t)
	direct := mustConnect(t, db, false)
	exec1(t, direct, seed)
	// The proxy's own functions are the baseline.
	e.mustOops("db", "list", "--url", db)
	p := e.startProxy(t, db)
	_ = p
	base := strings.Join(oopsObjects(t, direct), "\n")
	e.mustOops("db", "protect", "--url", db)
	if strings.Join(oopsObjects(t, direct), "\n") == base {
		t.Fatal("protect added nothing")
	}
	exec1(t, direct, "DELETE FROM events WHERE kind = 'k4'")
	out := e.mustOops("db", "unprotect", "--url", db)
	if !strings.Contains(out, "protect triggers are gone") {
		t.Fatalf("unprotect: %s", out)
	}
	if got := strings.Join(oopsObjects(t, direct), "\n"); got != base {
		t.Fatalf("unprotect left objects behind:\nbefore protect:\n%s\nafter unprotect:\n%s", base, got)
	}
	// What remains is data: the journal and the saved rows.
	rels := scalar[string](t, direct, "SELECT string_agg(relname, ' ' ORDER BY relname) FROM pg_class WHERE relnamespace = 'oops'::regnamespace AND relkind IN ('r', 'S')")
	if rels != "audit journal journal_id_seq" {
		t.Fatalf("oops schema holds %s", rels)
	}
	// And the change saved while protected can still be undone.
	fp := scalar[int](t, direct, "SELECT count(*) FROM events")
	e.dbUndo(t, db)
	if scalar[int](t, direct, "SELECT count(*) FROM events") <= fp {
		t.Fatal("change saved before unprotect could not be undone")
	}
}

// TestPGProtectCost measures what protect costs: bytes kept per changed row
// and how much slower writes get.
func TestPGProtectCost(t *testing.T) {
	e := newEnv(t)
	db := freshDB(t)
	c := mustConnect(t, db, false)
	exec1(t, c, `CREATE TABLE items (id bigint PRIMARY KEY, sku text NOT NULL, title text, price numeric(10,2), stock int, updated_at timestamptz);
	  INSERT INTO items SELECT g, 'SKU-' || g, 'Item number ' || g, g % 1000 / 10.0, g % 50, '2024-01-01'::timestamptz + g * interval '1 second'
	  FROM generate_series(1, 100000) g`)
	ctx := context.Background()
	timeIt := func(sql string) time.Duration {
		best := time.Hour
		for i := 0; i < 3; i++ {
			start := time.Now()
			if _, err := c.Exec(ctx, sql); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}
	small := func() time.Duration {
		start := time.Now()
		for i := 1; i <= 500; i++ {
			if _, err := c.Exec(ctx, "UPDATE items SET stock = stock + 1 WHERE id = $1", i); err != nil {
				t.Fatal(err)
			}
		}
		return time.Since(start)
	}
	bulk := "UPDATE items SET stock = stock + 1"
	baseBulk, baseSmall := timeIt(bulk), small()
	exec1(t, c, "TRUNCATE items") // reset bloat so both runs start alike
	exec1(t, c, `INSERT INTO items SELECT g, 'SKU-' || g, 'Item number ' || g, g % 1000 / 10.0, g % 50, '2024-01-01'::timestamptz + g * interval '1 second'
	  FROM generate_series(1, 100000) g`)
	e.mustOops("db", "protect", "--url", db)
	exec1(t, c, "VACUUM oops.audit")
	before := scalar[int64](t, c, "SELECT pg_total_relation_size('oops.audit') + pg_total_relation_size('oops.journal')")
	protBulk := timeIt(bulk)
	after := scalar[int64](t, c, "SELECT pg_total_relation_size('oops.audit') + pg_total_relation_size('oops.journal')")
	protSmall := small()
	after2 := scalar[int64](t, c, "SELECT pg_total_relation_size('oops.audit') + pg_total_relation_size('oops.journal')")
	rowBytes := scalar[int64](t, c, "SELECT avg(pg_column_size(i.*))::bigint FROM items i")
	t.Logf("bulk UPDATE of 100000 rows: %v without protect, %v with (%.1fx)", baseBulk, protBulk, float64(protBulk)/float64(baseBulk))
	t.Logf("500 single row UPDATEs: %v without protect, %v with (%.1fx)", baseSmall, protSmall, float64(protSmall)/float64(baseSmall))
	t.Logf("bytes kept per audited row: %d in bulk statements, %d for single row statements (the row itself is %d bytes)",
		(after-before)/300000, (after2-after)/500, rowBytes)
}
