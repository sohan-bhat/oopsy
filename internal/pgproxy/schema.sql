-- Oopsy keeps snapshots of destroyed data in the "oops" schema. Every
-- snapshot is written in the same transaction as the statement it guards, so
-- a rollback discards it automatically.
CREATE SCHEMA IF NOT EXISTS oops;

CREATE TABLE IF NOT EXISTS oops.journal (
  id          bigserial PRIMARY KEY,
  grp         text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  kind        text NOT NULL,
  rel_schema  text,
  rel_name    text,
  col_name    text,
  snap        text,
  key_cols    text[],
  col_def     text,
  ddl         text,
  ddl_post    text[],
  meta        jsonb NOT NULL DEFAULT '{}'::jsonb,
  row_count   bigint,
  statement   text,
  db_user     text NOT NULL DEFAULT current_user,
  restored_at timestamptz
);
CREATE INDEX IF NOT EXISTS journal_grp ON oops.journal (grp);

-- Columns that identify a row: the primary key, else a unique index whose
-- columns are all NOT NULL. NULL when the table has neither.
CREATE OR REPLACE FUNCTION oops.key_cols(r regclass) RETURNS text[]
LANGUAGE sql STABLE AS $f$
  SELECT cols FROM (
    SELECT i.indisprimary,
           (SELECT array_agg(a.attname::text ORDER BY k.ord)
              FROM unnest(i.indkey::int2[]) WITH ORDINALITY AS k(attnum, ord)
              JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum) AS cols,
           (SELECT bool_and(a.attnotnull)
              FROM unnest(i.indkey::int2[]) AS k(attnum)
              JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum) AS all_notnull,
           i.indexprs IS NULL AND i.indpred IS NULL AND NOT (0 = ANY (i.indkey::int2[])) AS plain
      FROM pg_index i
     WHERE i.indrelid = r AND (i.indisprimary OR i.indisunique) AND i.indisvalid
  ) x
  WHERE plain AND (indisprimary OR all_notnull)
  ORDER BY indisprimary DESC, array_length(cols, 1)
  LIMIT 1
$f$;

CREATE OR REPLACE FUNCTION oops.qualified(r regclass) RETURNS text
LANGUAGE sql STABLE AS $f$
  SELECT format('%I.%I', n.nspname, c.relname)
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = r
$f$;

-- Called right after the proxy copied the rows a DELETE or UPDATE will touch
-- into oops.<snap>.
CREATE OR REPLACE FUNCTION oops.record_rows(p_kind text, p_rel text, p_snap text, p_grp text, p_stmt text)
RETURNS bigint LANGUAGE plpgsql AS $f$
DECLARE
  r regclass := to_regclass(p_rel);
  n bigint;
  k text[];
  ns text; nm text;
BEGIN
  IF r IS NULL THEN
    EXECUTE format('DROP TABLE IF EXISTS oops.%I', p_snap);
    RETURN 0;
  END IF;
  SELECT nspname, relname INTO ns, nm FROM pg_class c JOIN pg_namespace s ON s.oid = c.relnamespace WHERE c.oid = r;
  EXECUTE format('SELECT count(*) FROM oops.%I', p_snap) INTO n;
  IF n = 0 THEN
    EXECUTE format('DROP TABLE oops.%I', p_snap);
    RETURN 0;
  END IF;
  k := oops.key_cols(r);
  IF p_kind = 'update' AND k IS NULL THEN
    -- Without a key we cannot match rows after the update, so keep the
    -- whole table instead and restore it as a whole.
    EXECUTE format('DROP TABLE oops.%I', p_snap);
    EXECUTE format('CREATE TABLE oops.%I AS TABLE %s', p_snap, r);
    EXECUTE format('SELECT count(*) FROM oops.%I', p_snap) INTO n;
    p_kind := 'update_full';
  END IF;
  INSERT INTO oops.journal (grp, kind, rel_schema, rel_name, snap, key_cols, row_count, statement)
  VALUES (p_grp, p_kind, ns, nm, p_snap, k, n, p_stmt);
  IF p_kind = 'delete' THEN
    n := n + oops._snap_fk_children(r, p_snap, p_grp, p_stmt, 0);
  END IF;
  RETURN n;
