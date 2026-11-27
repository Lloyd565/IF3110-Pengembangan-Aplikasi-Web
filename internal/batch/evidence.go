package batch

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

const proofV2 byte = 2

func (r *Runner) proof(ctx context.Context, q queryer, sp Spec) ([]byte, error) {
	mac := hmac.New(sha256.New, r.EvidenceKey)
	io.WriteString(mac, strings.Join([]string{"pg-v2", sp.Binding, strconv.FormatInt(sp.RootID, 10), sp.Op, ""}, "\x00"))
	if err := streamState(ctx, q, mac, `
SELECT digest FROM (
  SELECT 0 AS section, 0 AS seq, sha256(convert_to(coalesce((
    SELECT jsonb_build_array(scope_max,total_rows,finished_at IS NOT NULL)::text
    FROM _platform.migration_runs WHERE root_job=$1 AND op=$2), 'null'), 'UTF8')) AS digest
  UNION ALL
  SELECT 1, batch_no, sha256(convert_to(jsonb_build_array(batch_no,cursor_from,cursor_to,rows_affected,row_manifest)::text, 'UTF8'))
    FROM _platform.migration_checkpoints WHERE root_job=$1 AND op=$2
) states ORDER BY section, seq`, sp.RootID, sp.Op); err != nil {
		return nil, err
	}
	return mac.Sum([]byte{proofV2}), nil
}

// legacyProof reproduces the original PostgreSQL jsonb text without materializing all checkpoints.
func (r *Runner) legacyProof(ctx context.Context, q queryer, sp Spec) ([]byte, error) {
	mac := hmac.New(sha256.New, r.EvidenceKey)
	io.WriteString(mac, strings.Join([]string{"pg-v1", sp.Binding, strconv.FormatInt(sp.RootID, 10), sp.Op, ""}, "\x00"))
	var run string
	if err := q.QueryRow(ctx, `SELECT coalesce((SELECT jsonb_build_array(scope_max,total_rows,finished_at IS NOT NULL)::text
    FROM _platform.migration_runs WHERE root_job=$1 AND op=$2), 'null')`, sp.RootID, sp.Op).Scan(&run); err != nil {
		return nil, err
	}
	io.WriteString(mac, "["+run+", ")
	rows, err := q.Query(ctx, `SELECT jsonb_build_array(batch_no,cursor_from,cursor_to,rows_affected,row_manifest)::text
    FROM _platform.migration_checkpoints WHERE root_job=$1 AND op=$2 ORDER BY batch_no`, sp.RootID, sp.Op)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	first := true
	for rows.Next() {
		var checkpoint []byte
		if err := rows.Scan(&checkpoint); err != nil {
			return nil, err
		}
		if first {
			io.WriteString(mac, "[")
			first = false
		} else {
			io.WriteString(mac, ", ")
		}
		mac.Write(checkpoint)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if first {
		io.WriteString(mac, "null")
	} else {
		io.WriteString(mac, "]")
	}
	io.WriteString(mac, "]")
	return mac.Sum(nil), nil
}

func streamState(ctx context.Context, q queryer, dst io.Writer, sql string, args ...any) error {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var digest []byte
		if err := rows.Scan(&digest); err != nil {
			return err
		}
		if _, err := dst.Write(digest); err != nil {
			return err
		}
	}
	return rows.Err()
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
	var expected []byte
	switch {
	case len(existing) == sha256.Size:
		expected, err = r.legacyProof(ctx, q, sp)
	case len(existing) == sha256.Size+1 && existing[0] == proofV2:
		expected, err = r.proof(ctx, q, sp)
	default:
		return fmt.Errorf("%w: format bukti target tidak dikenal", ErrCommitUnknown)
	}
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
	hash := sha256.New()
	err := streamState(ctx, q, hash, `
SELECT digest FROM (
  SELECT 0 AS section, root_job, op, 0 AS seq, sha256(convert_to(to_jsonb(r)::text,'UTF8')) AS digest FROM _platform.migration_runs r
  UNION ALL
  SELECT 1, root_job, op, batch_no, sha256(convert_to(to_jsonb(c)::text,'UTF8')) FROM _platform.migration_checkpoints c
  UNION ALL
  SELECT 2, root_job, op, 0, sha256(convert_to(to_jsonb(s)::text,'UTF8')) FROM _platform.migration_seals s
  UNION ALL
  SELECT 3, c.oid::bigint, '', 0, sha256(convert_to(jsonb_build_array(c.oid,c.relname,c.relkind,c.relowner,c.relacl,
     c.relrowsecurity,c.relforcerowsecurity,c.relhasrules,c.relhastriggers)::text,'UTF8'))
  FROM pg_catalog.pg_class c WHERE c.relnamespace='_platform'::regnamespace
) states ORDER BY section, root_job, op, seq`)
	return string(hash.Sum(nil)), err
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
