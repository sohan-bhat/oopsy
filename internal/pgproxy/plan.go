// Package pgproxy is a Postgres wire protocol proxy that snapshots data
// right before destructive statements run, plus the undo logic.
package pgproxy

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	pg "github.com/pganalyze/pg_query_go/v6"
	pgquery "github.com/wasilibs/go-pgquery"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// SnapSQL is one statement the proxy runs before the user's statement.
type SnapSQL struct {
	SQL        string
	UsesParams bool
}

// StmtPlan is the snapshot plan for one destructive statement.
type StmtPlan struct {
	Location int // byte offset of the statement in the query string
	Kind     string
	Targets  []string
	Group    string
	Text     string
	Snaps    []SnapSQL
}

var destructiveWord = regexp.MustCompile(`(?i)\b(delete|update|truncate|drop|alter|merge)\b`)

// MaybeDestructive is a cheap filter run on every query.
func MaybeDestructive(q string) bool { return destructiveWord.MatchString(q) }

var groupSeq atomic.Int64

func newGroup() string {
	return "c" + strconv.FormatInt(time.Now().UnixNano(), 36) + strconv.FormatInt(groupSeq.Add(1)%1296, 36)
}

// Plan parses a query string and returns snapshot plans for every
// destructive statement in it, in order.
func Plan(query string) ([]StmtPlan, error) {
	_, plans, err := PlanAll(query)
	return plans, err
}

// PlanAll also returns the byte offset of every statement in the query.
func PlanAll(query string) ([]int, []StmtPlan, error) {
	if !MaybeDestructive(query) {
		return nil, nil, nil
	}
	tree, err := pgquery.Parse(query)
	if err != nil {
		return nil, nil, err
	}
	var plans []StmtPlan
	var locs []int
	for _, raw := range tree.Stmts {
		locs = append(locs, int(raw.StmtLocation))
		loc := int(raw.StmtLocation)
		end := len(query)
		if raw.StmtLen > 0 {
			end = loc + int(raw.StmtLen)
		}
		if loc < 0 || loc > len(query) || end > len(query) {
			continue
		}
		text := strings.TrimSpace(query[loc:end])
		p := planStmt(raw.Stmt, text)
		if p == nil {
			continue
		}
		p.Location = loc
		p.Text = text
		plans = append(plans, *p)
	}
	return locs, plans, nil
}