END
$f$;

-- Rows in other tables that a DELETE will remove or change through
-- ON DELETE CASCADE, SET NULL or SET DEFAULT foreign keys.
CREATE OR REPLACE FUNCTION oops._snap_fk_children(r regclass, p_snap text, p_grp text, p_stmt text, depth int)
RETURNS bigint LANGUAGE plpgsql AS $f$
DECLARE
  c record;
  cond text;
  child_snap text;
  n bigint;
  total bigint := 0;
  kind text;
  k text[];
  ns text; nm text;
BEGIN
  IF depth > 10 THEN RETURN 0; END IF;
  FOR c IN SELECT oid, conrelid::regclass AS child, conkey, confkey, confdeltype FROM pg_constraint
            WHERE contype = 'f' AND confrelid = r AND confdeltype IN ('c', 'n', 'd') LOOP
    SELECT string_agg(format('ch.%I = p.%I', ca.attname, pa.attname), ' AND ')
      INTO cond
      FROM unnest(c.conkey, c.confkey) AS m(ck, pk)
      JOIN pg_attribute ca ON ca.attrelid = c.child AND ca.attnum = m.ck
      JOIN pg_attribute pa ON pa.attrelid = r AND pa.attnum = m.pk;
    child_snap := left(p_grp || '_fk' || c.oid || '_' || depth, 63);
    IF to_regclass(format('oops.%I', child_snap)) IS NOT NULL THEN CONTINUE; END IF;
    k := oops.key_cols(c.child);
    kind := CASE WHEN c.confdeltype = 'c' THEN 'delete' ELSE 'update' END;
    IF kind = 'update' AND k IS NULL THEN
      kind := 'update_full';
      EXECUTE format('CREATE TABLE oops.%I AS TABLE %s', child_snap, c.child);
    ELSE
      EXECUTE format('CREATE TABLE oops.%I AS SELECT ch.* FROM %s ch WHERE EXISTS (SELECT 1 FROM oops.%I p WHERE %s)', child_snap, c.child, p_snap, cond);
    END IF;
    EXECUTE format('SELECT count(*) FROM oops.%I', child_snap) INTO n;
    IF n = 0 THEN
      EXECUTE format('DROP TABLE oops.%I', child_snap);
      CONTINUE;
    END IF;
    SELECT nspname, relname INTO ns, nm FROM pg_class cl JOIN pg_namespace s ON s.oid = cl.relnamespace WHERE cl.oid = c.child;
    INSERT INTO oops.journal (grp, kind, rel_schema, rel_name, snap, key_cols, row_count, statement)
    VALUES (p_grp, kind, ns, nm, child_snap, k, n, p_stmt);
    total := total + n;
    IF c.confdeltype = 'c' THEN
      total := total + oops._snap_fk_children(c.child, child_snap, p_grp, p_stmt, depth + 1);
    END IF;
  END LOOP;
  RETURN total;
END
$f$;

-- Whole table copies for TRUNCATE (following CASCADE), MERGE and
-- statements the proxy cannot narrow down.
CREATE OR REPLACE FUNCTION oops.snap_table(p_rel text, p_grp text, p_kind text, p_stmt text, p_cascade boolean)
RETURNS bigint LANGUAGE plpgsql AS $f$
DECLARE
  r regclass := to_regclass(p_rel);
BEGIN
  IF r IS NULL THEN RETURN 0; END IF;
  RETURN oops._snap_whole(r, p_grp, p_kind, p_stmt, p_cascade);
END
$f$;

CREATE OR REPLACE FUNCTION oops._snap_whole(r regclass, p_grp text, p_kind text, p_stmt text, p_cascade boolean)
RETURNS bigint LANGUAGE plpgsql AS $f$
DECLARE
  snap text := p_grp || '_' || r::oid;
  n bigint := 0;
  ns text; nm text;
  child regclass;
