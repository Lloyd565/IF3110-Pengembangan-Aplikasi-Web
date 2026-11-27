package sqlpolicy

import (
	"errors"
	"slices"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
)

func BatchTarget(sql string) (string, string, error) {
	tree, err := Parse(sql)
	if err != nil || len(tree.Stmts) != 1 {
		return "", "", errors.New("SQL kompensasi batch tidak valid.")
	}
	var rel *pg.RangeVar
	var where *pg.Node
	if u := tree.Stmts[0].Stmt.GetUpdateStmt(); u != nil {
		rel, where = u.Relation, u.WhereClause
	}
	if d := tree.Stmts[0].Stmt.GetDeleteStmt(); d != nil {
		rel, where = d.Relation, d.WhereClause
	}
	if rel == nil {
		return "", "", errors.New("Kompensasi batch memerlukan UPDATE atau DELETE berbatas cursor.")
	}
	var column string
	var find func(*pg.Node)
	find = func(n *pg.Node) {
		if b := n.GetBoolExpr(); b != nil && b.Boolop == pg.BoolExprType_AND_EXPR {
			for _, a := range b.Args {
				find(a)
			}
		}
		if e := n.GetAExpr(); e != nil && e.Rexpr.GetParamRef().GetNumber() == 1 && e.Lexpr.GetColumnRef() != nil {
			fields := Strings(e.Lexpr.GetColumnRef().Fields)
			if len(fields) > 0 {
				column = fields[len(fields)-1]
			}
		}
	}
	find(where)
	name := rel.Schemaname + "." + rel.Relname
	return name, column, Batch(sql, name, column)
}

func Batch(sql, table, column string) error {
	tree, err := Parse(sql)
	if err != nil {
		return err
	}
	invalid := errors.New("Batch memerlukan satu statement dengan cakupan schema.table dan batas cursor > $1 AND cursor <= $2; cursor tidak boleh diubah.")
	if len(tree.Stmts) != 1 {
		return invalid
	}
	var rel *pg.RangeVar
	var where *pg.Node
	var cte *pg.WithClause
	n := tree.Stmts[0].Stmt
	if u := n.GetUpdateStmt(); u != nil {
		rel, where, cte = u.Relation, u.WhereClause, u.WithClause
		if len(u.FromClause) > 0 {
			return invalid
		}
		for _, target := range u.TargetList {
			if target.GetResTarget().GetName() == column {
				return invalid
			}
		}
	} else if d := n.GetDeleteStmt(); d != nil {
		rel, where, cte = d.Relation, d.WhereClause, d.WithClause
		if len(d.UsingClause) > 0 {
			return invalid
		}
	} else {
		s := n.GetSelectStmt()
		if ins := n.GetInsertStmt(); ins != nil && ins.WithClause == nil {
			s = ins.SelectStmt.GetSelectStmt()
		}
		if s == nil || len(s.FromClause) != 1 || s.Op != pg.SetOperation_SETOP_NONE || len(s.GroupClause) > 0 || s.LimitCount != nil || s.LimitOffset != nil {
			return invalid
		}
		rel, where, cte = s.FromClause[0].GetRangeVar(), s.WhereClause, s.WithClause
	}
	if cte != nil || rel == nil || rel.Catalogname != "" || rel.Schemaname+"."+rel.Relname != table {
		return invalid
	}
	qualifier := rel.Relname
	if rel.Alias != nil {
		qualifier = rel.Alias.Aliasname
	}
	bound := func(n *pg.Node, op string, param int32) bool {
		e := n.GetAExpr()
		if e == nil || e.Kind != pg.A_Expr_Kind_AEXPR_OP || strings.Join(Strings(e.Name), ".") != op || e.Rexpr.GetParamRef().GetNumber() != param {
			return false
		}
		c := e.Lexpr.GetColumnRef()
		if c == nil {
			return false
		}
		fields := Strings(c.Fields)
		return slices.Equal(fields, []string{column}) || slices.Equal(fields, []string{qualifier, column}) || slices.Equal(fields, []string{rel.Schemaname, rel.Relname, column})
	}
	var conjuncts func(*pg.Node) []*pg.Node
	conjuncts = func(n *pg.Node) []*pg.Node {
		if b := n.GetBoolExpr(); b != nil && b.Boolop == pg.BoolExprType_AND_EXPR {
			var out []*pg.Node
			for _, a := range b.Args {
				out = append(out, conjuncts(a)...)
			}
			return out
		}
		return []*pg.Node{n}
	}
	lower, upper := false, false
	for _, n := range conjuncts(where) {
		lower = lower || bound(n, ">", 1)
		upper = upper || bound(n, "<=", 2)
	}
	if !lower || !upper {
		return invalid
	}
	return Check(sql)
}
