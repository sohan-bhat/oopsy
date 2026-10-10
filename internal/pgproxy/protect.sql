-- oops db protect: triggers that save changes made without the proxy, so
-- oops db undo can reverse them too. Everything one transaction changed is
-- one undo unit, grouped as 'tx' plus the transaction id.
--
-- Rows: every user table gets statement level triggers with transition
-- tables. DELETE and UPDATE keep the old rows in oops.audit in their text
-- form, half the size of jsonb and exact for every type, with the column
-- list of the table at that moment in the journal (and the new rows too for
-- tables without a key, so undo can find them by value). TRUNCATE copies the
-- whole table first, like the proxy does.
--
-- DDL: a ddl_command_start event trigger runs before DROP TABLE, VIEW,
-- MATERIALIZED VIEW, FUNCTION, PROCEDURE, ROUTINE and SCHEMA, while the
-- data and definitions still exist. Postgres does not tell that trigger
-- which objects are about to go, so it reads the names from the statement
-- text and saves them as pending. The sql_drop event trigger, which does
-- get the exact list of dropped objects, then confirms the saves that were
-- really dropped. Saves never confirmed are ignored and pruned.

-- One row per changed row, tied to its journal entry by jid. No key and no
-- foreign key, so each saved row costs one insert and one index entry;
-- pruning deletes the rows of the journal entries it removes.
CREATE TABLE IF NOT EXISTS oops.audit (
  jid bigint NOT NULL,
  old text,
  new text
);
CREATE INDEX IF NOT EXISTS audit_jid ON oops.audit (jid);

-- The columns of a table as "name type, ...", enough to read its rows back
-- from their text form even after the table changed.
CREATE OR REPLACE FUNCTION oops._row_def(r oid) RETURNS text
LANGUAGE sql STABLE AS $f$
  SELECT string_agg(format('%I %s', attname, format_type(atttypid, atttypmod)), ', ' ORDER BY attnum)
    FROM pg_attribute WHERE attrelid = r AND attnum > 0 AND NOT attisdropped
$f$;

CREATE OR REPLACE FUNCTION oops.audit_delete() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER AS $f$
DECLARE
  j bigint;
  n bigint;
BEGIN
  IF current_setting('oops.paused', true) = 'on' OR oops._consume(TG_RELID, 'd') THEN RETURN NULL; END IF;
  SELECT count(*) INTO n FROM oops_old;
  IF n = 0 THEN RETURN NULL; END IF;
  INSERT INTO oops.journal (grp, kind, rel_schema, rel_name, key_cols, col_def, row_count, statement)
  VALUES ('tx' || txid_current(), 'audit_delete', TG_TABLE_SCHEMA, TG_TABLE_NAME, oops.key_cols(TG_RELID), oops._row_def(TG_RELID), n, current_query())
  RETURNING id INTO j;
  INSERT INTO oops.audit (jid, old) SELECT j, o::text FROM oops_old o;
  RETURN NULL;
END
$f$;

CREATE OR REPLACE FUNCTION oops.audit_update() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER AS $f$
DECLARE
  j bigint;
  n bigint;
  k text[] := oops.key_cols(TG_RELID);
BEGIN
  IF current_setting('oops.paused', true) = 'on' OR oops._consume(TG_RELID, 'u') THEN RETURN NULL; END IF;
  SELECT count(*) INTO n FROM oops_old;
  IF n = 0 THEN RETURN NULL; END IF;
  INSERT INTO oops.journal (grp, kind, rel_schema, rel_name, key_cols, col_def, row_count, statement)
  VALUES ('tx' || txid_current(), 'audit_update', TG_TABLE_SCHEMA, TG_TABLE_NAME, k, oops._row_def(TG_RELID), n, current_query())
  RETURNING id INTO j;
  INSERT INTO oops.audit (jid, old) SELECT j, o::text FROM oops_old o;
  IF k IS NULL THEN
    INSERT INTO oops.audit (jid, new) SELECT j, x::text FROM oops_new x;
  END IF;
  RETURN NULL;
END
$f$;

CREATE OR REPLACE FUNCTION oops.audit_truncate() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER AS $f$
BEGIN
  IF current_setting('oops.paused', true) = 'on' OR oops._consume(TG_RELID, 't') THEN RETURN NULL; END IF;
  -- A partitioned table holds no rows itself; its partitions save theirs.
  IF (SELECT relkind FROM pg_class WHERE oid = TG_RELID) = 'p' THEN RETURN NULL; END IF;
  PERFORM oops._snap_whole(TG_RELID, 'tx' || txid_current(), 'truncate', current_query(), false);
  RETURN NULL;