BEGIN
  IF to_regclass(format('oops.%I', snap)) IS NOT NULL THEN
    RETURN 0; -- already saved in this statement
  END IF;
  SELECT nspname, relname INTO ns, nm FROM pg_class c JOIN pg_namespace s ON s.oid = c.relnamespace WHERE c.oid = r;
  EXECUTE format('CREATE TABLE oops.%I AS TABLE %s', snap, r);
  EXECUTE format('SELECT count(*) FROM oops.%I', snap) INTO n;
  INSERT INTO oops.journal (grp, kind, rel_schema, rel_name, snap, key_cols, row_count, statement)
  VALUES (p_grp, p_kind, ns, nm, snap, oops.key_cols(r), n, p_stmt);
  IF p_cascade THEN
    FOR child IN SELECT DISTINCT conrelid::regclass FROM pg_constraint WHERE contype = 'f' AND confrelid = r AND conrelid <> r LOOP
      n := n + oops._snap_whole(child, p_grp, p_kind, p_stmt, true);
    END LOOP;
  END IF;
  RETURN n;
END
$f$;

-- Everything needed to rebuild a table: CREATE TABLE, then constraints,
-- indexes, triggers, foreign keys pointing at it and views built on it.
CREATE OR REPLACE FUNCTION oops.table_ddl(r regclass, OUT ddl text, OUT post text[], OUT meta jsonb)
LANGUAGE plpgsql AS $f$
DECLARE
  tbl text := oops.qualified(r);
  cols text[] := '{}';
  pre text := '';
  rec record;
  seqval record;
  def text;
