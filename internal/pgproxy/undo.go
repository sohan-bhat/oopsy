package pgproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Entry is one row of oops.journal.
type Entry struct {
	ID        int64
	Group     string
	Created   time.Time
	Kind      string
	Schema    string
	Table     string
	Column    string
	Snap      string
	KeyCols   []string
	ColDef    string
	DDL       string
	DDLPost   []string
	Meta      map[string]any
	Rows      int64
	Statement string
	Restored  *time.Time
}

// Change is everything one statement destroyed.
type Change struct {
	ID        int64 // smallest journal id in the group
	Group     string
	Created   time.Time
	Statement string
	Entries   []Entry
	Restored  *time.Time
}

// Rows is the total number of rows saved.
func (c Change) Rows() int64 {
	var n int64
	for _, e := range c.Entries {
		n += e.Rows
	}
	return n
}

// Tables lists the tables touched.
func (c Change) Tables() []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range c.Entries {
		t := e.Schema
		if e.Table != "" {
			t = e.Schema + "." + e.Table
		}
		if e.Column != "" {
			t += "." + e.Column
		}
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// Kind summarizes the change type.
func (c Change) Kind() string {
	for _, e := range c.Entries {
		if e.Kind == "drop_schema" {
			return "drop_schema"
		}
	}
	if len(c.Entries) > 0 {
		return c.Entries[0].Kind
	}
	return ""
}

func hasJournal(ctx context.Context, conn *pgx.Conn) (bool, error) {
	var ok bool
	err := conn.QueryRow(ctx, "SELECT to_regclass('oops.journal') IS NOT NULL").Scan(&ok)
	return ok, err
}

// ListChanges returns recent changes, newest first.
func ListChanges(ctx context.Context, conn *pgx.Conn, limit int) ([]Change, error) {
	ok, err := hasJournal(ctx, conn)
	if err != nil || !ok {
		return nil, err
	}
	rows, err := conn.Query(ctx, `
		SELECT id, grp, created_at, kind, coalesce(rel_schema,''), coalesce(rel_name,''), coalesce(col_name,''),
		       coalesce(snap,''), coalesce(key_cols, '{}'), coalesce(col_def,''), coalesce(ddl,''), coalesce(ddl_post,'{}'),
		       meta, coalesce(row_count,0), coalesce(statement,''), restored_at
		  FROM oops.journal
		 WHERE grp IN (SELECT grp FROM oops.journal GROUP BY grp ORDER BY min(id) DESC LIMIT $1)
		 ORDER BY id`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byGroup := map[string]*Change{}
	var order []string
	for rows.Next() {
		var e Entry
		var meta []byte
		if err := rows.Scan(&e.ID, &e.Group, &e.Created, &e.Kind, &e.Schema, &e.Table, &e.Column, &e.Snap, &e.KeyCols,
			&e.ColDef, &e.DDL, &e.DDLPost, &meta, &e.Rows, &e.Statement, &e.Restored); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(meta, &e.Meta)
		c, ok := byGroup[e.Group]
		if !ok {
			c = &Change{ID: e.ID, Group: e.Group, Created: e.Created, Statement: e.Statement}
			byGroup[e.Group] = c
			order = append(order, e.Group)
		}
		if e.Restored != nil {
			c.Restored = e.Restored
		}
		c.Entries = append(c.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []Change
	for _, g := range order {
		out = append(out, *byGroup[g])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// FindChange returns the change containing journal id, or the newest
// unrestored change when id is 0.
func FindChange(ctx context.Context, conn *pgx.Conn, id int64) (*Change, error) {
	list, err := ListChanges(ctx, conn, 1000)
	if err != nil {
		return nil, err
	}
	for _, c := range list {
		if id == 0 && c.Restored == nil {
			return &c, nil
		}
		for _, e := range c.Entries {
			if e.ID == id {
				return &c, nil
			}
		}
	}
	if id == 0 {
		return nil, nil
	}
	return nil, &NotFoundError{ID: id}
}

func qi(s string) string { return quoteIdent(s) }

func (e Entry) qualified() string { return qi(e.Schema) + "." + qi(e.Table) }

// Describe explains what undoing c will do.
func Describe(c Change) []string {
	var out []string
	for _, e := range c.Entries {
		switch e.Kind {
		case "delete", "truncate":
			out = append(out, fmt.Sprintf("put back %d rows into %s.%s", e.Rows, e.Schema, e.Table))
		case "update":
			out = append(out, fmt.Sprintf("restore the old values of %d rows in %s.%s", e.Rows, e.Schema, e.Table))
		case "upsert":
			fresh, _ := e.Meta["fresh"].(float64)
			out = append(out, fmt.Sprintf("restore the old values of %d rows in %s.%s that the upsert overwrote, and remove the %d rows it inserted", e.Rows, e.Schema, e.Table, int64(fresh)))
		case "update_full":
			out = append(out, fmt.Sprintf("replace all rows of %s.%s with the %d saved rows (no key to match rows on)", e.Schema, e.Table, e.Rows))
		case "drop_table":
			out = append(out, fmt.Sprintf("recreate table %s.%s with %d rows, its indexes and constraints", e.Schema, e.Table, e.Rows))
		case "drop_schema":
			out = append(out, fmt.Sprintf("recreate schema %s", e.Schema))
		case "drop_column":
			out = append(out, fmt.Sprintf("add column %s back to %s.%s and refill %d values", e.Column, e.Schema, e.Table, e.Rows))
		case "drop_view", "drop_function":
			out = append(out, "recreate "+e.defName())
		case "drop_matview":
			l := "recreate " + e.defName()
			if pop, _ := e.Meta["populated"].(bool); pop {
				l += " and refresh it"
			}
			out = append(out, l)
		}
	}
	return out
}

// Result reports an undo.
type Result struct {
	Lines    []string
	Warnings []string
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Undo restores change c in one transaction.
func Undo(ctx context.Context, conn *pgx.Conn, c Change) (*Result, error) {
	res := &Result{}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Phase 1: schemas and tables, phase 2: data, phase 3: constraints and views.
	var drops []Entry
	for _, e := range c.Entries {
		if e.Kind == "drop_schema" {
			if _, err := tx.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+qi(e.Schema)); err != nil {
				return nil, err
			}
			res.Lines = append(res.Lines, "schema "+e.Schema+" recreated")
		}
	}
	for _, e := range c.Entries {
		if e.Kind != "drop_table" {
			continue
		}
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", e.qualified()).Scan(&exists); err != nil {
			return nil, err
		}
		if exists {
			return nil, fmt.Errorf("table %s.%s exists again; rename or drop it first so nothing gets overwritten", e.Schema, e.Table)
		}
		if _, err := tx.Exec(ctx, e.DDL); err != nil {
			return nil, fmt.Errorf("recreating %s.%s: %w", e.Schema, e.Table, err)
		}
		drops = append(drops, e)
	}
	for _, e := range drops {
		n, err := insertRows(ctx, tx, e, false)
		if err != nil {
			return nil, err
		}
		if err := setSequences(ctx, tx, e); err != nil {
			res.Warnings = append(res.Warnings, err.Error())
		}
		res.Lines = append(res.Lines, fmt.Sprintf("table %s.%s recreated with %d rows", e.Schema, e.Table, n))
	}
	for _, e := range drops {
		for _, stmt := range e.DDLPost {
			if err := savepointExec(ctx, tx, stmt); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("could not rerun %q: %v", shorten(stmt, 80), err))
			}
		}
	}

	// Row level restores, retrying to satisfy foreign keys between tables.
	var pending []Entry
	for _, e := range c.Entries {
		switch e.Kind {
		case "delete", "truncate", "update", "update_full", "upsert":
			pending = append(pending, e)
		}
	}
	for pass := 0; len(pending) > 0 && pass <= len(pending); pass++ {
		var retry []Entry
		var lastErr error
		for _, e := range pending {
			line, err := restoreRows(ctx, tx, e)
			if err != nil {
				lastErr = err
				retry = append(retry, e)
				continue
			}
			res.Lines = append(res.Lines, line)
		}
		if len(retry) == len(pending) {
			return nil, lastErr
		}
		pending = retry
	}

	for _, e := range c.Entries {
		if e.Kind != "drop_column" {
			continue
		}
		line, err := restoreColumn(ctx, tx, e)
		if err != nil {
			return nil, err
		}
		res.Lines = append(res.Lines, line)
	}

	// Functions first, then views in the order they were saved, which puts a
	// view after the ones it is built on. Retry for views saved out of order.
	var defs []Entry
	for _, e := range c.Entries {
		if e.Kind == "drop_function" {
			defs = append(defs, e)
		}
	}
	for _, e := range c.Entries {
		if e.Kind == "drop_view" || e.Kind == "drop_matview" {
			defs = append(defs, e)
		}
	}
	for pass := 0; len(defs) > 0; pass++ {
		var retry []Entry
		var lastErr error
		for _, e := range defs {
			line, warns, err := restoreDefinition(ctx, tx, e)
			if err != nil {
				lastErr = err
				retry = append(retry, e)
				continue
			}
			res.Lines = append(res.Lines, line)
			res.Warnings = append(res.Warnings, warns...)
		}
		if len(retry) == len(defs) {
			return nil, lastErr
		}
		defs = retry
	}

	if _, err := tx.Exec(ctx, "UPDATE oops.journal SET restored_at = now() WHERE grp = $1", c.Group); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return res, nil
}

// defName is how a saved definition is shown to people.
func (e Entry) defName() string {
	switch e.Kind {
	case "drop_function":
		if r, _ := e.Meta["routine"].(string); r != "" {
			return r + " " + e.Schema + "." + e.Table
		}
		return "function " + e.Schema + "." + e.Table
	case "drop_matview":
		return "materialized view " + e.Schema + "." + e.Table
	}
	return "view " + e.Schema + "." + e.Table
}

// restoreDefinition recreates a dropped view, materialized view, function or
// procedure, then reruns what went with it: owner, grants, comments, indexes,
// triggers, column defaults and a refresh for a materialized view that held
// data.
func restoreDefinition(ctx context.Context, tx pgx.Tx, e Entry) (string, []string, error) {
	var exists bool
	var err error
	if e.Kind == "drop_function" {
		sig, _ := e.Meta["sig"].(string)
		err = tx.QueryRow(ctx, "SELECT to_regprocedure($1) IS NOT NULL", sig).Scan(&exists)
	} else {
		err = tx.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", e.qualified()).Scan(&exists)
	}
	if err != nil {
		return "", nil, err
	}
	if exists {
		return "", nil, fmt.Errorf("%s exists again; rename or drop it first so nothing gets overwritten", e.defName())
	}
	if err := savepointExec(ctx, tx, e.DDL); err != nil {
		return "", nil, fmt.Errorf("recreating %s: %w", e.defName(), err)
	}
	var warns []string
	for _, stmt := range e.DDLPost {
		if err := savepointExec(ctx, tx, stmt); err != nil {
			warns = append(warns, fmt.Sprintf("could not rerun %q: %v", shorten(stmt, 80), err))
		}
	}
	line := e.defName() + " recreated"
	if pop, _ := e.Meta["populated"].(bool); pop && e.Kind == "drop_matview" {
		line += " and refreshed"
	}
	return line, warns, nil
}

func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n-3] + "..."
	}
	return s
}

