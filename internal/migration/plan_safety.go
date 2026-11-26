package migration

import (
	"context"
	"slices"

	pg "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/sqlpolicy"
	"github.com/jackc/pgx/v5"
)

func safePlan(ctx context.Context, tx pgx.Tx, node *pg.Node, analyze bool) error {
	if analyze && node.GetSelectStmt() == nil {
		return ValidationError("EXPLAIN ANALYZE hanya tersedia untuk SELECT yang tidak mengubah state.")
	}
	readOnlyRelation := node.GetSelectStmt() != nil
	virtual := map[string]bool{}
	var walk func(protoreflect.Message) error
	walk = func(m protoreflect.Message) error {
		var clause *pg.WithClause
		switch x := m.Interface().(type) {
		case *pg.SelectStmt:
			clause = x.WithClause
		case *pg.InsertStmt:
			clause = x.WithClause
		case *pg.UpdateStmt:
			clause = x.WithClause
		case *pg.DeleteStmt:
			clause = x.WithClause
		case *pg.MergeStmt:
			clause = x.WithClause
		}
		if clause != nil {
			previous := virtual
			virtual = map[string]bool{}
			for name, value := range previous {
				virtual[name] = value
			}
			defer func() { virtual = previous }()
			if clause.Recursive {
				for _, cte := range clause.Ctes {
					virtual[cte.GetCommonTableExpr().GetCtename()] = true
				}
			}
			for _, cte := range clause.Ctes {
				if err := walk(cte.ProtoReflect()); err != nil {
					return err
				}
				virtual[cte.GetCommonTableExpr().GetCtename()] = true
			}
		}
		switch x := m.Interface().(type) {
		case *pg.SelectStmt:
			if x.IntoClause != nil || len(x.LockingClause) > 0 {
				return ValidationError("Analisis tidak mengizinkan SELECT INTO atau row lock.")
			}
		case *pg.InsertStmt, *pg.UpdateStmt, *pg.DeleteStmt, *pg.MergeStmt:
			readOnlyRelation = false
			if analyze {
				return ValidationError("Analisis tidak mengizinkan perubahan data, termasuk CTE.")
			}
		case *pg.RangeTableSample:
			return ValidationError("TABLESAMPLE tidak tersedia untuk analisis aman.")
		case *pg.FuncCall:
			names := sqlpolicy.Strings(x.Funcname)
			if len(names) == 0 || len(names) > 2 || len(names) == 2 && names[0] != "pg_catalog" || !slices.Contains([]string{"count", "sum", "avg", "min", "max", "lower", "upper", "length", "abs", "round", "coalesce", "nullif"}, names[len(names)-1]) {
				return ValidationError("Function ini tidak tersedia untuk analisis tanpa efek samping.")
			}
			x.Funcname = []*pg.Node{{Node: &pg.Node_String_{String_: &pg.String{Sval: "pg_catalog"}}}, {Node: &pg.Node_String_{String_: &pg.String{Sval: names[len(names)-1]}}}}
		case *pg.TypeCast:
			names := sqlpolicy.Strings(x.TypeName.Names)
			if len(names) == 0 || len(names) > 2 || len(names) == 2 && names[0] != "pg_catalog" || !slices.Contains([]string{"int2", "int4", "int8", "numeric", "float4", "float8", "text", "varchar", "bpchar", "bool", "date", "timestamp", "timestamptz", "interval"}, names[len(names)-1]) {
				return ValidationError("Tipe cast ini tidak tersedia untuk analisis aman.")
			}
		case *pg.A_Expr:
			names := sqlpolicy.Strings(x.Name)
			if len(names) != 1 || !slices.Contains([]string{"=", "<>", "!=", "<", ">", "<=", ">=", "+", "-", "*", "/", "%", "||", "~~", "!~~", "~~*", "!~~*"}, names[0]) {
				return ValidationError("Operator ini tidak tersedia untuk analisis aman.")
			}
			x.Name = []*pg.Node{{Node: &pg.Node_String_{String_: &pg.String{Sval: "pg_catalog"}}}, {Node: &pg.Node_String_{String_: &pg.String{Sval: names[0]}}}}
		case *pg.RangeVar:
			if x.Schemaname == "" && x.Catalogname == "" && virtual[x.Relname] {
				return nil
			}
			if x.Catalogname != "" {
				return ValidationError("Analisis lintas database tidak didukung.")
			}
			if x.Schemaname == "" {
				x.Schemaname = "public"
			}
			name := pgx.Identifier{x.Relname}.Sanitize()
			if x.Schemaname != "" {
				name = pgx.Identifier{x.Schemaname, x.Relname}.Sanitize()
			}
			if _, err := tx.Exec(ctx, "LOCK TABLE "+name+" IN ACCESS SHARE MODE"); err != nil {
				return ValidationError(errorMessage(err))
			}
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_class c WHERE c.oid = to_regclass($1)
				AND c.relkind = 'r' AND c.relpersistence <> 't' AND NOT c.relrowsecurity AND NOT c.relhassubclass
				AND NOT EXISTS (SELECT 1 FROM pg_attribute a JOIN pg_type t ON t.oid = a.atttypid
				 WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped AND (t.typnamespace <> 'pg_catalog'::regnamespace OR t.typtype <> 'b'))
				AND NOT EXISTS (SELECT 1 FROM pg_index i JOIN pg_class idx ON idx.oid=i.indexrelid WHERE i.indrelid = c.oid AND (i.indexprs IS NOT NULL OR i.indpred IS NOT NULL OR idx.relam >= 16384))
				AND NOT EXISTS (SELECT 1 FROM pg_rewrite r WHERE r.ev_class = c.oid)
 AND NOT EXISTS (SELECT 1 FROM pg_constraint co JOIN pg_depend dep ON dep.classid='pg_constraint'::regclass AND dep.objid=co.oid
 WHERE co.conrelid=c.oid AND co.contype='c' AND dep.refclassid NOT IN ('pg_class'::regclass,'pg_namespace'::regclass))
				AND ($2 OR NOT EXISTS (SELECT 1 FROM pg_attrdef d WHERE d.adrelid = c.oid))
AND NOT EXISTS (SELECT 1 FROM pg_index i, unnest(i.indclass) cl JOIN pg_opclass oc ON oc.oid=cl WHERE i.indrelid=c.oid AND oc.opcnamespace <> 'pg_catalog'::regnamespace)
			)`, name, readOnlyRelation).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return ValidationError("Analisis memerlukan tabel biasa tanpa policy, rule, default, index ekspresi, atau tipe pengguna yang dapat menjalankan function.")
			}
		}
		var err error
		m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			if clause != nil && string(f.Name()) == "with_clause" {
				return true
			}
			if f.Kind() != protoreflect.MessageKind {
				return true
			}
			if f.IsList() {
				list := v.List()
				for i := 0; i < list.Len(); i++ {
					if err = walk(list.Get(i).Message()); err != nil {
						return false
					}
				}
			} else {
				err = walk(v.Message())
			}
			return err == nil
		})
		return err
	}
	return walk(node.ProtoReflect())
}
