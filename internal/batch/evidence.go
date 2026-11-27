package batch

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (r *Runner) proof(ctx context.Context, q queryer, sp Spec) ([]byte, error) {
	var state string
	err := q.QueryRow(ctx, `SELECT jsonb_build_array(
  (SELECT jsonb_build_array(scope_max,total_rows,finished_at IS NOT NULL)
   FROM _platform.migration_runs WHERE root_job=$1 AND op=$2),
  (SELECT jsonb_agg(jsonb_build_array(batch_no,cursor_from,cursor_to,rows_affected,row_manifest) ORDER BY batch_no)
   FROM _platform.migration_checkpoints WHERE root_job=$1 AND op=$2))::text`, sp.RootID, sp.Op).Scan(&state)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, r.EvidenceKey)
	mac.Write([]byte(strings.Join([]string{"pg-v1", sp.Binding, strconv.FormatInt(sp.RootID, 10), sp.Op, state}, "\x00")))
	return mac.Sum(nil), nil
}

func (r *Runner) seal(ctx context.Context, q queryer, sp Spec) error {
	if len(r.EvidenceKey) == 0 {
		return nil
	}
	proof, err := r.proof(ctx, q, sp)
	if err != nil {
		return err
	}
	_, err = q.Exec(ctx, `INSERT INTO _platform.migration_seals(root_job,op,proof) VALUES($1,$2,$3)
 ON CONFLICT(root_job,op) DO UPDATE SET proof=excluded.proof`, sp.RootID, sp.Op, proof)
	return err
}

func (r *Runner) verify(ctx context.Context, q queryer, sp Spec, allowNew bool) error {
	if len(r.EvidenceKey) == 0 {
		return nil
	}
	if err := trustedControl(ctx, q); err != nil {
		return err
	}
	var existing []byte
	err := q.QueryRow(ctx, `SELECT proof FROM _platform.migration_seals WHERE root_job=$1 AND op=$2`, sp.RootID, sp.Op).Scan(&existing)
	if err == pgx.ErrNoRows && allowNew {
		var exists bool
		if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM _platform.migration_runs WHERE root_job=$1 AND op=$2)
   OR EXISTS(SELECT 1 FROM _platform.migration_checkpoints WHERE root_job=$1 AND op=$2)`, sp.RootID, sp.Op).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return r.seal(ctx, q, sp)
		}
	}
	if err != nil {
		if err == pgx.ErrNoRows || IsDefinite(err) {
			return fmt.Errorf("%w: bukti autentik tidak tersedia", ErrCommitUnknown)
		}
		return err
	}
	expected, err := r.proof(ctx, q, sp)
	if err != nil {
		return err
	}
	if !hmac.Equal(expected, existing) {
		return fmt.Errorf("%w: bukti target tidak autentik", ErrCommitUnknown)
	}
	return nil
}

func trustedControl(ctx context.Context, q queryer) error {
	var count int
	err := q.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_class c
 JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='_platform'
 AND c.relname IN ('migration_runs','migration_checkpoints','migration_seals')
 AND c.relkind='r' AND c.relpersistence='p'
 AND NOT (c.relrowsecurity OR c.relhasrules OR c.relhastriggers OR c.relhassubclass)`).Scan(&count)
	if err != nil {
		return err
	}
	if count != 3 {
		return ErrCommitUnknown
	}
	return nil
}

func (r *Runner) controlState(ctx context.Context, q queryer) (string, error) {
	if len(r.EvidenceKey) == 0 {
		return "", nil
	}
	if err := trustedControl(ctx, q); err != nil {
		return "", err
	}
	var state string
	err := q.QueryRow(ctx, `SELECT encode(sha256(convert_to(jsonb_build_array(
  (SELECT jsonb_agg(to_jsonb(r) ORDER BY root_job,op) FROM _platform.migration_runs r),
  (SELECT jsonb_agg(to_jsonb(c) ORDER BY root_job,op,batch_no) FROM _platform.migration_checkpoints c),
  (SELECT jsonb_agg(to_jsonb(s) ORDER BY root_job,op) FROM _platform.migration_seals s),
  (SELECT jsonb_agg(jsonb_build_array(c.oid,c.relname,c.relkind,c.relowner,c.relacl,c.relrowsecurity,c.relforcerowsecurity,c.relhasrules,c.relhastriggers) ORDER BY c.oid)
   FROM pg_class c WHERE c.relnamespace='_platform'::regnamespace)
 )::text,'UTF8')),'hex')`).Scan(&state)
	return state, err
}

func (r *Runner) checkControl(ctx context.Context, q queryer, before string) error {
	after, err := r.controlState(ctx, q)
	if err != nil {
		return err
	}
	if before != after {
		return ErrUnsafe
	}
	return nil
}

func (r *Runner) record(ctx context.Context, s *session, sp Spec, sql string, args ...any) error {
	return pgx.BeginFunc(ctx, s.conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			return err
		}
		return r.seal(ctx, tx, sp)
	})
}
