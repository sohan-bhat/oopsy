package pgproxy

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The DROP statement parser runs inside Postgres, so this needs a server.
func TestDDLTargets(t *testing.T) {
	u := os.Getenv("OOPS_TEST_DATABASE_URL")
	if u == "" {
		t.Skip("set OOPS_TEST_DATABASE_URL")
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) // leave nothing behind
	if _, err := tx.Exec(ctx, schemaSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, protectSQL); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		q, tag string
		nth    int
		want   string
	}{
		{"DROP TABLE a", "DROP TABLE", 1, "a"},
		{"drop  table if exists s.a, \"We,ird\" cascade;", "DROP TABLE", 1, `s.a|"We,ird"`},
		{"/* DROP TABLE x */ DROP TABLE y; -- DROP TABLE z\nDROP TABLE w RESTRICT", "DROP TABLE", 2, "w"},
		{"DELETE FROM a; DROP TABLE b; DROP TABLE c", "DROP TABLE", 2, "c"},
		{"DROP FUNCTION f(int, text), g CASCADE", "DROP FUNCTION", 1, "f(int, text)|g"},
		{"DROP MATERIALIZED VIEW mv", "DROP VIEW", 1, ""},
		{"DROP MATERIALIZED VIEW mv", "DROP MATERIALIZED VIEW", 1, "mv"},
		{"DROP TABLESPACE t", "DROP TABLE", 1, ""},
	}
	for _, c := range cases {
		var got []string
		if err := tx.QueryRow(ctx, "SELECT oops._ddl_targets($1, $2, $3)", c.q, c.tag, c.nth).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, "|") != c.want {
			t.Errorf("%q %s #%d: got %q, want %q", c.q, c.tag, c.nth, strings.Join(got, "|"), c.want)
		}
	}
}
