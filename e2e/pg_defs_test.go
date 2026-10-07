package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

const defsSeed = `
CREATE FUNCTION add_tax(numeric) RETURNS numeric LANGUAGE sql IMMUTABLE AS $$ SELECT $1 * 1.2 $$;
COMMENT ON FUNCTION add_tax(numeric) IS 'adds tax';
REVOKE ALL ON FUNCTION add_tax(numeric) FROM PUBLIC;
CREATE VIEW big_orders WITH (security_barrier = true) AS
  SELECT o.id, o.user_id, add_tax(o.total) AS taxed FROM orders o WHERE o.total > 100;
CREATE VIEW top_big AS SELECT id, taxed FROM big_orders WHERE taxed > 1000;
COMMENT ON VIEW big_orders IS 'orders over 100';
COMMENT ON COLUMN big_orders.taxed IS 'with tax';
GRANT SELECT ON big_orders TO PUBLIC;
CREATE MATERIALIZED VIEW order_totals AS SELECT user_id, sum(total) AS s FROM orders GROUP BY user_id;
CREATE UNIQUE INDEX order_totals_uid ON order_totals (user_id);
CREATE INDEX order_totals_s ON order_totals (s DESC);
COMMENT ON MATERIALIZED VIEW order_totals IS 'totals';
CREATE MATERIALIZED VIEW empty_mv AS SELECT 1 AS x WITH NO DATA;
CREATE PROCEDURE bump(n int) LANGUAGE sql AS $$ UPDATE users SET age = age + n WHERE id = 1 $$;
CREATE FUNCTION touch() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.payload := NEW.payload || '!'; RETURN NEW; END $$;
CREATE TRIGGER events_touch BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION touch();
`

// defsState renders every view, materialized view, function and procedure in
// public with everything that belongs to them, so two states compare exactly.
func defsState(t *testing.T, c *pgx.Conn) string {
	t.Helper()
	return scalar[string](t, c, `SELECT coalesce(string_agg(x, E'\n' ORDER BY x), '') FROM (
	  SELECT format('rel %s %s %s %s %s %s %s', c.relname, c.relkind, pg_get_viewdef(c.oid), c.reloptions, c.relacl,
	                obj_description(c.oid, 'pg_class'), c.relispopulated) AS x
	    FROM pg_class c WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('v', 'm')
	  UNION ALL
	  SELECT format('col %s.%s %s', c.relname, a.attname, col_description(c.oid, a.attnum))
	    FROM pg_class c JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0
	   WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('v', 'm') AND col_description(c.oid, a.attnum) IS NOT NULL
	  UNION ALL
	  SELECT 'idx ' || indexdef FROM pg_indexes WHERE schemaname = 'public'
	  UNION ALL
	  SELECT 'trg ' || pg_get_triggerdef(oid) FROM pg_trigger WHERE NOT tgisinternal
	  UNION ALL
	  SELECT format('fn %s %s %s %s', pg_get_functiondef(p.oid), p.proacl, obj_description(p.oid, 'pg_proc'), pg_get_userbyid(p.proowner))
	    FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace
	) s`)
}

func setupDefs(t *testing.T) (*env, string, *proxyProc, *pgx.Conn, string) {
	t.Helper()
	e, db, p, direct := setupDB(t)
	exec1(t, direct, defsSeed)
	return e, db, p, direct, defsState(t, direct)
}

func sameState(t *testing.T, what, want, got string) {
	t.Helper()
	if want == got {
		return
	}
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	have := map[string]bool{}
	for _, l := range gl {
		have[l] = true
	}
	for _, l := range wl {
		if !have[l] {
			t.Errorf("%s: missing after undo: %s", what, l)
		}
	}
	had := map[string]bool{}
	for _, l := range wl {
		had[l] = true
	}
	for _, l := range gl {
		if !had[l] {
			t.Errorf("%s: unexpected after undo: %s", what, l)
		}
	}
}