func planStmt(n *pg.Node, text string) *StmtPlan {
	grp := newGroup()
	p := &StmtPlan{Group: grp}
	snapN := 0
	nextSnap := func() string {
		snapN++
		return fmt.Sprintf("%s_%d", grp, snapN)
	}
	whole := func(rv *pg.RangeVar, kind string) {
		rel := rangeVarText(rv)
		p.Targets = append(p.Targets, rel)
		p.Snaps = append(p.Snaps, SnapSQL{SQL: fmt.Sprintf("SELECT oops.snap_table(%s, %s, %s, %s, false)", lit(rel), lit(grp), lit(kind), dollar(text))})
	}
	switch {
	case n.GetDeleteStmt() != nil:
		d := n.GetDeleteStmt()
		p.Kind = "DELETE"
		if nestedDML(d.WithClause) || isCurrentOf(d.WhereClause) {
			whole(d.Relation, "update_full")
			break
		}
		rows(p, d.Relation, d.UsingClause, d.WhereClause, d.WithClause, "delete", nextSnap(), grp, text)
	case n.GetUpdateStmt() != nil:
		u := n.GetUpdateStmt()
		p.Kind = "UPDATE"
		if nestedDML(u.WithClause) || isCurrentOf(u.WhereClause) {
			whole(u.Relation, "update_full")
			break
		}
		rows(p, u.Relation, u.FromClause, u.WhereClause, u.WithClause, "update", nextSnap(), grp, text)
	case n.GetInsertStmt() != nil:
		ins := n.GetInsertStmt()
		if oc := ins.OnConflictClause; oc == nil || oc.Action != pg.OnConflictAction_ONCONFLICT_UPDATE {
			break
		}
		p.Kind = "UPSERT"
		if !upsert(p, ins, nextSnap, grp, text) {
			p.Snaps = nil
			p.Targets = nil
			whole(ins.Relation, "update_full")
		}
	case n.GetMergeStmt() != nil:
		p.Kind = "MERGE"
		whole(n.GetMergeStmt().Relation, "update_full")
	case n.GetTruncateStmt() != nil:
		t := n.GetTruncateStmt()
		p.Kind = "TRUNCATE"
		cascade := t.Behavior == pg.DropBehavior_DROP_CASCADE
		for _, r := range t.Relations {
			if rv := r.GetRangeVar(); rv != nil {
				rel := rangeVarText(rv)
				p.Targets = append(p.Targets, rel)
				p.Snaps = append(p.Snaps, SnapSQL{SQL: fmt.Sprintf("SELECT oops.snap_table(%s, %s, 'truncate', %s, %v)", lit(rel), lit(grp), dollar(text), cascade)})
			}
		}
	case n.GetDropStmt() != nil:
		d := n.GetDropStmt()
		switch d.RemoveType {
		case pg.ObjectType_OBJECT_TABLE:
			p.Kind = "DROP TABLE"
			for _, o := range d.Objects {
				rel := nameListText(o)
				if rel == "" {
					continue
				}
				p.Targets = append(p.Targets, rel)
				p.Snaps = append(p.Snaps, SnapSQL{SQL: fmt.Sprintf("SELECT oops.snap_drop_table(%s, %s, %s)", lit(rel), lit(grp), dollar(text))})
			}
		case pg.ObjectType_OBJECT_SCHEMA:
			p.Kind = "DROP SCHEMA"
			for _, o := range d.Objects {
				name := o.GetString_().GetSval()
				if name == "" {
					continue
				}
				p.Targets = append(p.Targets, name)
				p.Snaps = append(p.Snaps, SnapSQL{SQL: fmt.Sprintf("SELECT oops.snap_drop_schema(%s, %s, %s)", lit(name), lit(grp), dollar(text))})
			}
		case pg.ObjectType_OBJECT_VIEW, pg.ObjectType_OBJECT_MATVIEW:
			p.Kind = "DROP VIEW"
			if d.RemoveType == pg.ObjectType_OBJECT_MATVIEW {
				p.Kind = "DROP MATERIALIZED VIEW"
			}
			for _, o := range d.Objects {
				rel := nameListText(o)
				if rel == "" {
					continue
				}
				p.Targets = append(p.Targets, rel)
				p.Snaps = append(p.Snaps, SnapSQL{SQL: fmt.Sprintf("SELECT oops.snap_drop_view(%s, %s, %s)", lit(rel), lit(grp), dollar(text))})
			}
		case pg.ObjectType_OBJECT_FUNCTION, pg.ObjectType_OBJECT_PROCEDURE, pg.ObjectType_OBJECT_ROUTINE:
			p.Kind = "DROP FUNCTION"
			if d.RemoveType == pg.ObjectType_OBJECT_PROCEDURE {
				p.Kind = "DROP PROCEDURE"
			}
			for _, o := range d.Objects {
				sig := routineSig(o.GetObjectWithArgs())
				if sig == "" {
					continue
				}
				p.Targets = append(p.Targets, sig)
				p.Snaps = append(p.Snaps, SnapSQL{SQL: fmt.Sprintf("SELECT oops.snap_drop_routine(%s, %s, %s)", lit(sig), lit(grp), dollar(text))})
			}
		}
	case n.GetAlterTableStmt() != nil:
		a := n.GetAlterTableStmt()
		if a.Objtype != pg.ObjectType_OBJECT_TABLE || a.Relation == nil {
			break
		}
		rel := rangeVarText(a.Relation)
		for _, c := range a.Cmds {
			cmd := c.GetAlterTableCmd()
			if cmd == nil || cmd.Subtype != pg.AlterTableType_AT_DropColumn {
				continue
			}
			p.Kind = "DROP COLUMN"
			p.Targets = append(p.Targets, rel+"."+quoteIdent(cmd.Name))
			p.Snaps = append(p.Snaps, SnapSQL{SQL: fmt.Sprintf("SELECT oops.snap_column(%s, %s, %s, %s)", lit(rel), lit(cmd.Name), lit(grp), dollar(text))})
		}
	}
	// DELETE or UPDATE hidden inside other statements (data modifying CTEs).
	if p.Kind == "" {
		for _, rv := range nestedDMLTargets(n) {
			p.Kind = "nested DML"
			whole(rv, "update_full")
		}
	}
	if len(p.Snaps) == 0 {
		return nil
	}
	return p
}

