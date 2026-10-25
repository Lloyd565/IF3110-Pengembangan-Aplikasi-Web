package explorer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
)

func mariaIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

func mariaRead(ctx context.Context, target *dbconn.Target, fn func(context.Context, *sql.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := target.Maria.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(ctx, tx)
}

func mariaTable(ctx context.Context, tx *sql.Tx, schema, table string) (string, error) {
	var kind string
	err := tx.QueryRowContext(ctx, `SELECT CASE TABLE_TYPE WHEN 'VIEW' THEN 'view' ELSE 'tabel' END
		FROM information_schema.TABLES WHERE TABLE_SCHEMA=? AND TABLE_NAME=?`, schema, table).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return kind, err
}

func mariaSchemas(ctx context.Context, target *dbconn.Target) ([]Schema, error) {
	var out []Schema
	err := mariaRead(ctx, target, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT s.SCHEMA_NAME, COALESCE(t.TABLE_NAME,''),
			CASE t.TABLE_TYPE WHEN 'VIEW' THEN 'view' ELSE 'tabel' END
			FROM information_schema.SCHEMATA s LEFT JOIN information_schema.TABLES t ON t.TABLE_SCHEMA=s.SCHEMA_NAME
			WHERE s.SCHEMA_NAME=? ORDER BY t.TABLE_NAME`, target.Database)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var schema string
			var table Table
			if err := rows.Scan(&schema, &table.Name, &table.Kind); err != nil {
				return err
			}
			if len(out) == 0 {
				out = append(out, Schema{Name: schema})
			}
			if table.Name != "" {
				out[0].Tables = append(out[0].Tables, table)
			}
		}
		return rows.Err()
	})
	return out, err
}

func mariaDescribe(ctx context.Context, target *dbconn.Target, schema, table string) (*TableDetail, error) {
	if schema != target.Database {
		return nil, ErrNotFound
	}
	d := &TableDetail{Schema: schema, Name: table}
	err := mariaRead(ctx, target, func(ctx context.Context, tx *sql.Tx) error {
		kind, err := mariaTable(ctx, tx, schema, table)
		if err != nil {
			return err
		}
		d.Kind = kind
		rows, err := tx.QueryContext(ctx, `SELECT COLUMN_NAME,COLUMN_TYPE,IS_NULLABLE,COALESCE(COLUMN_DEFAULT,'')
			FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY ORDINAL_POSITION`, schema, table)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c Column
			var nullable string
			if err := rows.Scan(&c.Name, &c.Type, &nullable, &c.Default); err != nil {
				rows.Close()
				return err
			}
			c.Nullable = nullable == "YES"
			d.Columns = append(d.Columns, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		rows, err = tx.QueryContext(ctx, `SELECT CONSTRAINT_NAME,CONSTRAINT_TYPE FROM information_schema.TABLE_CONSTRAINTS
			WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY CASE CONSTRAINT_TYPE WHEN 'PRIMARY KEY' THEN 0 ELSE 1 END,CONSTRAINT_NAME`, schema, table)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c Constraint
			if err := rows.Scan(&c.Name, &c.Type); err != nil {
				rows.Close()
				return err
			}
			d.Constraints = append(d.Constraints, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for i := range d.Constraints {
			c := &d.Constraints[i]
			if c.Type == "CHECK" {
				err = tx.QueryRowContext(ctx, `SELECT CHECK_CLAUSE FROM information_schema.CHECK_CONSTRAINTS
					WHERE CONSTRAINT_SCHEMA=? AND TABLE_NAME=? AND CONSTRAINT_NAME=?`, schema, table, c.Name).Scan(&c.Definition)
				if err != nil {
					return err
				}
				continue
			}
			rows, err = tx.QueryContext(ctx, `SELECT COLUMN_NAME,COALESCE(REFERENCED_TABLE_SCHEMA,''),
				COALESCE(REFERENCED_TABLE_NAME,''),COALESCE(REFERENCED_COLUMN_NAME,'')
				FROM information_schema.KEY_COLUMN_USAGE WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND CONSTRAINT_NAME=? ORDER BY ORDINAL_POSITION`, schema, table, c.Name)
			if err != nil {
				return err
			}
			var columns, refs []string
			var refSchema, refTable string
			for rows.Next() {
				var col, ref string
				if err := rows.Scan(&col, &refSchema, &refTable, &ref); err != nil {
					rows.Close()
					return err
				}
				columns = append(columns, mariaIdent(col))
				refs = append(refs, mariaIdent(ref))
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			c.Definition = c.Type + " (" + strings.Join(columns, ", ") + ")"
			if c.Type == "FOREIGN KEY" {
				var updateRule, deleteRule string
				if err := tx.QueryRowContext(ctx, `SELECT UPDATE_RULE,DELETE_RULE FROM information_schema.REFERENTIAL_CONSTRAINTS
					WHERE CONSTRAINT_SCHEMA=? AND TABLE_NAME=? AND CONSTRAINT_NAME=?`, schema, table, c.Name).Scan(&updateRule, &deleteRule); err != nil {
					return err
				}
				c.Definition += " REFERENCES " + mariaIdent(refSchema) + "." + mariaIdent(refTable) + " (" + strings.Join(refs, ", ") + ") ON UPDATE " + updateRule + " ON DELETE " + deleteRule
			}
		}
		rows, err = tx.QueryContext(ctx, `SELECT INDEX_NAME,NON_UNIQUE,COALESCE(COLUMN_NAME,'(expression)'),INDEX_TYPE,SUB_PART,COLLATION
			FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY INDEX_NAME,SEQ_IN_INDEX`, schema, table)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name, col, kind string
			var nonUnique int
			var prefix sql.NullInt64
			var order sql.NullString
			if err := rows.Scan(&name, &nonUnique, &col, &kind, &prefix, &order); err != nil {
				return err
			}
			value := mariaIdent(col)
			if prefix.Valid {
				value += fmt.Sprintf("(%d)", prefix.Int64)
			}
			if order.String == "D" {
				value += " DESC"
			}
			if len(d.Indexes) > 0 && d.Indexes[len(d.Indexes)-1].Name == name {
				ix := &d.Indexes[len(d.Indexes)-1]
				ix.Definition = strings.TrimSuffix(ix.Definition, ")") + ", " + value + ")"
			} else {
				unique := ""
				if nonUnique == 0 {
					unique = "UNIQUE "
				}
				d.Indexes = append(d.Indexes, Index{Name: name, Definition: unique + "INDEX " + mariaIdent(name) + " USING " + kind + " (" + value + ")"})
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

func mariaData(ctx context.Context, target *dbconn.Target, schema, table string, page int) (*DataPage, error) {
	if schema != target.Database {
		return nil, ErrNotFound
	}
	if page < 1 {
		page = 1
	}
	if int64(page-1) > math.MaxInt64/PageSize {
		return nil, errors.New("nomor halaman terlalu besar")
	}
	out := &DataPage{Page: page}
	err := mariaRead(ctx, target, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := mariaTable(ctx, tx, schema, table); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT COLUMN_NAME FROM information_schema.KEY_COLUMN_USAGE
			WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND CONSTRAINT_NAME='PRIMARY' ORDER BY ORDINAL_POSITION`, schema, table)
		if err != nil {
			return err
		}
		var pk []string
		for rows.Next() {
			var col string
			if err := rows.Scan(&col); err != nil {
				rows.Close()
				return err
			}
			pk = append(pk, mariaIdent(col))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		query := "SELECT * FROM " + mariaIdent(schema) + "." + mariaIdent(table)
		if len(pk) > 0 {
			query += " ORDER BY " + strings.Join(pk, ", ")
		}
		query += " LIMIT ? OFFSET ?"
		rows, err = tx.QueryContext(ctx, query, PageSize+1, int64(page-1)*PageSize)
		if err != nil {
			return err
		}
		defer rows.Close()
		out.Columns, err = rows.Columns()
		if err != nil {
			return err
		}
		for rows.Next() {
			raw := make([]sql.RawBytes, len(out.Columns))
			dest := make([]any, len(raw))
			for i := range raw {
				dest[i] = &raw[i]
			}
			if err := rows.Scan(dest...); err != nil {
				return err
			}
			row := make([]Cell, len(raw))
			for i, value := range raw {
				if value == nil {
					row[i].Null = true
					continue
				}
				runes := []rune(string(value))
				if len(runes) > maxCellLen {
					row[i].Value = string(runes[:maxCellLen]) + "…"
				} else {
					row[i].Value = string(value)
				}
			}
			out.Rows = append(out.Rows, row)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(out.Rows) > PageSize {
			out.HasNext = true
			out.Rows = out.Rows[:PageSize]
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