BEGIN
  post := '{}';
  meta := '{"identity": {}, "sequences": {}}'::jsonb;
  FOR rec IN
    SELECT a.attname, a.attnum, format_type(a.atttypid, a.atttypmod) AS typ, a.attnotnull, a.attidentity, a.attgenerated,
           pg_get_expr(d.adbin, d.adrelid) AS dflt,
           CASE WHEN a.attcollation <> t.typcollation AND a.attcollation <> 0
                THEN (SELECT format('%I.%I', cn.nspname, co.collname) FROM pg_collation co JOIN pg_namespace cn ON cn.oid = co.collnamespace WHERE co.oid = a.attcollation) END AS coll
      FROM pg_attribute a
      JOIN pg_type t ON t.oid = a.atttypid
      LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
     WHERE a.attrelid = r AND a.attnum > 0 AND NOT a.attisdropped
     ORDER BY a.attnum
  LOOP
    def := format('%I %s', rec.attname, rec.typ);
    IF rec.coll IS NOT NULL THEN def := def || ' COLLATE ' || rec.coll; END IF;
    IF rec.attidentity = 'a' THEN def := def || ' GENERATED ALWAYS AS IDENTITY';
    ELSIF rec.attidentity = 'd' THEN def := def || ' GENERATED BY DEFAULT AS IDENTITY';
    ELSIF rec.attgenerated = 's' THEN def := def || format(' GENERATED ALWAYS AS (%s) STORED', rec.dflt);
    ELSIF rec.dflt IS NOT NULL THEN def := def || ' DEFAULT ' || rec.dflt;
    END IF;
    IF rec.attnotnull THEN def := def || ' NOT NULL'; END IF;
    cols := cols || def;
    IF rec.attidentity IN ('a', 'd') THEN
      EXECUTE format('SELECT last_value, is_called FROM %s', pg_get_serial_sequence(tbl, rec.attname)) INTO seqval;
      meta := jsonb_set(meta, ARRAY['identity', rec.attname::text], jsonb_build_object('last_value', seqval.last_value, 'is_called', seqval.is_called), true);
    END IF;
  END LOOP;

  -- serial columns: sequences owned by the table are dropped with it
  FOR rec IN
    SELECT format('%I.%I', sn.nspname, s.relname) AS seq, a.attname
      FROM pg_depend dep
      JOIN pg_class s ON s.oid = dep.objid AND s.relkind = 'S'
      JOIN pg_namespace sn ON sn.oid = s.relnamespace
      JOIN pg_attribute a ON a.attrelid = dep.refobjid AND a.attnum = dep.refobjsubid
     WHERE dep.classid = 'pg_class'::regclass AND dep.refobjid = r AND dep.deptype = 'a'
  LOOP
    EXECUTE format('SELECT last_value, is_called FROM %s', rec.seq) INTO seqval;
    pre := pre || format('CREATE SEQUENCE IF NOT EXISTS %s; ', rec.seq);
    post := post || format('ALTER SEQUENCE %s OWNED BY %s.%I', rec.seq, tbl, rec.attname);
    meta := jsonb_set(meta, ARRAY['sequences', rec.seq], jsonb_build_object('last_value', seqval.last_value, 'is_called', seqval.is_called), true);
  END LOOP;

  ddl := pre || format('CREATE TABLE %s (%s)', tbl, array_to_string(cols, ', '));
  IF (SELECT relkind FROM pg_class WHERE oid = r) = 'p' THEN
    ddl := ddl || ' PARTITION BY ' || pg_get_partkeydef(r);
  END IF;

  FOR rec IN SELECT conname, pg_get_constraintdef(oid) AS def FROM pg_constraint
              WHERE conrelid = r AND contype IN ('p', 'u', 'c', 'x') ORDER BY contype = 'p' DESC, conname LOOP
    post := post || format('ALTER TABLE %s ADD CONSTRAINT %I %s', tbl, rec.conname, rec.def);
  END LOOP;
  FOR rec IN SELECT pg_get_indexdef(i.indexrelid) AS def FROM pg_index i
              WHERE i.indrelid = r AND NOT EXISTS (SELECT 1 FROM pg_constraint c WHERE c.conindid = i.indexrelid AND c.conrelid = r) LOOP
    post := post || rec.def;
  END LOOP;
  FOR rec IN SELECT conname, conrelid::regclass AS onrel, pg_get_constraintdef(oid) AS def FROM pg_constraint
              WHERE contype = 'f' AND (conrelid = r OR confrelid = r) LOOP
    post := post || format('ALTER TABLE %s ADD CONSTRAINT %I %s', oops.qualified(rec.onrel), rec.conname, rec.def);
  END LOOP;
  FOR rec IN SELECT pg_get_triggerdef(oid) AS def FROM pg_trigger WHERE tgrelid = r AND NOT tgisinternal LOOP
    post := post || rec.def;
  END LOOP;
  FOR rec IN
    SELECT DISTINCT v.oid, v.relkind, format('%I.%I', vn.nspname, v.relname) AS name
      FROM pg_depend dep
      JOIN pg_rewrite rw ON rw.oid = dep.objid
      JOIN pg_class v ON v.oid = rw.ev_class
      JOIN pg_namespace vn ON vn.oid = v.relnamespace
     WHERE dep.classid = 'pg_rewrite'::regclass AND dep.refobjid = r AND v.oid <> r AND v.relkind IN ('v', 'm')
  LOOP
    IF rec.relkind = 'm' THEN
      post := post || format('CREATE MATERIALIZED VIEW %s AS %s', rec.name, rtrim(pg_get_viewdef(rec.oid), ';'));
    ELSE
      post := post || format('CREATE VIEW %s AS %s', rec.name, rtrim(pg_get_viewdef(rec.oid), ';'));
    END IF;
  END LOOP;
  IF (SELECT obj_description(r, 'pg_class')) IS NOT NULL THEN
    post := post || format('COMMENT ON TABLE %s IS %L', tbl, obj_description(r, 'pg_class'));
  END IF;
END
$f$;

CREATE OR REPLACE FUNCTION oops.snap_drop_table(p_rel text, p_grp text, p_stmt text)
RETURNS bigint LANGUAGE plpgsql AS $f$
DECLARE
  r regclass := to_regclass(p_rel);
  snap text;
  n bigint;
  d record;
  ns text; nm text;
BEGIN
  IF r IS NULL OR (SELECT relkind FROM pg_class WHERE oid = r) NOT IN ('r', 'p') THEN RETURN 0; END IF;
  snap := p_grp || '_' || r::oid;
  IF to_regclass(format('oops.%I', snap)) IS NOT NULL THEN RETURN 0; END IF;
  SELECT nspname, relname INTO ns, nm FROM pg_class c JOIN pg_namespace s ON s.oid = c.relnamespace WHERE c.oid = r;
  SELECT * INTO d FROM oops.table_ddl(r);
  EXECUTE format('CREATE TABLE oops.%I AS TABLE %s', snap, r);
  EXECUTE format('SELECT count(*) FROM oops.%I', snap) INTO n;
  INSERT INTO oops.journal (grp, kind, rel_schema, rel_name, snap, key_cols, ddl, ddl_post, meta, row_count, statement)
  VALUES (p_grp, 'drop_table', ns, nm, snap, oops.key_cols(r), d.ddl, d.post, d.meta, n, p_stmt);
  RETURN n;
