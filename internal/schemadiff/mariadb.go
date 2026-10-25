package schemadiff

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
)

func readMaria(ctx context.Context, target *dbconn.Target, schema string) (Snapshot, error) {
	out := Snapshot{DBMS: dbconn.MariaDB, Schema: schema}
	tx, err := target.Maria.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME=?`, schema).Scan(&exists); err != nil {
		return out, err
	}
	if exists == 0 {
		return out, ErrNotFound
	}
	rows, err := tx.QueryContext(ctx, `SELECT t.TABLE_NAME,t.TABLE_TYPE,COALESCE(t.ENGINE,''),COALESCE(c.CHARACTER_SET_NAME,''),COALESCE(t.TABLE_COLLATION,''),COALESCE(t.CREATE_OPTIONS,'')
		FROM information_schema.TABLES t LEFT JOIN information_schema.CHARACTER_SETS c ON LEFT(t.TABLE_COLLATION,LENGTH(c.CHARACTER_SET_NAME)+1)=CONCAT(c.CHARACTER_SET_NAME,'_')
		WHERE t.TABLE_SCHEMA=? ORDER BY t.TABLE_NAME`, schema)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var t Table
		var kind, options string
		if err := rows.Scan(&t.Name, &kind, &t.Engine, &t.Charset, &t.Collation, &options); err != nil {
			rows.Close()
			return out, err
		}
		if kind != "BASE TABLE" {
			t.Manual = append(t.Manual, "jenis objek "+kind)
		}
		if kind == "BASE TABLE" && t.Engine != "InnoDB" {
			t.Manual = append(t.Manual, "engine "+t.Engine)
		}
		if options != "" {
			t.Manual = append(t.Manual, "opsi tabel "+options)
		}
		out.Tables = append(out.Tables, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	// Hold metadata locks on ordinary tables until the read transaction ends.
	for _, t := range out.Tables {
		if len(t.Manual) > 0 {
			continue
		}
		lockRows, err := tx.QueryContext(ctx, "SELECT * FROM "+qualified(dbconn.MariaDB, schema, t.Name)+" LIMIT 0")
		if err != nil {
			return out, err
		}
		lockRows.Close()
	}
	for i := range out.Tables {
		t := &out.Tables[i]
		if len(t.Manual) > 0 {
			var definition sql.NullString
			_ = tx.QueryRowContext(ctx, `SELECT VIEW_DEFINITION FROM information_schema.VIEWS WHERE TABLE_SCHEMA=? AND TABLE_NAME=?`, schema, t.Name).Scan(&definition)
			if definition.Valid {
				t.Manual = append(t.Manual, definition.String)
			}
			continue
		}
		rows, err = tx.QueryContext(ctx, `SELECT COLUMN_NAME,COLUMN_TYPE,IS_NULLABLE,COLUMN_DEFAULT,COALESCE(CHARACTER_SET_NAME,''),COALESCE(COLLATION_NAME,''),EXTRA,COALESCE(GENERATION_EXPRESSION,''),COLUMN_COMMENT
			FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY ORDINAL_POSITION`, schema, t.Name)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var c Column
			var nullable, extra, generated string
			var def sql.NullString
			if err := rows.Scan(&c.Name, &c.Type, &nullable, &def, &c.Charset, &c.Collation, &extra, &generated, &c.Comment); err != nil {
				rows.Close()
				return out, err
			}
			c.Nullable = nullable == "YES"
			if def.Valid && !strings.EqualFold(def.String, "NULL") {
				c.Default = def.String
			}
			extra = strings.ToLower(extra)
			if strings.Contains(extra, "auto_increment") {
				c.Auto = "auto_increment"
				extra = strings.TrimSpace(strings.ReplaceAll(extra, "auto_increment", ""))
			}
			c.Extra = extra
			if generated != "" {
				c.Manual = "generated: " + generated
			} else if extra != "" && !strings.HasPrefix(extra, "on update ") {
				c.Manual = "opsi kolom " + extra
			}
			t.Columns = append(t.Columns, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
		rows, err = tx.QueryContext(ctx, `SELECT CONSTRAINT_NAME,CONSTRAINT_TYPE FROM information_schema.TABLE_CONSTRAINTS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY CONSTRAINT_NAME`, schema, t.Name)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var c Constraint
			if err := rows.Scan(&c.Name, &c.Kind); err != nil {
				rows.Close()
				return out, err
			}
			t.Constraints = append(t.Constraints, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
		for j := range t.Constraints {
			c := &t.Constraints[j]
			if c.Kind == "CHECK" {
				if err := tx.QueryRowContext(ctx, `SELECT CHECK_CLAUSE FROM information_schema.CHECK_CONSTRAINTS WHERE CONSTRAINT_SCHEMA=? AND TABLE_NAME=? AND CONSTRAINT_NAME=?`, schema, t.Name, c.Name).Scan(&c.Check); err != nil {
					return out, err
				}
				continue
			}
			rows, err = tx.QueryContext(ctx, `SELECT COLUMN_NAME,COALESCE(REFERENCED_TABLE_SCHEMA,''),COALESCE(REFERENCED_TABLE_NAME,''),COALESCE(REFERENCED_COLUMN_NAME,'')
				FROM information_schema.KEY_COLUMN_USAGE WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND CONSTRAINT_NAME=? ORDER BY ORDINAL_POSITION`, schema, t.Name, c.Name)
			if err != nil {
				return out, err
			}
			for rows.Next() {
				var col, ref string
				if err := rows.Scan(&col, &c.RefSchema, &c.RefTable, &ref); err != nil {
					rows.Close()
					return out, err
				}
				c.Columns = append(c.Columns, col)
				if ref != "" {
					c.RefColumns = append(c.RefColumns, ref)
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return out, err
			}
			if c.RefSchema == schema {
				c.RefSchema = ""
			}
			if c.Kind == "FOREIGN KEY" {
				if err := tx.QueryRowContext(ctx, `SELECT UPDATE_RULE,DELETE_RULE FROM information_schema.REFERENTIAL_CONSTRAINTS WHERE CONSTRAINT_SCHEMA=? AND TABLE_NAME=? AND CONSTRAINT_NAME=?`, schema, t.Name, c.Name).Scan(&c.Update, &c.Delete); err != nil {
					return out, err
				}
			}
		}
		rows, err = tx.QueryContext(ctx, `SELECT INDEX_NAME,NON_UNIQUE,INDEX_TYPE,COALESCE(COLUMN_NAME,''),SUB_PART,COLLATION,IGNORED
			FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY INDEX_NAME,SEQ_IN_INDEX`, schema, t.Name)
		if err != nil {
			return out, err
		}
		var indexes []Index
		for rows.Next() {
			var name, method, col, ignored string
			var nonUnique int
			var prefix sql.NullInt64
			var order sql.NullString
			if err := rows.Scan(&name, &nonUnique, &method, &col, &prefix, &order, &ignored); err != nil {
				rows.Close()
				return out, err
			}
			if method != "BTREE" || col == "" || ignored == "YES" {
				t.Manual = append(t.Manual, fmt.Sprintf("index khusus %s (%s,%s,%s)", name, method, col, ignored))
				continue
			}
			if len(indexes) == 0 || indexes[len(indexes)-1].Name != name {
				indexes = append(indexes, Index{Name: name, Method: "btree", Unique: nonUnique == 0})
			}
			ix := &indexes[len(indexes)-1]
			ix.Keys = append(ix.Keys, IndexKey{Name: col, Prefix: prefix.Int64, Desc: order.String == "D"})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
		for _, ix := range indexes {
			owned := false
			for _, c := range t.Constraints {
				if (c.Kind == "PRIMARY KEY" || c.Kind == "UNIQUE") && c.Name == ix.Name {
					owned = true
					for _, key := range ix.Keys {
						if key.Prefix != 0 || key.Desc {
							t.Manual = append(t.Manual, "constraint dengan key khusus "+c.Name)
						}
					}
				}
			}
			if !owned {
				t.Indexes = append(t.Indexes, ix)
			}
		}
		rows, err = tx.QueryContext(ctx, `SELECT TRIGGER_NAME,ACTION_STATEMENT FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA=? AND EVENT_OBJECT_TABLE=? ORDER BY TRIGGER_NAME`, schema, t.Name)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var name, body string
			if err := rows.Scan(&name, &body); err != nil {
				rows.Close()
				return out, err
			}
			t.Manual = append(t.Manual, "trigger "+name+": "+body)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT DISTINCT CONCAT(TABLE_SCHEMA,'.',TABLE_NAME,'.',CONSTRAINT_NAME) FROM information_schema.KEY_COLUMN_USAGE
		WHERE REFERENCED_TABLE_SCHEMA=? AND TABLE_SCHEMA<>? ORDER BY 1`, schema, schema)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return out, err
		}
		out.Incoming = append(out.Incoming, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA=? ORDER BY TABLE_NAME`, schema)
	if err != nil {
		return out, err
	}
	var final []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return out, err
		}
		final = append(final, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	var initial []string
	for _, t := range out.Tables {
		initial = append(initial, t.Name)
	}
	if !same(initial, final) {
		return out, fmt.Errorf("schema berubah ketika snapshot dibaca; ulangi perbandingan")
	}
	return out, nil
}