END
$f$;

-- Attach the audit triggers to one table. Returns whether it is protected.
CREATE OR REPLACE FUNCTION oops.protect_table(r oid) RETURNS boolean
LANGUAGE plpgsql AS $f$
DECLARE
  c record;
  name text;
BEGIN
  SELECT cl.relkind, cl.relpersistence, ns.nspname INTO c
    FROM pg_class cl JOIN pg_namespace ns ON ns.oid = cl.relnamespace WHERE cl.oid = r;
  IF NOT FOUND OR c.relkind NOT IN ('r', 'p') OR c.relpersistence = 't'
     OR c.nspname IN ('oops', 'pg_catalog', 'information_schema') OR c.nspname LIKE 'pg\_toast%' THEN
    RETURN false;
  END IF;
  IF EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = r AND tgname = 'oops_audit_delete') THEN RETURN true; END IF;
  name := oops.qualified(r);
  EXECUTE format('CREATE TRIGGER oops_audit_delete AFTER DELETE ON %s REFERENCING OLD TABLE AS oops_old FOR EACH STATEMENT EXECUTE FUNCTION oops.audit_delete()', name);
  EXECUTE format('CREATE TRIGGER oops_audit_update AFTER UPDATE ON %s REFERENCING OLD TABLE AS oops_old NEW TABLE AS oops_new FOR EACH STATEMENT EXECUTE FUNCTION oops.audit_update()', name);
  EXECUTE format('CREATE TRIGGER oops_audit_truncate BEFORE TRUNCATE ON %s FOR EACH STATEMENT EXECUTE FUNCTION oops.audit_truncate()', name);
  RETURN true;
END
$f$;

-- The names after the nth "DROP <tag> [IF EXISTS]" in a query, split on
-- top level commas. Comments are skipped; quoted names keep their quotes.
CREATE OR REPLACE FUNCTION oops._ddl_targets(q text, tag text, nth int) RETURNS text[]
LANGUAGE plpgsql IMMUTABLE AS $f$
DECLARE
  s text := regexp_replace(regexp_replace(q, '/\*.*?\*/', ' ', 'g'), '--[^\n]*', ' ', 'g');
  pos int;
  i int;
  ch text;
  depth int := 0;
  inq boolean := false;
  cur text := '';
  out text[] := '{}';
BEGIN
  pos := regexp_instr(s, '\m' || replace(tag, ' ', '\s+') || '\M(\s+IF\s+EXISTS\M)?', 1, nth, 1, 'i');
  IF pos = 0 THEN RETURN out; END IF;
  FOR i IN pos .. length(s) LOOP
    ch := substr(s, i, 1);
    IF inq THEN
      cur := cur || ch;
      IF ch = '"' THEN inq := false; END IF;
    ELSIF ch = '"' THEN
      inq := true;
      cur := cur || ch;
    ELSIF ch = '(' THEN
      depth := depth + 1;
      cur := cur || ch;
    ELSIF ch = ')' THEN
      depth := depth - 1;
      cur := cur || ch;
    ELSIF depth = 0 AND ch = ';' THEN
      EXIT;
    ELSIF depth = 0 AND ch = ',' THEN
      out := out || btrim(cur, E' \t\r\n');
      cur := '';
    ELSE
      cur := cur || ch;
    END IF;
  END LOOP;
  out := out || btrim(regexp_replace(cur, '\s(CASCADE|RESTRICT)\s*$', '', 'i'), E' \t\r\n');
  RETURN array_remove(out, '');
END
$f$;

-- Which DROP <tag> statement of the current query string is running: the
-- first, second and so on. Statements sent in one message share
-- statement_timestamp().
CREATE OR REPLACE FUNCTION oops._ddl_nth(tag text) RETURNS int
LANGUAGE plpgsql AS $f$
DECLARE
  k text := 'oops.n_' || lower(replace(tag, ' ', '_'));
  v text := coalesce(current_setting(k, true), '');
  ts text := statement_timestamp()::text;
  n int := 1;
BEGIN
  IF split_part(v, '|', 1) = ts THEN n := split_part(v, '|', 2)::int + 1; END IF;
  PERFORM set_config(k, ts || '|' || n, true);
  RETURN n;
