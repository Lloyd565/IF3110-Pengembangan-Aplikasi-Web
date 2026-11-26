package batch

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type rowVersion struct {
	ID   int64
	XMin string
	CTID string
}

type rowManifest struct {
	Relation uint32
	Rows     []rowVersion
}

func captureRows(ctx context.Context, tx pgx.Tx, sql, tbl string, from, to int64, limit int) (rowManifest, error) {
	m := rowManifest{Rows: []rowVersion{}}
	if err := tx.QueryRow(ctx, "SELECT $1::regclass::oid", tbl).Scan(&m.Relation); err != nil {
		return m, err
	}
	rows, err := tx.Query(ctx, sql, from, to)
	if err != nil {
		return m, err
	}
	defer rows.Close()
	for rows.Next() {
		var v rowVersion
		if len(rows.FieldDescriptions()) < 3 {
			return m, ErrUnsafe
		}
		args := make([]any, len(rows.FieldDescriptions()))
		args[0], args[1], args[2] = &v.ID, &v.XMin, &v.CTID
		if err := rows.Scan(args...); err != nil {
			return m, err
		}
		m.Rows = append(m.Rows, v)
		if len(m.Rows) > limit {
			return m, ErrInvalidSpec
		}
	}
	return m, rows.Err()
}

func matchingRows(ctx context.Context, tx pgx.Tx, sp Spec, tbl string, m *rowManifest) ([]int64, error) {
	if m == nil || m.Rows == nil || len(m.Rows) > sp.Size {
		return nil, ErrUnsafe
	}
	var oid uint32
	if err := tx.QueryRow(ctx, "SELECT $1::regclass::oid", tbl).Scan(&oid); err != nil {
		return nil, err
	}
	if oid != m.Relation {
		return nil, ErrUnsafe
	}
	ids := make([]int64, len(m.Rows))
	versions := make(map[int64]rowVersion, len(ids))
	for i, v := range m.Rows {
		ids[i] = v.ID
		versions[v.ID] = v
	}
	col := pgx.Identifier{sp.Column}.Sanitize()
	rows, err := tx.Query(ctx, "SELECT "+col+",xmin::text,ctid::text FROM "+tbl+" WHERE "+col+" = ANY($1::bigint[])", ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var actual rowVersion
		if err := rows.Scan(&actual.ID, &actual.XMin, &actual.CTID); err != nil {
			return nil, err
		}
		if expected, ok := versions[actual.ID]; !ok || expected != actual {
			return nil, ErrUnsafe
		}
		delete(versions, actual.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(versions) != 0 {
		return nil, ErrUnsafe
	}
	return ids, nil
}