func savepointExec(ctx context.Context, tx pgx.Tx, stmt string) error {
	if _, err := tx.Exec(ctx, "SAVEPOINT oopsy_sp"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, stmt); err != nil {
		_, _ = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT oopsy_sp")
		return err
	}
	_, err := tx.Exec(ctx, "RELEASE SAVEPOINT oopsy_sp")
	return err
}

type colInfo struct {
	name     string
	identity string
	generate bool
}

// columns returns the current columns of a table.
func columns(ctx context.Context, q execer, rel string) ([]colInfo, error) {
	rows, err := q.Query(ctx, `SELECT attname, attidentity::text, attgenerated <> '' FROM pg_attribute
		WHERE attrelid = to_regclass($1) AND attnum > 0 AND NOT attisdropped ORDER BY attnum`, rel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []colInfo
	for rows.Next() {
		var c colInfo
		if err := rows.Scan(&c.name, &c.identity, &c.generate); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 && rows.Err() == nil {
		return nil, fmt.Errorf("table %s does not exist", rel)
	}
	return out, rows.Err()
}

// insertable lists columns present in both the table and the snapshot that
// can be written directly.
func insertable(ctx context.Context, q execer, e Entry) ([]string, error) {
	cols, err := columns(ctx, q, e.qualified())
	if err != nil {
		return nil, err
	}
	snapCols, err := columns(ctx, q, "oops."+qi(e.Snap))
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, c := range snapCols {
		have[c.name] = true
	}
	var out []string
	for _, c := range cols {
		if c.generate || !have[c.name] {
			continue
		}
		out = append(out, qi(c.name))
	}
	return out, nil
}

func insertRows(ctx context.Context, q execer, e Entry, skipConflicts bool) (int64, error) {
	cols, err := insertable(ctx, q, e)
	if err != nil {
		return 0, err
	}
	list := strings.Join(cols, ", ")
	sql := fmt.Sprintf("INSERT INTO %s (%s) OVERRIDING SYSTEM VALUE SELECT %s FROM oops.%s", e.qualified(), list, list, qi(e.Snap))
	if skipConflicts {
		sql += " ON CONFLICT DO NOTHING"
	}
	tag, err := q.Exec(ctx, sql)
	if err != nil {
		return 0, fmt.Errorf("restoring rows into %s.%s: %w", e.Schema, e.Table, err)
	}
	return tag.RowsAffected(), nil
}

func setSequences(ctx context.Context, q execer, e Entry) error {
	set := func(seq string, v map[string]any) error {
		last, _ := v["last_value"].(float64)
		called, _ := v["is_called"].(bool)
		_, err := q.Exec(ctx, "SELECT setval($1::regclass, $2::bigint, $3)", seq, int64(last), called)
		return err
	}
	if seqs, ok := e.Meta["sequences"].(map[string]any); ok {
		for name, v := range seqs {
			if m, ok := v.(map[string]any); ok {
				if err := set(name, m); err != nil {
					return err
				}
			}
		}
	}
	if ids, ok := e.Meta["identity"].(map[string]any); ok {
		for col, v := range ids {
			var seq *string
			if err := q.QueryRow(ctx, "SELECT pg_get_serial_sequence($1, $2)", e.qualified(), col).Scan(&seq); err != nil || seq == nil {
				continue
			}
			if m, ok := v.(map[string]any); ok {
				if err := set(*seq, m); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func restoreRows(ctx context.Context, tx pgx.Tx, e Entry) (string, error) {
	if _, err := tx.Exec(ctx, "SAVEPOINT oopsy_rows"); err != nil {
		return "", err
	}
	line, err := restoreRowsInner(ctx, tx, e)
	if err != nil {
		_, _ = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT oopsy_rows")
		return "", err
	}
	_, err = tx.Exec(ctx, "RELEASE SAVEPOINT oopsy_rows")
	return line, err
}

func restoreRowsInner(ctx context.Context, tx pgx.Tx, e Entry) (string, error) {
	switch e.Kind {
	case "delete", "truncate":
		n, err := insertRows(ctx, tx, e, true)
		if err != nil {
			return "", err
		}
		line := fmt.Sprintf("%d rows put back into %s.%s", n, e.Schema, e.Table)
		if n < e.Rows {
			line += fmt.Sprintf(" (%d already existed and were left alone)", e.Rows-n)
		}
		return line, nil
	case "update_full":
		if _, err := tx.Exec(ctx, "DELETE FROM "+e.qualified()); err != nil {
			return "", err
		}
		n, err := insertRows(ctx, tx, e, false)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s.%s reset to the %d saved rows", e.Schema, e.Table, n), nil
	case "update":
		updated, inserted, err := restoreOldValues(ctx, tx, e)
		if err != nil {
			return "", err
		}
		line := fmt.Sprintf("%d rows in %s.%s set back to their old values", updated, e.Schema, e.Table)
		if inserted > 0 {
			line += fmt.Sprintf(", %d missing rows put back", inserted)
		}
		return line, nil
	case "upsert":
		updated, inserted, err := restoreOldValues(ctx, tx, e)
		if err != nil {
			return "", err
		}
		removed, kept, err := removeFresh(ctx, tx, e)
		if err != nil {
			return "", err
		}
		line := fmt.Sprintf("%d rows in %s.%s set back to their old values, %d rows the upsert inserted removed", updated, e.Schema, e.Table, removed)
		if inserted > 0 {
			line += fmt.Sprintf(", %d missing rows put back", inserted)
		}
		if kept > 0 {
			line += fmt.Sprintf(", %d inserted rows changed since and were kept", kept)
		}
		return line, nil
	}
	return "", errors.New("unknown change kind " + e.Kind)
}

// restoreOldValues sets rows matched on the key columns back to the saved
// values and puts back saved rows that no longer exist.
func restoreOldValues(ctx context.Context, tx pgx.Tx, e Entry) (int64, int64, error) {
	cols, err := columns(ctx, tx, e.qualified())
	if err != nil {
		return 0, 0, err
	}
	key := map[string]bool{}
	for _, k := range e.KeyCols {
		key[k] = true
	}
	snapCols, err := columns(ctx, tx, "oops."+qi(e.Snap))
	if err != nil {
		return 0, 0, err
	}
	have := map[string]bool{}
	for _, c := range snapCols {
		have[c.name] = true
	}
	var set, src []string
	for _, c := range cols {
		if c.generate || c.identity == "a" || key[c.name] || !have[c.name] {
			continue
		}
		set = append(set, qi(c.name))
		src = append(src, "s."+qi(c.name))
	}
	var on []string
	for _, k := range e.KeyCols {
		on = append(on, fmt.Sprintf("t.%s = s.%s", qi(k), qi(k)))
	}
	var updated int64
	if len(set) > 0 && len(on) > 0 {
		sql := fmt.Sprintf("UPDATE %s t SET (%s) = ROW(%s) FROM oops.%s s WHERE %s",
			e.qualified(), strings.Join(set, ", "), strings.Join(src, ", "), qi(e.Snap), strings.Join(on, " AND "))
		tag, err := tx.Exec(ctx, sql)
		if err != nil {
			return 0, 0, fmt.Errorf("restoring old values in %s.%s: %w", e.Schema, e.Table, err)
		}
		updated = tag.RowsAffected()
	}
	// Rows deleted since the update come back as well.
	inserted, err := insertRows(ctx, tx, e, true)
	if err != nil {
		return 0, 0, err
	}
	return updated, inserted, nil
}

// removeFresh deletes the rows an upsert inserted, that is incoming rows
// whose key had no old row, but only while every column the statement gave
// still holds the value it wrote. Rows changed since are kept and counted.
func removeFresh(ctx context.Context, tx pgx.Tx, e Entry) (int64, int64, error) {
	in, _ := e.Meta["incoming"].(string)
	if in == "" || len(e.KeyCols) == 0 {
		return 0, 0, nil
	}
	var given []string
	if l, ok := e.Meta["cols"].([]any); ok {
		for _, c := range l {
			if s, ok := c.(string); ok {
				given = append(given, s)
			}
		}
	}
	cols, err := columns(ctx, tx, e.qualified())
	if err != nil {
		return 0, 0, err
	}
	present := map[string]bool{}
	for _, c := range cols {
		if !c.generate {
			present[c.name] = true
		}
	}
	var keyT, keyO, same []string
	for _, k := range e.KeyCols {
		keyT = append(keyT, fmt.Sprintf("t.%s = i.%s", qi(k), qi(k)))
		keyO = append(keyO, fmt.Sprintf("o.%s = i.%s", qi(k), qi(k)))
	}
	for _, c := range given {
		if present[c] {
			same = append(same, fmt.Sprintf("t.%s::text IS NOT DISTINCT FROM i.%s::text", qi(c), qi(c)))
		}
	}
	from := fmt.Sprintf("FROM oops.%s i WHERE %s AND NOT EXISTS (SELECT 1 FROM oops.%s o WHERE %s)",
		qi(in), strings.Join(keyT, " AND "), qi(e.Snap), strings.Join(keyO, " AND "))
	var candidates int64
	if err := tx.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s t, %s", e.qualified(), strings.TrimPrefix(from, "FROM "))).Scan(&candidates); err != nil {
		return 0, 0, err
	}
	cond := ""
	if len(same) > 0 {
		cond = " AND " + strings.Join(same, " AND ")
	}
	tag, err := tx.Exec(ctx, fmt.Sprintf("DELETE FROM %s t USING %s%s", e.qualified(), strings.TrimPrefix(from, "FROM "), cond))
	if err != nil {
		return 0, 0, fmt.Errorf("removing rows the upsert inserted into %s.%s: %w", e.Schema, e.Table, err)
	}
	return tag.RowsAffected(), candidates - tag.RowsAffected(), nil
}

func restoreColumn(ctx context.Context, tx pgx.Tx, e Entry) (string, error) {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass($1) AND attname = $2 AND attnum > 0 AND NOT attisdropped)`,
		e.qualified(), e.Column).Scan(&exists); err != nil {
		return "", err
	}
	if exists {
		return "", fmt.Errorf("column %s already exists on %s.%s; drop or rename it first so nothing gets overwritten", e.Column, e.Schema, e.Table)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", e.qualified(), qi(e.Column), e.ColDef)); err != nil {
		return "", err
	}
	if gen, _ := e.Meta["generated"].(bool); gen {
		return fmt.Sprintf("generated column %s added back to %s.%s", e.Column, e.Schema, e.Table), nil
	}
	var on string
	if len(e.KeyCols) > 0 {
		var parts []string
		for _, k := range e.KeyCols {
			parts = append(parts, fmt.Sprintf("t.%s = s.%s", qi(k), qi(k)))
		}
		on = strings.Join(parts, " AND ")
	} else {
		on = "t.ctid = s.oops_ctid::tid"
	}
	tag, err := tx.Exec(ctx, fmt.Sprintf("UPDATE %s t SET %s = s.%s FROM oops.%s s WHERE %s", e.qualified(), qi(e.Column), qi(e.Column), qi(e.Snap), on))
	if err != nil {
		return "", err
	}
	if nn, _ := e.Meta["not_null"].(bool); nn {
		if err := savepointExec(ctx, tx, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", e.qualified(), qi(e.Column))); err != nil {
			return "", err
		}
	}
	extra := ""
	failed := 0
	for _, stmt := range e.DDLPost {
		if err := savepointExec(ctx, tx, stmt); err != nil {
			failed++
		}
	}
	if n := len(e.DDLPost) - failed; n > 0 {
		extra = fmt.Sprintf(", %d dependent constraints, indexes or views recreated", n)
	}
	if failed > 0 {
		extra += fmt.Sprintf(", %d could not be recreated", failed)
	}
	return fmt.Sprintf("column %s added back to %s.%s, %d values refilled%s", e.Column, e.Schema, e.Table, tag.RowsAffected(), extra), nil
}

// Prune drops snapshot tables of changes older than the cutoff or already restored.
func Prune(ctx context.Context, conn *pgx.Conn, olderThan time.Duration, restored bool) (int, error) {
	ok, err := hasJournal(ctx, conn)
	if err != nil || !ok {
		return 0, err
	}
	rows, err := conn.Query(ctx, `SELECT grp, array_agg(snap) FILTER (WHERE snap IS NOT NULL) || coalesce(array_agg(meta->>'incoming') FILTER (WHERE meta ? 'incoming'), '{}') FROM oops.journal
		GROUP BY grp HAVING min(created_at) < now() - $1::interval OR ($2 AND bool_and(restored_at IS NOT NULL))`,
		fmt.Sprintf("%d seconds", int(olderThan.Seconds())), restored)
	if err != nil {
		return 0, err
	}
	type g struct {
		grp   string
		snaps []string
	}
	var groups []g
	for rows.Next() {
		var x g
		if err := rows.Scan(&x.grp, &x.snaps); err != nil {
			rows.Close()
			return 0, err
		}
		groups = append(groups, x)
	}
	rows.Close()
	for _, x := range groups {
		for _, s := range x.snaps {
			if _, err := conn.Exec(ctx, "DROP TABLE IF EXISTS oops."+qi(s)); err != nil {
				return 0, err
			}
		}
		if _, err := conn.Exec(ctx, "DELETE FROM oops.journal WHERE grp = $1", x.grp); err != nil {
			return 0, err
		}
	}
	return len(groups), nil
}

// NotFoundError is a change id that is not in the journal.
type NotFoundError struct{ ID int64 }

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("there is no database change %d. oops db list shows the ones you have (they are kept 7 days)", e.ID)
}