// rows plans a precise copy of the rows a DELETE or UPDATE will touch.
func rows(p *StmtPlan, rv *pg.RangeVar, from []*pg.Node, where *pg.Node, with *pg.WithClause, kind, snap, grp, text string) {
	rel := rangeVarText(rv)
	p.Targets = append(p.Targets, rel)
	alias := rv.Relname
	if rv.Alias != nil && rv.Alias.Aliasname != "" {
		alias = rv.Alias.Aliasname
	}
	star := pg.MakeResTargetNodeWithVal(pg.MakeColumnRefNode([]*pg.Node{pg.MakeStrNode(alias), pg.MakeAStarNode()}, -1), -1)
	relNode := &pg.Node{Node: &pg.Node_RangeVar{RangeVar: rv}}
	sel := &pg.SelectStmt{
		TargetList:  []*pg.Node{star},
		FromClause:  []*pg.Node{relNode},
		WhereClause: where,
		WithClause:  with,
		Op:          pg.SetOperation_SETOP_NONE,
		LimitOption: pg.LimitOption_LIMIT_OPTION_DEFAULT,
	}
	if len(from) > 0 {
		// A join can repeat target rows, so pick them by ctid.
		ctid := pg.MakeColumnRefNode([]*pg.Node{pg.MakeStrNode(alias), pg.MakeStrNode("ctid")}, -1)
		inner := &pg.SelectStmt{
			TargetList:  []*pg.Node{pg.MakeResTargetNodeWithVal(ctid, -1)},
			FromClause:  append([]*pg.Node{relNode}, from...),
			WhereClause: where,
			Op:          pg.SetOperation_SETOP_NONE,
			LimitOption: pg.LimitOption_LIMIT_OPTION_DEFAULT,
		}
		sub := &pg.Node{Node: &pg.Node_SubLink{SubLink: &pg.SubLink{
			SubLinkType: pg.SubLinkType_ANY_SUBLINK,
			Testexpr:    pg.MakeColumnRefNode([]*pg.Node{pg.MakeStrNode(alias), pg.MakeStrNode("ctid")}, -1),
			Subselect:   &pg.Node{Node: &pg.Node_SelectStmt{SelectStmt: inner}},
		}}}
		sel.WhereClause = sub
	}
	tree := &pg.ParseResult{Version: 170000, Stmts: []*pg.RawStmt{{Stmt: &pg.Node{Node: &pg.Node_SelectStmt{SelectStmt: sel}}}}}
	q, err := pgquery.Deparse(tree)
	if err != nil {
		// Fall back to copying the whole table.
		p.Snaps = append(p.Snaps, SnapSQL{SQL: fmt.Sprintf("SELECT oops.snap_table(%s, %s, 'update_full', %s, false)", lit(rel), lit(grp), dollar(text))})
		return
	}
	uses := hasMessage(sel, "ParamRef")
	p.Snaps = append(p.Snaps,
		SnapSQL{SQL: fmt.Sprintf("CREATE TABLE oops.%s AS %s", quoteIdent(snap), q), UsesParams: uses},
		SnapSQL{SQL: fmt.Sprintf("SELECT oops.record_rows(%s, %s, %s, %s, %s)", lit(kind), lit(rel), lit(snap), lit(grp), dollar(text))},
	)
}

