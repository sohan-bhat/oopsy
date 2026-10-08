package pgproxy

import (
	"strings"
	"testing"
)

func TestPlanKinds(t *testing.T) {
	cases := []struct {
		sql, kind string
		want      []string // substrings expected in the snapshot SQL
		params    bool
	}{
		{"DELETE FROM users WHERE id = 3", "DELETE", []string{`CREATE TABLE oops.`, `SELECT users.* FROM users WHERE id = 3`, `record_rows(E'delete', E'"users"'`}, false},
		{"delete from public.users u where u.age > $1", "DELETE", []string{`SELECT u.* FROM public.users u WHERE u.age > $1`}, true},
		{"DELETE FROM t USING s WHERE t.id = s.id", "DELETE", []string{`t.ctid IN (SELECT t.ctid FROM t, s WHERE t.id = s.id)`}, false},
		{"UPDATE accounts SET balance = 0", "UPDATE", []string{`SELECT accounts.* FROM accounts`, `record_rows(E'update'`}, false},
		{"UPDATE t SET a = $1 WHERE id = $2", "UPDATE", []string{`WHERE id = $2`}, true},
		{"TRUNCATE a, b CASCADE", "TRUNCATE", []string{`snap_table(E'"a"', `, `snap_table(E'"b"'`, `true)`}, false},
		{"DROP TABLE IF EXISTS s.orders, items", "DROP TABLE", []string{`snap_drop_table(E'"s"."orders"'`, `snap_drop_table(E'"items"'`}, false},
		{"DROP SCHEMA app CASCADE", "DROP SCHEMA", []string{`snap_drop_schema(E'app'`}, false},
		{"ALTER TABLE users DROP COLUMN email, ADD COLUMN x int", "DROP COLUMN", []string{`snap_column(E'"users"', E'email'`}, false},
		{"WITH d AS (DELETE FROM logs RETURNING *) SELECT count(*) FROM d", "nested DML", []string{`snap_table(E'"logs"'`}, false},
		{"MERGE INTO t USING s ON t.id = s.id WHEN MATCHED THEN DELETE", "MERGE", []string{`snap_table(E'"t"'`}, false},
		{"DELETE FROM \"Weird \"\"Name\" WHERE note = 'it''s'", "DELETE", []string{`"Weird ""Name"`, `it''s`}, false},
		{"DROP VIEW IF EXISTS app.v, w CASCADE", "DROP VIEW", []string{`snap_drop_view(E'"app"."v"'`, `snap_drop_view(E'"w"'`}, false},
		{"DROP MATERIALIZED VIEW mv", "DROP MATERIALIZED VIEW", []string{`snap_drop_view(E'"mv"'`}, false},
		{"DROP FUNCTION s.f(int, character varying(10), OUT x text)", "DROP FUNCTION", []string{`snap_drop_routine(E'"s"."f"(int, varchar(10))'`}, false},
		{"DROP FUNCTION g CASCADE", "DROP FUNCTION", []string{`snap_drop_routine(E'"g"'`}, false},
		{"INSERT INTO t (id, v) VALUES (1, 'a'), ($1, $2) ON CONFLICT (id) DO UPDATE SET v = excluded.v", "UPSERT",
			[]string{`AS SELECT * FROM (VALUES (1, 'a'), ($1, $2)) oops_src(id, v)`, `record_upsert(E'"t"', ARRAY[E'id', E'v']::text[]`, `ARRAY[E'id']::text[], E''`}, true},
		{"INSERT INTO t SELECT * FROM s ON CONFLICT ON CONSTRAINT t_pkey DO UPDATE SET v = 1", "UPSERT", []string{`'{}'::text[]`, `E't_pkey'`}, false},
		{"INSERT INTO t (id) VALUES (DEFAULT) ON CONFLICT (id) DO UPDATE SET v = 1", "UPSERT", []string{`snap_table(E'"t"'`}, false},
		{"INSERT INTO t (id) VALUES (nextval('s')) ON CONFLICT (id) DO UPDATE SET v = 1", "UPSERT", []string{`snap_table(E'"t"'`}, false},
		{"INSERT INTO t (id) VALUES (1) ON CONFLICT (id) WHERE active DO UPDATE SET v = 1", "UPSERT", []string{`snap_table(E'"t"'`}, false},
		{"DROP PROCEDURE p(IN a int)", "DROP PROCEDURE", []string{`snap_drop_routine(E'"p"(int)'`}, false},
	}
	for _, c := range cases {
		plans, err := Plan(c.sql)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		if len(plans) != 1 {
			t.Fatalf("%s: want 1 plan, got %d", c.sql, len(plans))
		}
		p := plans[0]
		if p.Kind != c.kind {
			t.Errorf("%s: kind %q, want %q", c.sql, p.Kind, c.kind)
		}
		var all []string
		uses := false
		for _, s := range p.Snaps {
			all = append(all, s.SQL)
			uses = uses || s.UsesParams
		}
		joined := strings.Join(all, "\n")
		for _, w := range c.want {
			if !strings.Contains(joined, w) {
				t.Errorf("%s: snapshot SQL missing %q:\n%s", c.sql, w, joined)
			}
		}
		if uses != c.params {
			t.Errorf("%s: uses params %v, want %v", c.sql, uses, c.params)
		}
	}
}

func TestPlanIgnoresHarmless(t *testing.T) {
	for _, q := range []string{
		"SELECT * FROM users",
		"INSERT INTO t VALUES (1)",
		"INSERT INTO t VALUES (1) ON CONFLICT DO NOTHING",
		"SELECT 'delete from x'",
		"CREATE TABLE updates (id int)",
		"ALTER TABLE t ADD COLUMN c int",
		"DROP INDEX idx",
		"SELECT * FROM updated_rows",
	} {
		plans, err := Plan(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if len(plans) != 0 {
			t.Errorf("%s: expected no plan, got %+v", q, plans)
		}
	}
}

func TestPlanMultiStatementLocations(t *testing.T) {
	q := "SELECT 1; DELETE FROM a; SELECT 2; DROP TABLE b"
	locs, plans, err := PlanAll(q)
	if err != nil {
		t.Fatal(err)
	}
	if len(locs) != 4 || len(plans) != 2 {
		t.Fatalf("locs %v plans %d", locs, len(plans))
	}
	if plans[0].Location != locs[1] || plans[1].Location != locs[3] {
		t.Fatalf("locations do not line up: %v %d %d", locs, plans[0].Location, plans[1].Location)
	}
	if plans[0].Text != "DELETE FROM a" {
		t.Fatalf("text %q", plans[0].Text)
	}
}