END
$f$;

CREATE OR REPLACE FUNCTION oops.snap_drop_schema(p_schema text, p_grp text, p_stmt text)
RETURNS bigint LANGUAGE plpgsql AS $f$
DECLARE
  n bigint := 0;
  t regclass;
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = p_schema) OR p_schema = 'oops' THEN RETURN 0; END IF;
  INSERT INTO oops.journal (grp, kind, rel_schema, row_count, statement) VALUES (p_grp, 'drop_schema', p_schema, 0, p_stmt);
  FOR t IN SELECT c.oid::regclass FROM pg_class c JOIN pg_namespace s ON s.oid = c.relnamespace
            WHERE s.nspname = p_schema AND c.relkind IN ('r', 'p') AND NOT c.relispartition ORDER BY c.oid LOOP
    n := n + oops.snap_drop_table(oops.qualified(t), p_grp, p_stmt);
  END LOOP;
  RETURN n;
END
$f$;

CREATE OR REPLACE FUNCTION oops.snap_column(p_rel text, p_col text, p_grp text, p_stmt text)
RETURNS bigint LANGUAGE plpgsql AS $f$
DECLARE
  r regclass := to_regclass(p_rel);
  k text[];
  a record;
  snap text;
  n bigint;
  def text;
  ns text; nm text;
  keylist text;
  post text[];
  dep record;
BEGIN
  IF r IS NULL THEN RETURN 0; END IF;
  SELECT at.attnum, at.attname, format_type(at.atttypid, at.atttypmod) AS typ, at.attnotnull, at.attgenerated, pg_get_expr(d.adbin, d.adrelid) AS dflt
    INTO a
    FROM pg_attribute at LEFT JOIN pg_attrdef d ON d.adrelid = at.attrelid AND d.adnum = at.attnum
   WHERE at.attrelid = r AND at.attname = p_col AND at.attnum > 0 AND NOT at.attisdropped;
  IF NOT FOUND THEN RETURN 0; END IF;
  snap := p_grp || '_' || r::oid || '_' || md5(p_col);
  snap := left(snap, 63);
  SELECT nspname, relname INTO ns, nm FROM pg_class c JOIN pg_namespace s ON s.oid = c.relnamespace WHERE c.oid = r;
  k := oops.key_cols(r);
  IF k IS NOT NULL AND p_col = ANY (k) THEN k := NULL; END IF;
  def := a.typ;
  IF a.attgenerated = 's' THEN def := def || format(' GENERATED ALWAYS AS (%s) STORED', a.dflt);
  ELSIF a.dflt IS NOT NULL THEN def := def || ' DEFAULT ' || a.dflt;
  END IF;
  IF k IS NULL THEN
    keylist := 'ctid::text AS oops_ctid';
  ELSE
    SELECT string_agg(format('%I', c), ', ') INTO keylist FROM unnest(k) AS c;
  END IF;
  EXECUTE format('CREATE TABLE oops.%I AS SELECT %s, %I FROM %s', snap, keylist, p_col, r);
  EXECUTE format('SELECT count(*) FROM oops.%I', snap) INTO n;
  -- Constraints, indexes and views that go away with the column.
  post := '{}';
  FOR dep IN SELECT conname, pg_get_constraintdef(oid) AS cdef FROM pg_constraint
              WHERE conrelid = r AND a.attnum = ANY (conkey) AND contype IN ('p', 'u', 'c', 'f', 'x') LOOP
    post := post || format('ALTER TABLE %s ADD CONSTRAINT %I %s', oops.qualified(r), dep.conname, dep.cdef);
  END LOOP;
  FOR dep IN SELECT DISTINCT pg_get_indexdef(d.objid) AS idef FROM pg_depend d JOIN pg_index i ON i.indexrelid = d.objid
              WHERE d.classid = 'pg_class'::regclass AND d.refobjid = r AND d.refobjsubid = a.attnum
                AND NOT EXISTS (SELECT 1 FROM pg_constraint co WHERE co.conindid = i.indexrelid) LOOP
    post := post || dep.idef;
  END LOOP;
  FOR dep IN SELECT DISTINCT v.oid, v.relkind, format('%I.%I', vn.nspname, v.relname) AS vname
               FROM pg_depend d JOIN pg_rewrite rw ON rw.oid = d.objid JOIN pg_class v ON v.oid = rw.ev_class
               JOIN pg_namespace vn ON vn.oid = v.relnamespace
              WHERE d.classid = 'pg_rewrite'::regclass AND d.refobjid = r AND d.refobjsubid = a.attnum AND v.oid <> r LOOP
    post := post || format(CASE WHEN dep.relkind = 'm' THEN 'CREATE MATERIALIZED VIEW %s AS %s' ELSE 'CREATE VIEW %s AS %s' END,
                           dep.vname, rtrim(pg_get_viewdef(dep.oid), ';'));
  END LOOP;
  INSERT INTO oops.journal (grp, kind, rel_schema, rel_name, col_name, snap, key_cols, col_def, ddl_post, meta, row_count, statement)
  VALUES (p_grp, 'drop_column', ns, nm, p_col, snap, k, def, post, jsonb_build_object('not_null', a.attnotnull, 'generated', a.attgenerated = 's'), n, p_stmt);
  RETURN n;
