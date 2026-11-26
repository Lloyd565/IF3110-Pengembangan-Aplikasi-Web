package batch

import (
	"context"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"
)

func validateCursor(ctx context.Context, q queryer, sp Spec) error {
	tbl, err := table(sp.Table)
	if err != nil {
		return err
	}
	var ok bool
	err = q.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_class c JOIN pg_attribute a ON a.attrelid = c.oid
		WHERE c.oid = to_regclass($1) AND c.relkind = 'r' AND NOT c.relhassubclass
		AND a.attname = $2 AND NOT a.attisdropped AND a.attnotnull
		AND a.atttypid IN ('int2'::regtype,'int4'::regtype,'int8'::regtype)
		AND EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid = c.oid AND i.indisunique
		AND i.indisvalid AND i.indisready AND i.indimmediate AND i.indnkeyatts = 1
		AND i.indkey[0] = a.attnum AND i.indpred IS NULL AND i.indexprs IS NULL)
		AND NOT EXISTS (SELECT 1 FROM pg_trigger t WHERE t.tgrelid = c.oid AND NOT t.tgisinternal AND t.tgenabled <> 'D')
	)`, tbl, sp.Column).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: cursor harus integer, NOT NULL, unik tanpa index parsial, pada tabel biasa tanpa inheritance atau trigger pengguna", ErrInvalidSpec)
	}
	var minID *int64
	if err := q.QueryRow(ctx, `SELECT min(`+pgx.Identifier{sp.Column}.Sanitize()+`) FROM `+tbl).Scan(&minID); err != nil {
		return err
	}
	if minID != nil && *minID == math.MinInt64 {
		return fmt.Errorf("%w: cursor minimum bigint tidak dapat memakai batas bawah eksklusif", ErrInvalidSpec)
	}
	return nil
}

func (r *Runner) Validate(ctx context.Context, sp Spec) error {
	if err := validate(sp); err != nil {
		return err
	}
	if sp.Size > 0 {
		return validateCursor(ctx, r.Pool, sp)
	}
	return nil
}