// upsert plans INSERT ... ON CONFLICT DO UPDATE: copy the incoming rows,
// then let oops.record_upsert save the existing rows they collide with.
// It returns false when the statement is too unusual to narrow down, so
// the caller copies the whole table instead.
func upsert(p *StmtPlan, ins *pg.InsertStmt, nextSnap func() string, grp, text string) bool {
	rel := rangeVarText(ins.Relation)
	inf := ins.OnConflictClause.Infer
	if rel == "" || inf == nil || inf.WhereClause != nil {
		return false // a partial unique index can let equal keys exist twice
	}
	var keys []string
	for _, ie := range inf.IndexElems {
		el := ie.GetIndexElem()
		if el == nil || el.Expr != nil || el.Name == "" {
			return false
		}
		keys = append(keys, el.Name)
	}
	if len(keys) == 0 && inf.Conname == "" {
		return false
	}
	var cols []string
	alias := &pg.Alias{Aliasname: "oops_src"}
	for _, c := range ins.Cols {
		rt := c.GetResTarget()
		if rt == nil || len(rt.Indirection) > 0 || rt.Name == "" {
			return false
		}
		cols = append(cols, rt.Name)
		alias.Colnames = append(alias.Colnames, pg.MakeStrNode(rt.Name))
	}
	src := ins.SelectStmt
	// The incoming rows are computed twice, once for the copy and once by the
	// statement, so anything that changes state when evaluated rules it out.
	if src == nil || hasMessage(src, "SetToDefault") || nestedDML(ins.WithClause) ||
		hasMessage(src, "DeleteStmt") || hasMessage(src, "UpdateStmt") || hasMessage(src, "InsertStmt") || hasMessage(src, "MergeStmt") ||
		callsAny(src, "nextval", "setval") || (ins.WithClause != nil && callsAny(ins.WithClause, "nextval", "setval")) {
		return false
	}
	star := pg.MakeResTargetNodeWithVal(pg.MakeColumnRefNode([]*pg.Node{pg.MakeAStarNode()}, -1), -1)
	sel := &pg.SelectStmt{
		TargetList:  []*pg.Node{star},
		FromClause:  []*pg.Node{{Node: &pg.Node_RangeSubselect{RangeSubselect: &pg.RangeSubselect{Subquery: src, Alias: alias}}}},
		WithClause:  ins.WithClause,
		Op:          pg.SetOperation_SETOP_NONE,
		LimitOption: pg.LimitOption_LIMIT_OPTION_DEFAULT,
	}
	tree := &pg.ParseResult{Version: 170000, Stmts: []*pg.RawStmt{{Stmt: &pg.Node{Node: &pg.Node_SelectStmt{SelectStmt: sel}}}}}
	q, err := pgquery.Deparse(tree)
	if err != nil {
		return false
	}
	raw, in, old := nextSnap(), nextSnap(), nextSnap()
	p.Targets = append(p.Targets, rel)
	p.Snaps = append(p.Snaps,
		SnapSQL{SQL: fmt.Sprintf("CREATE TABLE oops.%s AS %s", quoteIdent(raw), q), UsesParams: hasMessage(sel, "ParamRef")},
		SnapSQL{SQL: fmt.Sprintf("SELECT oops.record_upsert(%s, %s, %s, %s, %s, %s, %s, %s, %s)",
			lit(rel), textArray(cols), lit(raw), lit(in), lit(old), textArray(keys), lit(inf.Conname), lit(grp), dollar(text))},
	)
	return true
}

func textArray(items []string) string {
	if len(items) == 0 {
		return "'{}'::text[]"
	}
	var parts []string
	for _, it := range items {
		parts = append(parts, lit(it))
	}
	return "ARRAY[" + strings.Join(parts, ", ") + "]::text[]"
}

// callsAny reports whether m calls a function with one of the given names.
func callsAny(m proto.Message, names ...string) bool {
	found := false
	walk(m.ProtoReflect(), func(x protoreflect.Message) {
		fc, ok := x.Interface().(*pg.FuncCall)
		if !ok || len(fc.Funcname) == 0 {
			return
		}
		last := strings.ToLower(fc.Funcname[len(fc.Funcname)-1].GetString_().GetSval())
		for _, n := range names {
			if last == n {
				found = true
			}
		}
	})
	return found
}

func isCurrentOf(n *pg.Node) bool { return n != nil && n.GetCurrentOfExpr() != nil }

func nestedDML(w *pg.WithClause) bool {
	if w == nil {
		return false
	}
	return hasMessage(w, "DeleteStmt") || hasMessage(w, "UpdateStmt") || hasMessage(w, "InsertStmt") || hasMessage(w, "MergeStmt")
}