END
$f$;

CREATE OR REPLACE FUNCTION oops.protect_ddl_start() RETURNS event_trigger
LANGUAGE plpgsql SECURITY DEFINER AS $f$
DECLARE
  g text;
  q text := current_query();
  t text;
  o oid;
  ns text;
  before bigint;
BEGIN
  IF current_setting('oops.paused', true) = 'on' THEN RETURN; END IF;
  g := 'tx' || txid_current();
  SELECT coalesce(max(id), 0) INTO before FROM oops.journal;
  FOREACH t IN ARRAY oops._ddl_targets(q, tg_tag, oops._ddl_nth(tg_tag)) LOOP
    BEGIN
      IF tg_tag = 'DROP SCHEMA' THEN
        SELECT oid, nspname INTO o, ns FROM pg_namespace WHERE nspname = (parse_ident(t))[1];
      ELSIF tg_tag IN ('DROP FUNCTION', 'DROP PROCEDURE', 'DROP ROUTINE') THEN
        o := CASE WHEN position('(' in t) > 0 THEN to_regprocedure(t)::oid ELSE to_regproc(t)::oid END;
        SELECT n.nspname INTO ns FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE p.oid = o;
      ELSE
        o := to_regclass(t)::oid;
        SELECT n.nspname INTO ns FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = o;
      END IF;
    EXCEPTION WHEN OTHERS THEN
      o := NULL;
    END;
    -- Skip what is gone already, the oops schema itself, and what the proxy
    -- saved for this same statement.
    CONTINUE WHEN o IS NULL OR ns = 'oops' OR position('x' in coalesce(current_setting('oops.px_' || o, true), '')) > 0;
    IF tg_tag = 'DROP TABLE' THEN
      PERFORM oops.snap_drop_table(oops.qualified(o), g, q);
    ELSIF tg_tag IN ('DROP VIEW', 'DROP MATERIALIZED VIEW') THEN
      PERFORM oops._snap_view(o, g, q, 0);
    ELSIF tg_tag = 'DROP SCHEMA' THEN
      PERFORM oops.snap_drop_schema(ns, g, q);
    ELSE
      PERFORM oops._snap_routine(o, g, q);
    END IF;
  END LOOP;
  UPDATE oops.journal SET meta = meta || '{"pending": true}' WHERE grp = g AND id > before;
END
$f$;

CREATE OR REPLACE FUNCTION oops.protect_ddl_drop() RETURNS event_trigger
LANGUAGE plpgsql SECURITY DEFINER AS $f$
BEGIN
  UPDATE oops.journal SET meta = meta - 'pending'
   WHERE grp = 'tx' || txid_current() AND meta ? 'pending'
     AND (meta->>'oid')::oid IN (SELECT objid FROM pg_event_trigger_dropped_objects());
END
$f$;

CREATE OR REPLACE FUNCTION oops.protect_ddl_end() RETURNS event_trigger
LANGUAGE plpgsql SECURITY DEFINER AS $f$
DECLARE
  r record;
BEGIN
  FOR r IN SELECT objid FROM pg_event_trigger_ddl_commands()
            WHERE object_type = 'table' AND command_tag IN ('CREATE TABLE', 'CREATE TABLE AS', 'SELECT INTO') LOOP
    PERFORM oops.protect_table(r.objid);
  END LOOP;
END
$f$;

DROP EVENT TRIGGER IF EXISTS oops_protect_start;
CREATE EVENT TRIGGER oops_protect_start ON ddl_command_start
  WHEN TAG IN ('DROP TABLE', 'DROP VIEW', 'DROP MATERIALIZED VIEW', 'DROP FUNCTION', 'DROP PROCEDURE', 'DROP ROUTINE', 'DROP SCHEMA')
  EXECUTE FUNCTION oops.protect_ddl_start();
DROP EVENT TRIGGER IF EXISTS oops_protect_drop;
CREATE EVENT TRIGGER oops_protect_drop ON sql_drop
  EXECUTE FUNCTION oops.protect_ddl_drop();
DROP EVENT TRIGGER IF EXISTS oops_protect_end;
CREATE EVENT TRIGGER oops_protect_end ON ddl_command_end
  WHEN TAG IN ('CREATE TABLE', 'CREATE TABLE AS', 'SELECT INTO')
  EXECUTE FUNCTION oops.protect_ddl_end();
