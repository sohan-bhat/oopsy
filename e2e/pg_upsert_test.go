package e2e

import (
	"context"
	"strings"
	"testing"
)

func TestPGUpsertSimpleProtocol(t *testing.T) {
	e, db, p, direct := setupDB(t)
	fp := fingerprint(t, direct, "users", "id")
	client := mustConnect(t, p.url, true)
	exec1(t, client, `INSERT INTO users (id, email, name, age) VALUES
	    (1, 'user1@example.com', 'changed', 99),
	    (2, 'user2@example.com', NULL, 1),
	    (9001, 'fresh@example.com', 'fresh', 5)
	  ON CONFLICT (id) DO UPDATE SET name = excluded.name, age = excluded.age, meta = NULL`)
	if n := scalar[int](t, direct, "SELECT count(*) FROM users WHERE name = 'changed' OR id = 9001"); n != 2 {
		t.Fatal("upsert did not run")
	}
	list, _, _ := e.oopsCmd("", "db", "list", "--url", db)
	if !strings.Contains(list, "UPSERT") {
		t.Fatalf("db list: %s", list)
	}
	out := e.dbUndo(t, db)
	if !strings.Contains(out, "2 rows in public.users set back to their old values, 1 rows the upsert inserted removed") {
		t.Fatalf("undo output: %s", out)
	}
	if fingerprint(t, direct, "users", "id") != fp {
		t.Fatal("users not restored exactly")
	}
	// Cost: a bulk upsert of 20000 rows, 500 of them overwriting old rows.
	exec1(t, client, `INSERT INTO users (id, email, name, age) SELECT g, 'user' || g || '@example.com', 'bulk', 1
	  FROM generate_series(1, 20000) g ON CONFLICT (id) DO UPDATE SET name = excluded.name`)
	var oldB, inB int64
	if err := direct.QueryRow(context.Background(), `SELECT pg_total_relation_size(('oops.' || quote_ident(snap))::regclass),
	    pg_total_relation_size(('oops.' || quote_ident(meta->>'incoming'))::regclass)
	  FROM oops.journal WHERE kind = 'upsert' AND restored_at IS NULL`).Scan(&oldB, &inB); err != nil {
		t.Fatal(err)
	}
	t.Logf("bulk upsert of 20000 rows: %d bytes for the 500 overwritten rows (%d per row), %d bytes for the incoming rows (%d per row)",
		oldB, oldB/500, inB, inB/20000)
	e.dbUndo(t, db)
	if fingerprint(t, direct, "users", "id") != fp {
		t.Fatal("users not restored exactly after the bulk upsert")
	}
}

func TestPGUpsertExtendedProtocolKeepsChangedRows(t *testing.T) {
	e, db, p, direct := setupDB(t)
	fp := fingerprint(t, direct, "users", "id")
	client := mustConnect(t, p.url, false)
	ctx := context.Background()
	q := "INSERT INTO users (email, name, age) VALUES ($1, $2, $3), ($4, $5, $6) ON CONFLICT (email) DO UPDATE SET name = excluded.name || '!'"
	if _, err := client.Exec(ctx, q, "user7@example.com", "seven", 7, "brand@new", "new", 30); err != nil {
		t.Fatal(err)
	}
	if v := scalar[string](t, direct, "SELECT name FROM users WHERE email = 'user7@example.com'"); v != "seven!" {
		t.Fatalf("upsert did not update: %q", v)
	}
	// A second upsert in the same prepared statement, then someone edits the
	// row the first one inserted.
	if _, err := client.Exec(ctx, q, "user8@example.com", "eight", 8, "other@new", "other", 31); err != nil {
		t.Fatal(err)
	}
	exec1(t, direct, "UPDATE users SET age = 77 WHERE email = 'brand@new'")
	out := e.dbUndo(t, db)
	if !strings.Contains(out, "1 rows the upsert inserted removed") {
		t.Fatalf("undo output: %s", out)
	}
	out = e.dbUndo(t, db)
	if !strings.Contains(out, "1 inserted rows changed since and were kept") {
		t.Fatalf("undo output: %s", out)
	}
	exec1(t, direct, "DELETE FROM users WHERE email = 'brand@new'")
	if fingerprint(t, direct, "users", "id") != fp {
		t.Fatal("users not restored exactly")
	}
}

func TestPGUpsertOnConstraintWithoutColumnList(t *testing.T) {
	e, db, p, direct := setupDB(t)
	fp := fingerprint(t, direct, "users", "id")
	client := mustConnect(t, p.url, true)
	// Untyped literals for jsonb and timestamptz, and no column list.
	exec1(t, client, `INSERT INTO users VALUES (3, 'user3@example.com', 'n', 7, '{"a": 1}', '2030-01-01 00:00:00+00'),
	    (4, 'user4@example.com', 'm', 8, NULL, '2030-01-01')
	  ON CONFLICT ON CONSTRAINT users_pkey DO UPDATE SET meta = excluded.meta, created_at = excluded.created_at`)
	e.dbUndo(t, db)
	if fingerprint(t, direct, "users", "id") != fp {
		t.Fatal("users not restored exactly")
	}
}

func TestPGUpsertWithDefaultKeyFallsBackToWholeTable(t *testing.T) {
	e, db, p, direct := setupDB(t)
	fp := fingerprint(t, direct, "users", "id")
	client := mustConnect(t, p.url, true)
	// The conflict column comes from a default, so the proxy keeps the table.
	exec1(t, client, `INSERT INTO users (id, email) VALUES (DEFAULT, 'user5@example.com') ON CONFLICT (email) DO UPDATE SET name = 'x'`)
	out := e.dbUndo(t, db)
	if !strings.Contains(out, "reset to the 500 saved rows") {
		t.Fatalf("undo output: %s", out)
	}
	if fingerprint(t, direct, "users", "id") != fp {
		t.Fatal("users not restored exactly")
	}
}