// nestedDMLTargets finds DELETE, UPDATE and MERGE statements anywhere inside n.
func nestedDMLTargets(n *pg.Node) []*pg.RangeVar {
	var out []*pg.RangeVar
	walk(n.ProtoReflect(), func(m protoreflect.Message) {
		switch x := m.Interface().(type) {
		case *pg.DeleteStmt:
			out = append(out, x.Relation)
		case *pg.UpdateStmt:
			out = append(out, x.Relation)
		case *pg.MergeStmt:
			out = append(out, x.Relation)
		case *pg.InsertStmt:
			if oc := x.OnConflictClause; oc != nil && oc.Action == pg.OnConflictAction_ONCONFLICT_UPDATE {
				out = append(out, x.Relation)
			}
		}
	})
	return out
}

func hasMessage(m proto.Message, name string) bool {
	found := false
	walk(m.ProtoReflect(), func(x protoreflect.Message) {
		if string(x.Descriptor().Name()) == name {
			found = true
		}
	})
	return found
}

func walk(m protoreflect.Message, fn func(protoreflect.Message)) {
	if !m.IsValid() {
		return
	}
	fn(m)
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList() && fd.Message() != nil:
			l := v.List()
			for i := 0; i < l.Len(); i++ {
				walk(l.Get(i).Message(), fn)
			}
		case fd.Message() != nil && !fd.IsMap():
			walk(v.Message(), fn)
		}
		return true
	})
}

func rangeVarText(rv *pg.RangeVar) string {
	if rv == nil {
		return ""
	}
	if rv.Schemaname != "" {
		return quoteIdent(rv.Schemaname) + "." + quoteIdent(rv.Relname)
	}
	return quoteIdent(rv.Relname)
}

func nameListText(n *pg.Node) string {
	l := n.GetList()
	if l == nil {
		return ""
	}
	var parts []string
	for _, it := range l.Items {
		parts = append(parts, quoteIdent(it.GetString_().GetSval()))
	}
	return strings.Join(parts, ".")
}

// routineSig renders a function reference as name(argtypes), the form
// to_regprocedure understands, or as a bare name when no arguments were given.
func routineSig(o *pg.ObjectWithArgs) string {
	if o == nil || len(o.Objname) == 0 {
		return ""
	}
	var parts []string
	for _, n := range o.Objname {
		parts = append(parts, quoteIdent(n.GetString_().GetSval()))
	}
	name := strings.Join(parts, ".")
	if o.ArgsUnspecified {
		return name
	}
	var args []string
	for _, a := range o.Objargs {
		tn := a.GetTypeName()
		if tn == nil {
			return ""
		}
		// Deparse each type on its own as a cast, then cut it out.
		cast := &pg.Node{Node: &pg.Node_TypeCast{TypeCast: &pg.TypeCast{Arg: pg.MakeAConstIntNode(0, -1), TypeName: tn}}}
		sel := &pg.SelectStmt{TargetList: []*pg.Node{pg.MakeResTargetNodeWithVal(cast, -1)}, Op: pg.SetOperation_SETOP_NONE, LimitOption: pg.LimitOption_LIMIT_OPTION_DEFAULT}
		q, err := pgquery.Deparse(&pg.ParseResult{Version: 170000, Stmts: []*pg.RawStmt{{Stmt: &pg.Node{Node: &pg.Node_SelectStmt{SelectStmt: sel}}}}})
		if err != nil {
			return ""
		}
		q = strings.TrimPrefix(q, "SELECT 0::")
		if strings.HasPrefix(q, "SELECT CAST(0 AS ") {
			q = strings.TrimSuffix(strings.TrimPrefix(q, "SELECT CAST(0 AS "), ")")
		}
		args = append(args, q)
	}
	return name + "(" + strings.Join(args, ", ") + ")"
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func lit(s string) string {
	return "E'" + strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(s) + "'"
}

func dollar(s string) string {
	tag := "$oopsy$"
	for i := 0; strings.Contains(s, tag); i++ {
		tag = fmt.Sprintf("$oopsy%d$", i)
	}
	return tag + s + tag
}