END
$f$;

-- The proxy and the protect triggers both save before destructive changes.
-- Each marks what it saved in a transaction local setting, so a change the
-- proxy already saved is not saved a second time by the protect triggers.
-- kinds: d delete, u update, t truncate, x drop.
CREATE OR REPLACE FUNCTION oops._mark(o oid, kinds text) RETURNS void
LANGUAGE sql AS $f$
  SELECT set_config('oops.px_' || o, coalesce(current_setting('oops.px_' || o, true), '') || kinds, true)
$f$;

CREATE OR REPLACE FUNCTION oops._consume(o oid, kind text) RETURNS boolean
LANGUAGE plpgsql AS $f$
DECLARE
  v text := coalesce(current_setting('oops.px_' || o, true), '');
BEGIN
  IF position(kind in v) = 0 THEN RETURN false; END IF;
  PERFORM set_config('oops.px_' || o, regexp_replace(v, kind, ''), true);
  RETURN true;
END
$f$;

-- GRANT statements that rebuild an access list exactly, in its original
-- order. A NULL list means the default privileges, which a recreated object
-- gets anyway.
CREATE OR REPLACE FUNCTION oops._acl_sql(acl aclitem[], objtype text, obj text, owner oid) RETURNS text[]
LANGUAGE sql STABLE AS $f$
  SELECT CASE WHEN acl IS NULL THEN '{}'::text[] ELSE
    ARRAY[format('REVOKE ALL ON %s %s FROM PUBLIC', objtype, obj),
          format('REVOKE ALL ON %s %s FROM %s', objtype, obj, owner::regrole)]
    || coalesce((SELECT array_agg(format('GRANT %s ON %s %s TO %s%s', a.privilege_type, objtype, obj,
                   CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE a.grantee::regrole::text END,
                   CASE WHEN a.is_grantable THEN ' WITH GRANT OPTION' ELSE '' END) ORDER BY a.ord)
                   FROM aclexplode(acl) WITH ORDINALITY a(grantor, grantee, privilege_type, is_grantable, ord)), '{}') END
$f$;

-- Definition of a view or materialized view, everything that goes with it,
-- and the views built on it, which DROP ... CASCADE removes too.
CREATE OR REPLACE FUNCTION oops._snap_view(v oid, p_grp text, p_stmt text, depth int) RETURNS bigint
LANGUAGE plpgsql AS $f$
DECLARE
  c record;
  rec record;
  name text;
  kw text;
  ddl text;
  post text[] := '{}';
  n bigint := 1;
