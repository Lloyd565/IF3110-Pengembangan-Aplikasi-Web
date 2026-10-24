package explorer

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	PageSize    = 50
	maxCellLen  = 200
	readTimeout = "5s"
)

var ErrNotFound = errors.New("explorer: tabel tidak ditemukan")

type Schema struct {
	Name   string
	Tables []Table
}

type Table struct {
	Name string
	Kind string
}

type Column struct {
	Name     string
	Type     string
	Nullable bool
	Default  string
}

type Constraint struct {
	Name       string
	Type       string
	Definition string
}

type Index struct {
	Name       string
	Definition string
}

type TableDetail struct {
	Schema      string
	Name        string
	Kind        string
	Columns     []Column
	Constraints []Constraint
	Indexes     []Index
}

type Cell struct {
	Value string
	Null  bool
}

type DataPage struct {
	Columns []string
	Rows    [][]Cell
	Page    int
	HasNext bool
}

func readOnly(ctx context.Context, db *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+readTimeout+"'"); err != nil {
		return err
	}
	return fn(tx)
}

const kindSQL = `CASE c.relkind WHEN 'r' THEN 'tabel' WHEN 'p' THEN 'tabel (partisi)'
	WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized view' WHEN 'f' THEN 'foreign table' END`

func ListSchemas(ctx context.Context, db *pgxpool.Pool) ([]Schema, error) {
	var schemas []Schema
	err := readOnly(ctx, db, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT n.nspname, coalesce(c.relname, ''), coalesce(`+kindSQL+`, '')
			FROM pg_namespace n
			LEFT JOIN pg_class c ON c.relnamespace = n.oid
				AND c.relkind IN ('r','p','v','m','f') AND NOT c.relispartition
			WHERE n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%'
			ORDER BY n.nspname, c.relname`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var schema string
			var t Table
			if err := rows.Scan(&schema, &t.Name, &t.Kind); err != nil {
				return err
			}
			if len(schemas) == 0 || schemas[len(schemas)-1].Name != schema {
				schemas = append(schemas, Schema{Name: schema})
			}
			if t.Name != "" {
				last := &schemas[len(schemas)-1]
				last.Tables = append(last.Tables, t)
			}
		}
		return rows.Err()
	})
	return schemas, err
}

func findTable(ctx context.Context, tx pgx.Tx, schema, table string) (oid uint32, kind string, err error) {
	err = tx.QueryRow(ctx, `
		SELECT c.oid, `+kindSQL+`
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r','p','v','m','f')`,
		schema, table).Scan(&oid, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return oid, kind, err
}

func DescribeTable(ctx context.Context, db *pgxpool.Pool, schema, table string) (*TableDetail, error) {
	d := &TableDetail{Schema: schema, Name: table}
	err := readOnly(ctx, db, func(tx pgx.Tx) error {
		oid, kind, err := findTable(ctx, tx, schema, table)
		if err != nil {
			return err
		}
		d.Kind = kind

		d.Columns, err = collect(ctx, tx, `
			SELECT a.attname, format_type(a.atttypid, a.atttypmod), NOT a.attnotnull,
			       coalesce(pg_get_expr(ad.adbin, ad.adrelid), '')
			FROM pg_attribute a
			LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
			WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped
			ORDER BY a.attnum`, oid,
			func(r pgx.Rows, c *Column) error { return r.Scan(&c.Name, &c.Type, &c.Nullable, &c.Default) })
		if err != nil {
			return err
		}

		d.Constraints, err = collect(ctx, tx, `
			SELECT conname,
			       CASE contype WHEN 'p' THEN 'PRIMARY KEY' WHEN 'f' THEN 'FOREIGN KEY' WHEN 'u' THEN 'UNIQUE'
			                    WHEN 'c' THEN 'CHECK' WHEN 'x' THEN 'EXCLUDE' ELSE contype::text END,
			       pg_get_constraintdef(oid)
			FROM pg_constraint WHERE conrelid = $1
			ORDER BY array_position(ARRAY['p','u','f','c','x']::"char"[], contype), conname`, oid,
			func(r pgx.Rows, c *Constraint) error { return r.Scan(&c.Name, &c.Type, &c.Definition) })
		if err != nil {
			return err
		}

		d.Indexes, err = collect(ctx, tx, `
			SELECT i.relname, pg_get_indexdef(i.oid)
			FROM pg_index x JOIN pg_class i ON i.oid = x.indexrelid
			WHERE x.indrelid = $1 ORDER BY i.relname`, oid,
			func(r pgx.Rows, ix *Index) error { return r.Scan(&ix.Name, &ix.Definition) })
		return err
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

func collect[T any](ctx context.Context, tx pgx.Tx, sql string, arg any, scan func(pgx.Rows, *T) error) ([]T, error) {
	rows, err := tx.Query(ctx, sql, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		var v T
		if err := scan(rows, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func ReadData(ctx context.Context, db *pgxpool.Pool, schema, table string, page int) (*DataPage, error) {
	if page < 1 {
		page = 1
	}
	out := &DataPage{Page: page}
	err := readOnly(ctx, db, func(tx pgx.Tx) error {
		oid, _, err := findTable(ctx, tx, schema, table)
		if err != nil {
			return err
		}

		pk, err := collect(ctx, tx, `
			SELECT a.attname
			FROM pg_index x JOIN pg_attribute a ON a.attrelid = x.indrelid AND a.attnum = ANY (x.indkey)
			WHERE x.indrelid = $1 AND x.indisprimary
			ORDER BY array_position(x.indkey::int2[], a.attnum)`, oid,
			func(r pgx.Rows, s *string) error { return r.Scan(s) })
		if err != nil {
			return err
		}

		q := "SELECT * FROM " + pgx.Identifier{schema, table}.Sanitize()
		if len(pk) > 0 {
			cols := make([]string, len(pk))
			for i, c := range pk {
				cols[i] = pgx.Identifier{c}.Sanitize()
			}
			q += " ORDER BY " + strings.Join(cols, ", ")
		}
		q += " LIMIT $1 OFFSET $2"

		rows, err := tx.Query(ctx, q, pgx.QueryExecModeSimpleProtocol, PageSize+1, (page-1)*PageSize)
		if err != nil {
			return err
		}
		defer rows.Close()
		for _, f := range rows.FieldDescriptions() {
			out.Columns = append(out.Columns, f.Name)
		}
		for rows.Next() {
			raw := rows.RawValues()
			row := make([]Cell, len(raw))
			for i, v := range raw {
				if v == nil {
					row[i] = Cell{Null: true}
					continue
				}
				s := string(v)
				if r := []rune(s); len(r) > maxCellLen {
					s = string(r[:maxCellLen]) + "…"
				}
				row[i] = Cell{Value: s}
			}
			out.Rows = append(out.Rows, row)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(out.Rows) > PageSize {
			out.Rows = out.Rows[:PageSize]
			out.HasNext = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
