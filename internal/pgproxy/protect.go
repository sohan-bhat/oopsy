package pgproxy

import (
	"context"
	_ "embed"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

//go:embed protect.sql
var protectSQL string

// unprotectSQL removes the triggers, event triggers and functions that
// protect.sql added. It keeps oops.audit and the journal, which hold the
// changes saved so far, so they can still be undone.
const unprotectSQL = `
DROP EVENT TRIGGER IF EXISTS oops_protect_start;
DROP EVENT TRIGGER IF EXISTS oops_protect_drop;
DROP EVENT TRIGGER IF EXISTS oops_protect_end;
DO $u$
DECLARE
  r record;
BEGIN
  FOR r IN SELECT tgrelid::regclass AS rel, tgname FROM pg_trigger
            WHERE tgname IN ('oops_audit_delete', 'oops_audit_update', 'oops_audit_truncate') LOOP
    EXECUTE format('DROP TRIGGER %I ON %s', r.tgname, r.rel);
  END LOOP;
END
$u$;
DROP FUNCTION IF EXISTS oops.audit_delete(), oops.audit_update(), oops.audit_truncate(),
  oops.protect_table(oid), oops._ddl_targets(text, text, int), oops._ddl_nth(text), oops._row_def(oid),
  oops.protect_ddl_start(), oops.protect_ddl_drop(), oops.protect_ddl_end();
`

// ProtectStatus reports whether protect is on and how many tables it covers.
func ProtectStatus(ctx context.Context, conn *pgx.Conn) (on bool, tables int, err error) {
	err = conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_event_trigger WHERE evtname = 'oops_protect_start'),
		(SELECT count(*) FROM pg_trigger WHERE tgname = 'oops_audit_delete')`).Scan(&on, &tables)
	return
}

// Protect installs the audit triggers on every user table and the event
// triggers that cover DDL and new tables. It needs a superuser, because
// Postgres only lets superusers create event triggers.
func Protect(ctx context.Context, upstream string) (int, error) {
	if err := Setup(ctx, upstream); err != nil {
		return 0, fmt.Errorf("installing the oops schema: %w", err)
	}
	conn, err := pgx.Connect(ctx, upstream)
	if err != nil {
		return 0, err
	}
	defer conn.Close(ctx)
	var super bool
	if err := conn.QueryRow(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname = current_user").Scan(&super); err != nil {
		return 0, err
	}
	if !super {
		return 0, errors.New("oops db protect needs a superuser connection, because Postgres only lets superusers create event triggers")
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(7123007)"); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, protectSQL); err != nil {
		return 0, fmt.Errorf("installing the protect triggers: %w", err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE oops.protect_table(oid)) FROM pg_class WHERE relkind IN ('r', 'p')`).Scan(&n); err != nil {
		return 0, fmt.Errorf("adding triggers to tables: %w", err)
	}
	return n, tx.Commit(ctx)
}

// Unprotect removes everything Protect added except the saved changes.
func Unprotect(ctx context.Context, upstream string) error {
	conn, err := pgx.Connect(ctx, upstream)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(7123007)"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, unprotectSQL); err != nil {
		return fmt.Errorf("removing the protect triggers: %w", err)
	}
	return tx.Commit(ctx)
}
