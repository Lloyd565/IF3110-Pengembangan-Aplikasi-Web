package schemadiff

import (
	"context"
	"fmt"
	"time"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/jackc/pgx/v5"
)

func readPostgres(ctx context.Context, target *dbconn.Target, schema string) (Snapshot, error) {
	out := Snapshot{DBMS: dbconn.PostgreSQL, Schema: schema}
	tx, err := target.Postgres.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		tx.Rollback(cleanup)
	}()
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout='8s'"); err != nil {
		return out, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, schema).Scan(&exists); err != nil {
		return out, err
	}
	if !exists {
		return out, ErrNotFound
	}
	rows, err := tx.Query(ctx, `SELECT c.oid,c.relname,c.relkind::text,c.relpersistence::text,c.relrowsecurity,c.relforcerowsecurity,
		EXISTS(SELECT 1 FROM pg_inherits WHERE inhrelid=c.oid OR inhparent=c.oid),
		COALESCE(array_to_string(c.reloptions,','),''),c.reltablespace<>0,
		CASE WHEN c.relkind IN ('v','m') THEN pg_get_viewdef(c.oid,false) ELSE '' END
		FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE n.nspname=$1 AND c.relkind IN ('r','p','v','m','f') ORDER BY c.relname`, schema)
	if err != nil {
		return out, err
	}
	type relation struct {
		oid   uint32
		table Table
		kind  string
	}
	var relations []relation
	for rows.Next() {
		var r relation
		var kind, persistence, options, definition string
		var rls, force, inherits, tablespace bool
		if err := rows.Scan(&r.oid, &r.table.Name, &kind, &persistence, &rls, &force, &inherits, &options, &tablespace, &definition); err != nil {
			rows.Close()
			return out, err
		}
		if kind != "r" {
			r.table.Manual = append(r.table.Manual, "jenis objek "+kind+": "+definition)
		}
		r.kind = kind
		if persistence != "p" || rls || force || inherits || options != "" || tablespace {
			r.table.Manual = append(r.table.Manual, fmt.Sprintf("opsi tabel khusus: persistence=%s RLS=%t/%t inheritance=%t options=%s tablespace=%t", persistence, rls, force, inherits, options, tablespace))
		}
		relations = append(relations, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	for _, r := range relations {
		t := r.table
		if r.kind != "r" {
			out.Tables = append(out.Tables, t)
			continue
		}
		rows, err = tx.Query(ctx, `SELECT a.attname,format_type(a.atttypid,a.atttypmod),NOT a.attnotnull,COALESCE(pg_get_expr(d.adbin,d.adrelid),''),
			a.attidentity::text,a.attgenerated::text,pg_get_serial_sequence(format('%I.%I',$2::text,$3::text),a.attname) IS NOT NULL,
			CASE WHEN a.attcollation<>typ.typcollation THEN format('%I.%I',cn.nspname,coll.collname) ELSE '' END,
			typ.typtype::text,COALESCE(elem.typtype::text,''),
			COALESCE(seq.seqstart,1),COALESCE(seq.seqincrement,1),COALESCE(seq.seqcache,1),COALESCE(seq.seqcycle,false),COALESCE(seq.seqmin,1),COALESCE(seq.seqmax,0),
			typ.typnamespace<>'pg_catalog'::regnamespace OR COALESCE(elem.typnamespace<>'pg_catalog'::regnamespace,false),
			EXISTS(SELECT 1 FROM pg_depend dep JOIN pg_proc proc ON proc.oid=dep.refobjid
				WHERE dep.classid='pg_attrdef'::regclass AND dep.objid=d.oid AND dep.refclassid='pg_proc'::regclass AND proc.pronamespace<>'pg_catalog'::regnamespace)
			FROM pg_attribute a JOIN pg_type typ ON typ.oid=a.atttypid LEFT JOIN pg_type elem ON elem.oid=typ.typelem
			LEFT JOIN pg_attrdef d ON d.adrelid=a.attrelid AND d.adnum=a.attnum
			LEFT JOIN pg_collation coll ON coll.oid=a.attcollation LEFT JOIN pg_namespace cn ON cn.oid=coll.collnamespace
			LEFT JOIN pg_sequence seq ON seq.seqrelid=to_regclass(pg_get_serial_sequence(format('%I.%I',$2::text,$3::text),a.attname))
			WHERE a.attrelid=$1 AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`, r.oid, schema, t.Name)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var c Column
			var identity, generated, typeKind, elementKind string
			var serial, cycle bool
			var customType, customDefault bool
			var start, increment, cache, minValue, maxValue int64
			if err := rows.Scan(&c.Name, &c.Type, &c.Nullable, &c.Default, &identity, &generated, &serial, &c.Collation, &typeKind, &elementKind, &start, &increment, &cache, &cycle, &minValue, &maxValue, &customType, &customDefault); err != nil {
				rows.Close()
				return out, err
			}
			switch identity {
			case "a":
				c.Auto = "always"
			case "d":
				c.Auto = "default"
			default:
				if serial {
					c.Auto = "serial"
				}
			}
			if c.Auto != "" {
				c.Default = ""
				expectedMax := map[string]int64{"smallint": 32767, "integer": 2147483647, "bigint": 9223372036854775807}[c.Type]
				if start != 1 || increment != 1 || cache != 1 || cycle || minValue != 1 || maxValue != expectedMax {
					c.Manual = fmt.Sprintf("opsi sequence khusus (%d,%d,%d,%t,%d,%d)", start, increment, cache, cycle, minValue, maxValue)
				}
			}
			if generated != "" {
				c.Extra = generated + ":" + c.Default
				c.Manual = "kolom generated"
			}
			if customType || typeKind != "b" || (elementKind != "" && elementKind != "b") {
				c.Manual = "tipe/domain/enum khusus"
			}
			if customDefault {
				c.Manual = "default bergantung fungsi non-builtin"
			}
			if c.Collation != "" {
				c.Manual = "collation kolom khusus"
			}
			t.Columns = append(t.Columns, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
		rows, err = tx.Query(ctx, `SELECT con.conname,con.contype::text,
			ARRAY(SELECT a.attname::text FROM unnest(con.conkey) WITH ORDINALITY k(num,pos) JOIN pg_attribute a ON a.attrelid=con.conrelid AND a.attnum=k.num ORDER BY k.pos),
			COALESCE(rn.nspname,''),COALESCE(rc.relname,''),
			ARRAY(SELECT a.attname::text FROM unnest(con.confkey) WITH ORDINALITY k(num,pos) JOIN pg_attribute a ON a.attrelid=con.confrelid AND a.attnum=k.num ORDER BY k.pos),
			con.confupdtype::text,con.confdeltype::text,con.confmatchtype::text,
			CASE WHEN con.contype='c' THEN pg_get_expr(con.conbin,con.conrelid,false) ELSE '' END,
			con.condeferrable,con.condeferred,NOT con.convalidated,COALESCE(ix.indnullsnotdistinct,false),con.connoinherit,
			ARRAY(SELECT a.attname::text FROM unnest(con.confdelsetcols) WITH ORDINALITY k(num,pos) JOIN pg_attribute a ON a.attrelid=con.conrelid AND a.attnum=k.num ORDER BY k.pos)
			FROM pg_constraint con LEFT JOIN pg_class rc ON rc.oid=con.confrelid LEFT JOIN pg_namespace rn ON rn.oid=rc.relnamespace
			LEFT JOIN pg_index ix ON ix.indexrelid=con.conindid WHERE con.conrelid=$1 ORDER BY con.conname`, r.oid)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var c Constraint
			var kind, update, delete, match string
			if err := rows.Scan(&c.Name, &kind, &c.Columns, &c.RefSchema, &c.RefTable, &c.RefColumns, &update, &delete, &match, &c.Check, &c.Deferrable, &c.Deferred, &c.NotValid, &c.NullsNotDistinct, &c.NoInherit, &c.DeleteColumns); err != nil {
				rows.Close()
				return out, err
			}
			// PostgreSQL 18 catalogs NOT NULL separately; nullability is already
			// captured on the column, which also works on PostgreSQL 16/17.
			if kind == "n" {
				if c.NotValid {
					t.Manual = append(t.Manual, "NOT NULL NOT VALID "+c.Name)
				}
				continue
			}
			c.Kind = map[string]string{"p": "PRIMARY KEY", "u": "UNIQUE", "f": "FOREIGN KEY", "c": "CHECK"}[kind]
			if c.Kind == "" {
				t.Manual = append(t.Manual, "constraint khusus "+c.Name+" "+kind)
				continue
			}
			if c.Kind != "UNIQUE" && c.Kind != "PRIMARY KEY" {
				c.NullsNotDistinct = false
			}
			if c.RefSchema == schema {
				c.RefSchema = ""
			}
			c.Update = pgAction(update)
			c.Delete = pgAction(delete)
			if match == "f" {
				c.Match = "FULL"
			}
			t.Constraints = append(t.Constraints, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
		rows, err = tx.Query(ctx, `SELECT ic.relname,am.amname,i.indisunique,i.indnullsnotdistinct,i.indisvalid,i.indexprs IS NOT NULL,COALESCE(pg_get_expr(i.indpred,i.indrelid,false),''),
			ARRAY(SELECT a.attname::text FROM unnest(i.indkey) WITH ORDINALITY k(num,pos) JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.num WHERE k.pos<=i.indnkeyatts ORDER BY k.pos),
			ARRAY(SELECT opt::int FROM unnest(i.indoption) opt),
			ARRAY(SELECT a.attname::text FROM unnest(i.indkey) WITH ORDINALITY k(num,pos) JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.num WHERE k.pos>i.indnkeyatts ORDER BY k.pos),
			COALESCE(pg_get_expr(i.indexprs,i.indrelid,false),''),
			EXISTS(SELECT 1 FROM unnest(i.indclass) op JOIN pg_opclass opc ON opc.oid=op WHERE NOT opc.opcdefault),
			EXISTS(SELECT 1 FROM generate_series(0,i.indnkeyatts-1) pos JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=i.indkey[pos] WHERE i.indcollation[pos]<>a.attcollation),
			COALESCE(array_to_string(ic.reloptions,','),''),ic.reltablespace<>0,i.indisclustered,
			ARRAY(SELECT opc.opcname::text FROM unnest(i.indclass) WITH ORDINALITY op(id,pos) JOIN pg_opclass opc ON opc.oid=op.id ORDER BY op.pos)
			FROM pg_index i JOIN pg_class ic ON ic.oid=i.indexrelid JOIN pg_am am ON am.oid=ic.relam
			WHERE i.indrelid=$1 AND NOT EXISTS(SELECT 1 FROM pg_constraint con WHERE con.conindid=i.indexrelid AND con.contype IN ('p','u','x')) ORDER BY ic.relname`, r.oid)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var ix Index
			var valid, expression bool
			var predicate string
			var expressionSQL, storage string
			var customOp, customCollation, tablespace, clustered bool
			var opNames []string
			var columns []string
			var options []int
			if err := rows.Scan(&ix.Name, &ix.Method, &ix.Unique, &ix.NullsNotDistinct, &valid, &expression, &predicate, &columns, &options, &ix.Include, &expressionSQL, &customOp, &customCollation, &storage, &tablespace, &clustered, &opNames); err != nil {
				rows.Close()
				return out, err
			}
			if !valid || expression || predicate != "" || ix.Method != "btree" || customOp || customCollation || storage != "" || tablespace || clustered {
				t.Manual = append(t.Manual, fmt.Sprintf("index khusus %s (valid=%t expression=%s predicate=%s method=%s opclass=%v collation=%t storage=%s tablespace=%t clustered=%t)", ix.Name, valid, expressionSQL, predicate, ix.Method, opNames, customCollation, storage, tablespace, clustered))
				continue
			}
			for i, name := range columns {
				opt := 0
				if i < len(options) {
					opt = options[i]
				}
				ix.Keys = append(ix.Keys, IndexKey{Name: name, Desc: opt&1 != 0, NullsFirst: opt&2 != 0})
			}
			t.Indexes = append(t.Indexes, ix)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
		rows, err = tx.Query(ctx, `SELECT pg_get_triggerdef(oid,false) FROM pg_trigger WHERE tgrelid=$1 AND NOT tgisinternal ORDER BY tgname`, r.oid)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var trigger string
			if err := rows.Scan(&trigger); err != nil {
				rows.Close()
				return out, err
			}
			t.Manual = append(t.Manual, "trigger: "+trigger)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
		out.Tables = append(out.Tables, t)
	}
	rows, err = tx.Query(ctx, `SELECT format('%I.%I.%I',sn.nspname,src.relname,con.conname) FROM pg_constraint con
		JOIN pg_class src ON src.oid=con.conrelid JOIN pg_namespace sn ON sn.oid=src.relnamespace
		JOIN pg_class dst ON dst.oid=con.confrelid JOIN pg_namespace dn ON dn.oid=dst.relnamespace
		WHERE con.contype='f' AND dn.nspname=$1 AND sn.nspname<>$1 ORDER BY 1`, schema)
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
	return out, err
}

func pgAction(code string) string {
	return map[string]string{"a": "NO ACTION", "r": "RESTRICT", "c": "CASCADE", "n": "SET NULL", "d": "SET DEFAULT"}[code]
}
