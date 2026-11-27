package sqlpolicy

import (
	pg "github.com/pganalyze/pg_query_go/v6"
	parser "github.com/wasilibs/go-pgquery"
)

func BatchReplay(source, column string, down bool) (string, bool, error) {
	tree, err := Parse(source)
	if err != nil {
		return "", false, err
	}
	n := tree.Stmts[0].Stmt
	var rel *pg.RangeVar
	var where **pg.Node
	var returning *[]*pg.Node
	if u := n.GetUpdateStmt(); u != nil {
		rel, where, returning = u.Relation, &u.WhereClause, &u.ReturningList
	} else if d := n.GetDeleteStmt(); d != nil && down {
		rel, where = d.Relation, &d.WhereClause
	} else {
		return source, false, nil
	}
	qualifier := rel.Relname
	if rel.Alias != nil {
		qualifier = rel.Alias.Aliasname
	}
	field := func(col string) *pg.Node {
		return &pg.Node{Node: &pg.Node_ColumnRef{ColumnRef: &pg.ColumnRef{Fields: []*pg.Node{
			{Node: &pg.Node_String_{String_: &pg.String{Sval: qualifier}}},
			{Node: &pg.Node_String_{String_: &pg.String{Sval: col}}},
		}}}}
	}
	if down {
		template, err := Parse("SELECT 1 WHERE id = ANY($3::bigint[])")
		if err != nil {
			return "", false, err
		}
		filter := template.Stmts[0].Stmt.GetSelectStmt().WhereClause
		filter.GetAExpr().Lexpr = field(column)
		*where = &pg.Node{Node: &pg.Node_BoolExpr{BoolExpr: &pg.BoolExpr{Boolop: pg.BoolExprType_AND_EXPR, Args: []*pg.Node{*where, filter}}}}
	} else {
		template, err := Parse("UPDATE t SET x=0 RETURNING id, xmin::text, ctid::text")
		if err != nil {
			return "", false, err
		}
		fields := template.Stmts[0].Stmt.GetUpdateStmt().ReturningList
		fields[0].GetResTarget().Val = field(column)
		fields[1].GetResTarget().Val.GetTypeCast().Arg = field("xmin")
		fields[2].GetResTarget().Val.GetTypeCast().Arg = field("ctid")
		*returning = append(fields, *returning...)
	}
	text, err := parser.Deparse(tree)
	return text, true, err
}