BEGIN
  IF depth > 20 THEN RETURN 0; END IF;
  SELECT cl.relkind, cl.relname, ns.nspname, cl.reloptions, cl.relispopulated, cl.relacl, cl.relowner
    INTO c FROM pg_class cl JOIN pg_namespace ns ON ns.oid = cl.relnamespace WHERE cl.oid = v;
  IF NOT FOUND OR c.relkind NOT IN ('v', 'm') THEN RETURN 0; END IF;
  IF EXISTS (SELECT 1 FROM oops.journal WHERE grp = p_grp AND meta->>'oid' = v::text) THEN RETURN 0; END IF;
  name := format('%I.%I', c.nspname, c.relname);
  kw := CASE c.relkind WHEN 'm' THEN 'MATERIALIZED VIEW' ELSE 'VIEW' END;
  ddl := format('CREATE %s %s', kw, name);
  IF c.reloptions IS NOT NULL THEN
    ddl := ddl || format(' WITH (%s)', array_to_string(c.reloptions, ', '));
  END IF;
  ddl := ddl || ' AS ' || rtrim(pg_get_viewdef(v), ';');
  IF c.relkind = 'm' THEN ddl := ddl || ' WITH NO DATA'; END IF;
  post := post || format('ALTER %s %s OWNER TO %s', kw, name, c.relowner::regrole);
  FOR rec IN SELECT a.attname, pg_get_expr(d.adbin, d.adrelid) AS dflt FROM pg_attrdef d
               JOIN pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum WHERE d.adrelid = v LOOP
    post := post || format('ALTER VIEW %s ALTER COLUMN %I SET DEFAULT %s', name, rec.attname, rec.dflt);
  END LOOP;
  FOR rec IN SELECT pg_get_indexdef(indexrelid) AS def FROM pg_index WHERE indrelid = v ORDER BY indexrelid LOOP
    post := post || rec.def;
  END LOOP;
  FOR rec IN SELECT pg_get_triggerdef(oid) AS def FROM pg_trigger WHERE tgrelid = v AND NOT tgisinternal ORDER BY tgname LOOP
    post := post || rec.def;
  END LOOP;
  IF c.relkind = 'm' AND c.relispopulated THEN
    post := post || format('REFRESH MATERIALIZED VIEW %s', name);
  END IF;
  post := post || oops._acl_sql(c.relacl, 'TABLE', name, c.relowner);
  IF obj_description(v, 'pg_class') IS NOT NULL THEN
    post := post || format('COMMENT ON %s %s IS %L', kw, name, obj_description(v, 'pg_class'));
  END IF;
  FOR rec IN SELECT a.attname, col_description(v, a.attnum) AS d FROM pg_attribute a
              WHERE a.attrelid = v AND a.attnum > 0 AND NOT a.attisdropped AND col_description(v, a.attnum) IS NOT NULL ORDER BY a.attnum LOOP
    post := post || format('COMMENT ON COLUMN %s.%I IS %L', name, rec.attname, rec.d);
  END LOOP;
  INSERT INTO oops.journal (grp, kind, rel_schema, rel_name, ddl, ddl_post, meta, row_count, statement)
  VALUES (p_grp, CASE c.relkind WHEN 'm' THEN 'drop_matview' ELSE 'drop_view' END, c.nspname, c.relname, ddl, post,
          jsonb_build_object('oid', v, 'populated', c.relispopulated), 0, p_stmt);
  PERFORM oops._mark(v, 'x');
  FOR rec IN SELECT DISTINCT rw.ev_class AS dep FROM pg_depend d JOIN pg_rewrite rw ON rw.oid = d.objid
              WHERE d.classid = 'pg_rewrite'::regclass AND d.refclassid = 'pg_class'::regclass
                AND d.refobjid = v AND rw.ev_class <> v LOOP
    n := n + oops._snap_view(rec.dep, p_grp, p_stmt, depth + 1);
  END LOOP;
  RETURN n;
END
$f$;

CREATE OR REPLACE FUNCTION oops.snap_drop_view(p_rel text, p_grp text, p_stmt text) RETURNS bigint
LANGUAGE plpgsql AS $f$
DECLARE
  r regclass := to_regclass(p_rel);
BEGIN
  IF r IS NULL THEN RETURN 0; END IF;
  RETURN oops._snap_view(r, p_grp, p_stmt, 0);