func TestPGDropViewCascadeUndo(t *testing.T) {
	e, db, p, direct, before := setupDefs(t)
	rows := fingerprint(t, direct, "top_big", "id")
	client := mustConnect(t, p.url, true)
	exec1(t, client, "DROP VIEW big_orders CASCADE")
	if scalar[bool](t, direct, "SELECT to_regclass('top_big') IS NULL") != true {
		t.Fatal("cascade did not drop the dependent view")
	}
	list, _, _ := e.oopsCmd("", "db", "list", "--url", db)
	if !strings.Contains(list, "DROP VIEW") {
		t.Fatalf("db list: %s", list)
	}
	out := e.dbUndo(t, db)
	if !strings.Contains(out, "view public.big_orders recreated") || !strings.Contains(out, "view public.top_big recreated") {
		t.Fatalf("undo output: %s", out)
	}
	sameState(t, "views", before, defsState(t, direct))
	if fingerprint(t, direct, "top_big", "id") != rows {
		t.Fatal("dependent view returns different rows")
	}
	size := scalar[int](t, direct, "SELECT avg(pg_column_size(j.*))::int FROM oops.journal j WHERE kind = 'drop_view'")
	t.Logf("a saved view definition costs about %d bytes in oops.journal", size)
}

func TestPGDropMaterializedViewUndo(t *testing.T) {
	e, db, p, direct, before := setupDefs(t)
	data := fingerprint(t, direct, "order_totals", "user_id")
	client := mustConnect(t, p.url, false)
	exec1(t, client, "DROP MATERIALIZED VIEW order_totals, empty_mv")
	out := e.dbUndo(t, db)
	if !strings.Contains(out, "materialized view public.order_totals recreated and refreshed") {
		t.Fatalf("undo output: %s", out)
	}
	sameState(t, "materialized views", before, defsState(t, direct))
	if fingerprint(t, direct, "order_totals", "user_id") != data {
		t.Fatal("materialized view data differs after refresh")
	}
	if scalar[bool](t, direct, "SELECT relispopulated FROM pg_class WHERE oid = 'empty_mv'::regclass") {
		t.Fatal("an unpopulated materialized view came back populated")
	}
}

func TestPGDropFunctionCascadeUndo(t *testing.T) {
	e, db, p, direct, before := setupDefs(t)
	client := mustConnect(t, p.url, false)
	// Extended protocol, and CASCADE takes both views built on the function.
	exec1(t, client, "DROP FUNCTION add_tax(numeric) CASCADE")
	exec1(t, client, "DROP FUNCTION touch CASCADE")
	if scalar[bool](t, direct, "SELECT to_regclass('big_orders') IS NULL") != true {
		t.Fatal("cascade did not drop the views")
	}
	out := e.dbUndo(t, db)
	if !strings.Contains(out, "function public.touch() recreated") {
		t.Fatalf("undo output: %s", out)
	}
	out = e.dbUndo(t, db)
	if !strings.Contains(out, "function public.add_tax(numeric) recreated") || !strings.Contains(out, "view public.top_big recreated") {
		t.Fatalf("undo output: %s", out)
	}
	sameState(t, "functions", before, defsState(t, direct))
	// The restored trigger fires again.
	if v := scalar[string](t, direct, "INSERT INTO events VALUES ('t', 'x') RETURNING payload"); v != "x!" {
		t.Fatalf("trigger not restored: %q", v)
	}
}

func TestPGDropProcedureUndo(t *testing.T) {
	e, db, p, direct, before := setupDefs(t)
	client := mustConnect(t, p.url, true)
	exec1(t, client, "DROP PROCEDURE bump(int)")
	out := e.dbUndo(t, db)
	if !strings.Contains(out, "procedure public.bump(integer) recreated") {
		t.Fatalf("undo output: %s", out)
	}
	sameState(t, "procedures", before, defsState(t, direct))
	exec1(t, direct, "CALL bump(1)")
	_ = context.Background()
}