END
$f$;

-- Definition of a function or procedure, plus the triggers, column
-- defaults and views that DROP FUNCTION ... CASCADE takes with it.
CREATE OR REPLACE FUNCTION oops._snap_routine(f oid, p_grp text, p_stmt text) RETURNS bigint
LANGUAGE plpgsql AS $f$
DECLARE
  c record;
  rec record;
  sig text;
  kw text;
  post text[] := '{}';
  n bigint := 1;
BEGIN
  SELECT p.proname, ns.nspname, p.prokind, p.proacl, p.proowner, oidvectortypes(p.proargtypes) AS args
    INTO c FROM pg_proc p JOIN pg_namespace ns ON ns.oid = p.pronamespace WHERE p.oid = f;
  IF NOT FOUND OR c.prokind NOT IN ('f', 'p') THEN RETURN 0; END IF;
  IF EXISTS (SELECT 1 FROM oops.journal WHERE grp = p_grp AND meta->>'oid' = f::text) THEN RETURN 0; END IF;
  sig := format('%I.%I(%s)', c.nspname, c.proname, c.args);
  kw := CASE c.prokind WHEN 'p' THEN 'PROCEDURE' ELSE 'FUNCTION' END;
  post := post || format('ALTER %s %s OWNER TO %s', kw, sig, c.proowner::regrole);
  post := post || oops._acl_sql(c.proacl, kw, sig, c.proowner);
  IF obj_description(f, 'pg_proc') IS NOT NULL THEN
    post := post || format('COMMENT ON %s %s IS %L', kw, sig, obj_description(f, 'pg_proc'));
  END IF;
  FOR rec IN SELECT pg_get_triggerdef(oid) AS def FROM pg_trigger WHERE tgfoid = f AND NOT tgisinternal ORDER BY tgrelid, tgname LOOP
    post := post || rec.def;
  END LOOP;
  FOR rec IN SELECT ad.adrelid, a.attname, pg_get_expr(ad.adbin, ad.adrelid) AS dflt
               FROM pg_depend d JOIN pg_attrdef ad ON ad.oid = d.objid
               JOIN pg_attribute a ON a.attrelid = ad.adrelid AND a.attnum = ad.adnum
              WHERE d.classid = 'pg_attrdef'::regclass AND d.refclassid = 'pg_proc'::regclass AND d.refobjid = f LOOP
    post := post || format('ALTER TABLE %s ALTER COLUMN %I SET DEFAULT %s', oops.qualified(rec.adrelid), rec.attname, rec.dflt);
  END LOOP;
  INSERT INTO oops.journal (grp, kind, rel_schema, rel_name, ddl, ddl_post, meta, row_count, statement)
  VALUES (p_grp, 'drop_function', c.nspname, c.proname || '(' || c.args || ')', pg_get_functiondef(f), post,
          jsonb_build_object('oid', f, 'sig', sig, 'routine', lower(kw)), 0, p_stmt);
  PERFORM oops._mark(f, 'x');
  FOR rec IN SELECT DISTINCT rw.ev_class AS dep FROM pg_depend d JOIN pg_rewrite rw ON rw.oid = d.objid
              WHERE d.classid = 'pg_rewrite'::regclass AND d.refclassid = 'pg_proc'::regclass AND d.refobjid = f LOOP
    n := n + oops._snap_view(rec.dep, p_grp, p_stmt, 1);
  END LOOP;
  RETURN n;
END
$f$;

-- p_sig is name(argtypes), or a bare name when the statement gave no
-- argument list.
CREATE OR REPLACE FUNCTION oops.snap_drop_routine(p_sig text, p_grp text, p_stmt text) RETURNS bigint
LANGUAGE plpgsql AS $f$
DECLARE
  f oid;
BEGIN
  BEGIN
    IF position('(' in p_sig) > 0 THEN f := to_regprocedure(p_sig); ELSE f := to_regproc(p_sig); END IF;
  EXCEPTION WHEN OTHERS THEN
    RETURN 0;
  END;
  IF f IS NULL THEN RETURN 0; END IF;
  RETURN oops._snap_routine(f, p_grp, p_stmt);
END
$f$;
