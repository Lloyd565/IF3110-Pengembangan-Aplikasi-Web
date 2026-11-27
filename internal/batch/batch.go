package batch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/sqlpolicy"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/telemetry"
)

const lockKey = `hashtext('if3110.migration')`

var (
	ErrBusy          = errors.New("batch: migration lain sedang berjalan di database ini")
	ErrCommitUnknown = errors.New("batch: hasil commit belum pasti dan bukti di target tidak bisa dibaca")
	ErrRolledBack    = errors.New("batch: commit tidak terjadi di target")
	ErrUnsafe        = errors.New("batch: pembatalan tidak aman")
	ErrInvalidSpec   = errors.New("batch: spesifikasi tidak valid")
)

var ident = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const (
	OpUp   = "up"
	OpDown = "down"
)

type Hooks struct {
	BeforeCommit func(op string, batchNo int) error
	AfterCommit  func(op string, batchNo int) error
}

type Metrics interface {
	LockWait(time.Duration)
	BatchCommitted(rows int64)
}

type Runner struct {
	Pool             *pgxpool.Pool
	LockWait         time.Duration
	StatementTimeout time.Duration
	Hooks            Hooks
	Metrics          Metrics
	ReconcileWait    time.Duration
	Revalidate       func(context.Context) error
	Arm              func(context.Context) error
	RecordSQL        func(context.Context, string) error
	EvidenceKey      []byte
	TargetServer     string
}

func (r *Runner) reconcileWait() time.Duration {
	if r.ReconcileWait > 0 {
		return r.ReconcileWait
	}
	return 30 * time.Second
}

type Spec struct {
	RootID  int64
	Binding string
	Op      string
	SQL     string

	Size   int
	Table  string
	Column string
}

type Progress struct {
	Batches, Rows int64
	Total         *int64
}

func (r *Runner) wait() time.Duration {
	if r.LockWait > 0 {
		return r.LockWait
	}
	return 5 * time.Second
}

func (r *Runner) stmtTimeout() time.Duration {
	if r.StatementTimeout > 0 {
		return r.StatementTimeout
	}
	return 10 * time.Minute
}

func IsDefinite(err error) bool { return errors.As(err, new(*pgconn.PgError)) }

func IsTransient(err error) bool {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "40001", "40P01", "55P03", "53300", "57P01", "57P03":
			return true
		}
		return false
	}
	return errors.Is(err, ErrBusy) || errors.Is(err, ErrRolledBack)
}

type session struct{ conn *pgxpool.Conn }

func (r *Runner) hold(ctx context.Context) (*session, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "lock.acquire")
	defer span.End()
	started := time.Now()
	conn, err := r.Pool.Acquire(ctx)
	if err != nil {
		span.RecordError(errors.New(telemetry.RedactError(err)))
		return nil, err
	}
	if r.TargetServer != "" {
		actual, err := dbconn.PostgresIdentity(ctx, conn)
		if err != nil || actual != r.TargetServer {
			conn.Release()
			if err != nil {
				return nil, err
			}
			return nil, dbconn.ErrTargetIdentity
		}
	}
	deadline := started.Add(r.wait())
	for {
		var ok bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+lockKey+`)`).Scan(&ok); err != nil {
			conn.Release()
			span.RecordError(errors.New(telemetry.RedactError(err)))
			return nil, err
		}
		if ok {
			waited := time.Since(started)
			if r.Metrics != nil {
				r.Metrics.LockWait(waited)
			}
			telemetry.Event(ctx, slog.LevelInfo, "lock.acquired", "ok", slog.Float64("wait_ms", float64(waited.Microseconds())/1000))
			return &session{conn}, nil
		}
		if time.Now().After(deadline) {
			conn.Release()
			telemetry.Event(ctx, slog.LevelWarn, "lock.timeout", "busy")
			span.RecordError(ErrBusy)
			return nil, ErrBusy
		}
		select {
		case <-ctx.Done():
			conn.Release()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (s *session) release() {
	if s.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var ok bool
	if err := s.conn.QueryRow(ctx, `SELECT pg_advisory_unlock(`+lockKey+`)`).Scan(&ok); err != nil || !ok {

		s.conn.Conn().Close(ctx)
	}
	s.conn.Release()
	s.conn = nil
}

func validate(sp Spec) error {
	if sp.RootID <= 0 || (sp.Op != OpUp && sp.Op != OpDown) || sp.SQL == "" {
		return fmt.Errorf("%w: root, op, dan sql wajib", ErrInvalidSpec)
	}
	if err := sqlpolicy.Check(sp.SQL); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidSpec, err)
	}
	if sp.Size == 0 {
		return nil
	}
	if sp.Size < 0 || sp.Size > 100000 || !ident.MatchString(sp.Column) {
		return fmt.Errorf("%w: ukuran batch atau kolom cursor", ErrInvalidSpec)
	}
	_, err := table(sp.Table)
	if err != nil {
		return err
	}
	if err := sqlpolicy.Batch(sp.SQL, sp.Table, sp.Column); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidSpec, err)
	}
	return nil
}

func Validate(sp Spec) error { return validate(sp) }

func table(name string) (string, error) {
	parts := strings.Split(name, ".")
	if len(parts) != 2 || !ident.MatchString(parts[0]) || !ident.MatchString(parts[1]) {
		return "", fmt.Errorf("%w: cakupan batch harus berbentuk schema.table", ErrInvalidSpec)
	}
	return pgx.Identifier(parts).Sanitize(), nil
}

type queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func ensureControl(ctx context.Context, q queryer) error {
	var runs, cps, manifest, seals bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('_platform.migration_runs') IS NOT NULL,
		to_regclass('_platform.migration_checkpoints') IS NOT NULL,
  EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid=to_regclass('_platform.migration_checkpoints') AND attname='manifest_digest' AND NOT attisdropped), to_regclass('_platform.migration_seals') IS NOT NULL`).Scan(&runs, &cps, &manifest, &seals); err != nil {
		return err
	}
	if runs && cps && manifest && seals {
		return nil
	}
	_, err := q.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS _platform;
		CREATE TABLE IF NOT EXISTS _platform.migration_runs (
			root_job    bigint      NOT NULL,
			op          text        NOT NULL,
			scope_max   bigint,
			total_rows  bigint,
			started_at  timestamptz NOT NULL DEFAULT now(),
			finished_at timestamptz,
			PRIMARY KEY (root_job, op));
		CREATE TABLE IF NOT EXISTS _platform.migration_checkpoints (
			root_job      bigint      NOT NULL,
			op            text        NOT NULL,
			batch_no      int         NOT NULL,
			cursor_from   bigint      NOT NULL,
			cursor_to     bigint      NOT NULL,
			rows_affected bigint      NOT NULL,
			committed_at  timestamptz NOT NULL DEFAULT now(),
			PRIMARY KEY (root_job, op, batch_no));
  ALTER TABLE _platform.migration_checkpoints ADD COLUMN IF NOT EXISTS row_manifest jsonb;
  ALTER TABLE _platform.migration_checkpoints ADD COLUMN IF NOT EXISTS manifest_digest bytea
    GENERATED ALWAYS AS (sha256(replace(row_manifest::text, chr(92), chr(92)||chr(92))::bytea)) STORED;
  CREATE TABLE IF NOT EXISTS _platform.migration_seals(root_job bigint NOT NULL,op text NOT NULL,proof bytea NOT NULL,PRIMARY KEY(root_job,op));
  REVOKE ALL ON SCHEMA _platform FROM PUBLIC;
  REVOKE ALL ON ALL TABLES IN SCHEMA _platform FROM PUBLIC`)
	return err
}

type run struct {
	exists, finished bool
	scopeMax, total  *int64
}

func loadRun(ctx context.Context, q queryer, sp Spec) (run, error) {
	var r run
	var fin *time.Time
	err := q.QueryRow(ctx, `SELECT scope_max, total_rows, finished_at FROM _platform.migration_runs
		WHERE root_job = $1 AND op = $2`, sp.RootID, sp.Op).Scan(&r.scopeMax, &r.total, &fin)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, nil
	}
	r.exists, r.finished = err == nil, fin != nil
	return r, err
}

func totals(ctx context.Context, q queryer, sp Spec, r run) (Progress, error) {
	p := Progress{Total: r.total}
	err := q.QueryRow(ctx, `SELECT count(*), coalesce(sum(rows_affected), 0) FROM _platform.migration_checkpoints
		WHERE root_job = $1 AND op = $2`, sp.RootID, sp.Op).Scan(&p.Batches, &p.Rows)
	if err == nil && sp.Size == 0 && r.finished && r.total != nil {
		p.Batches, p.Rows = 1, *r.total
	}
	if err == nil && r.finished {
		p.Total = &p.Rows
	}
	return p, err
}

func (r *Runner) Evidence(ctx context.Context, sp Spec) (Progress, bool, error) {
	s, err := r.hold(ctx)
	if err != nil {
		return Progress{}, false, err
	}
	defer s.release()
	if err := r.verify(ctx, s.conn, sp, false); err != nil {
		return Progress{}, false, err
	}
	var ok bool
	if err := s.conn.QueryRow(ctx, `SELECT to_regclass('_platform.migration_runs') IS NOT NULL`).Scan(&ok); err != nil || !ok {
		return Progress{}, false, err
	}
	rn, err := loadRun(ctx, s.conn, sp)
	if err != nil {
		return Progress{}, false, err
	}
	p, err := totals(ctx, s.conn, sp, rn)
	return p, rn.finished, err
}

func (r *Runner) Run(ctx context.Context, sp Spec, report func(Progress)) (progress Progress, runErr error) {
	if err := validate(sp); err != nil {
		return Progress{}, err
	}
	s, err := r.hold(ctx)
	if err != nil {
		return Progress{}, err
	}
	defer s.release()
	if r.Revalidate != nil {
		if err := r.Revalidate(ctx); err != nil {
			return Progress{}, err
		}
	}
	if err := ensureControl(ctx, s.conn); err != nil {
		return Progress{}, err
	}
	if err := r.verify(ctx, s.conn, sp, true); err != nil {
		return Progress{}, err
	}
	if r.Arm != nil {
		if err := r.Arm(ctx); err != nil {
			return Progress{}, err
		}
	}
	rn, err := loadRun(ctx, s.conn, sp)
	if err != nil {
		return Progress{}, err
	}
	defer func() {
		if runErr == nil || s.conn == nil {
			return
		}
		proofCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if err := r.verify(proofCtx, s.conn, sp, false); err != nil {
			return
		}
		evidence, err := loadRun(proofCtx, s.conn, sp)
		if err != nil {
			if !errors.Is(runErr, ErrRolledBack) && !IsDefinite(runErr) && !errors.Is(runErr, ErrInvalidSpec) {
				runErr = fmt.Errorf("%w: bukti target tidak dapat dibaca", ErrCommitUnknown)
			}
			return
		}
		p, err := totals(proofCtx, s.conn, sp, evidence)
		if err == nil && p.Batches > progress.Batches {
			progress = p
		}
	}()
	if rn.finished {
		return totals(ctx, s.conn, sp, rn)
	}
	if sp.Size == 0 {
		return r.runNormal(ctx, s, sp)
	}
	return r.runBatches(ctx, s, sp, rn, report)
}

func (r *Runner) begin(ctx context.Context, s *session) (pgx.Tx, error) {
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = %d; SET LOCAL lock_timeout = %d; SET LOCAL standard_conforming_strings = on`,
		r.stmtTimeout().Milliseconds(), r.wait().Milliseconds())); err != nil {
		tx.Rollback(context.WithoutCancel(ctx))
		return nil, err
	}
	return tx, nil
}

func (r *Runner) commit(ctx context.Context, s *session, tx pgx.Tx, sp Spec, batchNo int) error {
	if err := r.seal(ctx, tx, sp); err != nil {
		tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	if h := r.Hooks.BeforeCommit; h != nil {
		if err := h(sp.Op, batchNo); err != nil {
			tx.Rollback(context.WithoutCancel(ctx))
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		if IsDefinite(err) && !tx.Conn().IsClosed() {
			return err
		}
		if err := r.reconcile(ctx, sp, batchNo, err, s); err != nil {
			return err
		}
	}
	if h := r.Hooks.AfterCommit; h != nil {
		return h(sp.Op, batchNo)
	}
	return nil
}

func (r *Runner) reconcile(ctx context.Context, sp Spec, batchNo int, cause error, current ...*session) error {
	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.reconcileWait())
	defer cancel()
	var s *session
	if len(current) > 0 {
		s = current[0]
	} else {
		var err error
		s, err = r.hold(ectx)
		if err != nil {
			return ErrCommitUnknown
		}
		defer s.release()
	}
	if s.conn.Conn().IsClosed() {
		s.release()
		acquired, err := r.hold(ectx)
		if err != nil {
			return ErrCommitUnknown
		}
		s.conn = acquired.conn
	}
	if err := r.verify(ectx, s.conn, sp, false); err != nil {
		return ErrCommitUnknown
	}
	var committed bool
	if sp.Size == 0 {
		rn, err := loadRun(ectx, s.conn, sp)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrCommitUnknown, cause)
		}
		committed = rn.finished
	} else if err := s.conn.QueryRow(ectx, `SELECT EXISTS (SELECT 1 FROM _platform.migration_checkpoints
		WHERE root_job = $1 AND op = $2 AND batch_no = $3)`, sp.RootID, sp.Op, batchNo).Scan(&committed); err != nil {
		return fmt.Errorf("%w: %v", ErrCommitUnknown, cause)
	}
	if committed {
		return nil
	}
	return fmt.Errorf("%w: %v", ErrRolledBack, cause)
}

func (r *Runner) runNormal(ctx context.Context, s *session, sp Spec) (Progress, error) {
	tx, err := r.begin(ctx, s)
	if err != nil {
		return Progress{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	control, err := r.controlState(ctx, tx)
	if err != nil {
		return Progress{}, err
	}
	if r.RecordSQL != nil {
		if err := r.RecordSQL(ctx, sp.SQL); err != nil {
			return Progress{}, err
		}
	}
	tag, err := tx.Exec(ctx, sp.SQL)
	if err == nil {
		err = r.checkControl(ctx, tx, control)
	}
	if err != nil {
		return Progress{}, err
	}
	rows := tag.RowsAffected()
	if _, err := tx.Exec(ctx, `INSERT INTO _platform.migration_runs (root_job, op, total_rows, finished_at)
		VALUES ($1, $2, $3, now())`, sp.RootID, sp.Op, rows); err != nil {
		return Progress{}, err
	}
	if err := r.commit(ctx, s, tx, sp, 0); err != nil {
		return Progress{}, err
	}
	return Progress{Batches: 1, Rows: rows, Total: &rows}, nil
}

func (r *Runner) runBatches(ctx context.Context, s *session, sp Spec, rn run, report func(Progress)) (Progress, error) {
	if err := validateCursor(ctx, s.conn, sp); err != nil {
		return Progress{}, err
	}
	replaySQL, trackRows, err := sqlpolicy.BatchReplay(sp.SQL, sp.Column, false)
	if err != nil {
		return Progress{}, ErrInvalidSpec
	}
	tbl, _ := table(sp.Table)
	col := pgx.Identifier{sp.Column}.Sanitize()
	if !rn.exists {
		var maxID, count *int64
		if err := s.conn.QueryRow(ctx, fmt.Sprintf(`SELECT max(%s), count(*) FROM %s`, col, tbl)).Scan(&maxID, &count); err != nil {
			return Progress{}, err
		}
		if err := r.record(ctx, s, sp, `INSERT INTO _platform.migration_runs (root_job, op, scope_max, total_rows, finished_at)
			VALUES ($1, $2, $3, $4, CASE WHEN $3::bigint IS NULL THEN now() END) ON CONFLICT (root_job, op) DO NOTHING`,
			sp.RootID, sp.Op, maxID, count); err != nil {
			return Progress{}, err
		}
		var err error
		if rn, err = loadRun(ctx, s.conn, sp); err != nil {
			return Progress{}, err
		}
		if rn.finished {
			return totals(ctx, s.conn, sp, rn)
		}
	}
	prog, err := totals(ctx, s.conn, sp, rn)
	if err != nil {
		return prog, err
	}
	var last int64
	var batchNo int
	if err := s.conn.QueryRow(ctx, `SELECT coalesce(max(cursor_to), $3), coalesce(max(batch_no), 0) FROM _platform.migration_checkpoints
		WHERE root_job = $1 AND op = $2`, sp.RootID, sp.Op, int64(math.MinInt64)).Scan(&last, &batchNo); err != nil {
		return prog, err
	}
	if rn.scopeMax == nil {

		return prog, fmt.Errorf("%w: run %s tidak punya cakupan batch", ErrInvalidSpec, sp.Op)
	}
	next := fmt.Sprintf(`SELECT max(c) FROM (SELECT %s AS c FROM %s WHERE %s > $1 AND %s <= $2 ORDER BY %s LIMIT $3) s`, col, tbl, col, col, col)
	recordSQL := r.RecordSQL
	for {
		if err := ctx.Err(); err != nil {
			return prog, err
		}
		if r.Revalidate != nil {
			if err := r.Revalidate(ctx); err != nil {
				return prog, err
			}
		}
		var upper *int64
		if err := s.conn.QueryRow(ctx, next, last, *rn.scopeMax, sp.Size).Scan(&upper); err != nil {
			return prog, err
		}
		if upper == nil {
			break
		}
		batchNo++
		bctx, span := telemetry.Tracer().Start(telemetry.WithBatch(ctx, batchNo), "batch.run")
		tx, err := r.begin(bctx, s)
		if err != nil {
			span.End()
			return prog, err
		}
		if _, err := tx.Exec(bctx, "LOCK TABLE "+tbl+" IN SHARE ROW EXCLUSIVE MODE"); err != nil {
			tx.Rollback(context.WithoutCancel(bctx))
			span.End()
			return prog, err
		}
		if err := validateCursor(bctx, tx, sp); err != nil {
			tx.Rollback(context.WithoutCancel(bctx))
			span.End()
			return prog, err
		}
		if err := tx.QueryRow(bctx, next, last, *rn.scopeMax, sp.Size).Scan(&upper); err != nil || upper == nil {
			tx.Rollback(context.WithoutCancel(bctx))
			span.End()
			if err != nil {
				return prog, err
			}
			continue
		}
		control, err := r.controlState(bctx, tx)
		if err != nil {
			tx.Rollback(context.WithoutCancel(bctx))
			span.End()
			return prog, err
		}
		if recordSQL != nil {
			if err := recordSQL(bctx, replaySQL); err != nil {
				tx.Rollback(context.WithoutCancel(bctx))
				span.End()
				return prog, err
			}
			recordSQL = nil
		}
		var affected int64
		var manifest *rowManifest
		if trackRows {
			var captured rowManifest
			captured, err = captureRows(bctx, tx, replaySQL, tbl, last, *upper, sp.Size)
			manifest = &captured
			affected = int64(len(captured.Rows))
		} else {
			var tag pgconn.CommandTag
			tag, err = tx.Exec(bctx, replaySQL, last, *upper)
			affected = tag.RowsAffected()
		}
		if err == nil {
			err = r.checkControl(bctx, tx, control)
		}
		if err == nil && affected > int64(sp.Size) {
			err = fmt.Errorf("%w: jumlah baris melampaui ukuran batch", ErrInvalidSpec)
		}
		if err == nil {
			_, err = tx.Exec(bctx, `INSERT INTO _platform.migration_checkpoints (root_job, op, batch_no, cursor_from, cursor_to, rows_affected,row_manifest)
    VALUES ($1,$2,$3,$4,$5,$6,$7)`, sp.RootID, sp.Op, batchNo, last, *upper, affected, manifest)
		}
		if err != nil {
			tx.Rollback(context.WithoutCancel(bctx))
			span.RecordError(errors.New(telemetry.RedactError(err)))
			span.End()
			return prog, err
		}
		if err := r.commit(bctx, s, tx, sp, batchNo); err != nil {
			span.RecordError(errors.New(telemetry.RedactError(err)))
			span.End()
			return prog, err
		}
		span.End()
		prog.Batches++
		prog.Rows += affected
		last = *upper
		if r.Metrics != nil {
			r.Metrics.BatchCommitted(affected)
		}
		telemetry.Event(bctx, slog.LevelInfo, "batch.committed", "ok", slog.Int64("rows", affected),
			slog.Int64("committed_batches", prog.Batches), slog.Int64("committed_rows", prog.Rows))
		if report != nil {
			report(prog)
		}
	}
	if err := r.record(ctx, s, sp, `UPDATE _platform.migration_runs SET finished_at = now()
		WHERE root_job = $1 AND op = $2 AND finished_at IS NULL`, sp.RootID, sp.Op); err != nil {
		return prog, err
	}
	prog.Total = &prog.Rows
	return prog, nil
}

func (r *Runner) Compensate(ctx context.Context, sp Spec, report func(Progress)) (Progress, error) {
	sp.Op = OpDown
	if sp.Size <= 0 {
		sp.Size = 100000
	}
	if sp.Table == "" || sp.Column == "" {
		var err error
		sp.Table, sp.Column, err = sqlpolicy.BatchTarget(sp.SQL)
		if err != nil {
			return Progress{}, fmt.Errorf("%w: %s", ErrInvalidSpec, err)
		}
	}
	if err := validate(sp); err != nil {
		return Progress{}, err
	}
	s, err := r.hold(ctx)
	if err != nil {
		return Progress{}, err
	}
	defer s.release()
	if r.Revalidate != nil {
		if err := r.Revalidate(ctx); err != nil {
			return Progress{}, err
		}
	}
	if err := ensureControl(ctx, s.conn); err != nil {
		return Progress{}, err
	}
	up := sp
	up.Op = OpUp
	if err := r.verify(ctx, s.conn, up, false); err != nil {
		return Progress{}, err
	}
	if err := r.verify(ctx, s.conn, sp, true); err != nil {
		return Progress{}, err
	}
	if r.Arm != nil {
		if err := r.Arm(ctx); err != nil {
			return Progress{}, err
		}
	}
	upRun, err := loadRun(ctx, s.conn, up)
	if err != nil {
		return Progress{}, err
	}
	if !upRun.exists {
		return Progress{}, fmt.Errorf("%w: tidak ada bukti eksekusi up untuk dibatalkan", ErrUnsafe)
	}
	var upBatches, upRows int64
	if err := s.conn.QueryRow(ctx, `SELECT count(*), coalesce(sum(rows_affected), 0) FROM _platform.migration_checkpoints WHERE root_job = $1 AND op = 'up'`,
		sp.RootID).Scan(&upBatches, &upRows); err != nil {
		return Progress{}, err
	}
	if upBatches == 0 && !(upRun.finished && upRun.total != nil && *upRun.total == 0 && upRun.scopeMax == nil) {

		return Progress{}, fmt.Errorf("%w: eksekusi up tidak punya checkpoint batch", ErrUnsafe)
	}
	if err := r.record(ctx, s, sp, `INSERT INTO _platform.migration_runs (root_job, op, total_rows) VALUES ($1, 'down', $2)
		ON CONFLICT DO NOTHING`, sp.RootID, upRows); err != nil {
		return Progress{}, err
	}
	prog := Progress{Total: &upRows}
	if err := s.conn.QueryRow(ctx, `SELECT count(*), coalesce(sum(rows_affected), 0) FROM _platform.migration_checkpoints
		WHERE root_job = $1 AND op = 'down'`, sp.RootID).Scan(&prog.Batches, &prog.Rows); err != nil {
		return prog, err
	}
	downSQL, tracked, err := sqlpolicy.BatchReplay(sp.SQL, sp.Column, true)
	if err != nil || !tracked {
		return prog, ErrUnsafe
	}
	tbl, _ := table(sp.Table)
	recordSQL := r.RecordSQL
	before := int64(math.MaxInt64)
	for {
		var no int
		var from, to int64
		var manifest *rowManifest
		err := s.conn.QueryRow(ctx, `SELECT u.batch_no, u.cursor_from, u.cursor_to, u.row_manifest FROM _platform.migration_checkpoints u
			WHERE u.root_job = $1 AND u.op = 'up' AND u.batch_no < $2::bigint AND NOT EXISTS (SELECT 1 FROM _platform.migration_checkpoints d
				WHERE d.root_job = u.root_job AND d.op = 'down' AND d.batch_no = u.batch_no)
			ORDER BY u.batch_no DESC LIMIT 1`, sp.RootID, before).Scan(&no, &from, &to, &manifest)
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			return prog, err
		}
		bctx := telemetry.WithBatch(ctx, no)
		if r.Revalidate != nil {
			if err := r.Revalidate(bctx); err != nil {
				return prog, err
			}
		}
		tx, err := r.begin(bctx, s)
		if err != nil {
			return prog, err
		}
		if _, err := tx.Exec(bctx, "LOCK TABLE "+tbl+" IN SHARE ROW EXCLUSIVE MODE"); err != nil {
			tx.Rollback(context.WithoutCancel(ctx))
			return prog, err
		}
		if err := validateCursor(bctx, tx, sp); err != nil {
			tx.Rollback(context.WithoutCancel(ctx))
			return prog, err
		}
		ids, err := matchingRows(bctx, tx, sp, tbl, manifest)
		if err != nil {
			tx.Rollback(context.WithoutCancel(ctx))
			return prog, err
		}
		control, err := r.controlState(bctx, tx)
		if err != nil {
			tx.Rollback(context.WithoutCancel(ctx))
			return prog, err
		}
		if recordSQL != nil {
			if err := recordSQL(bctx, downSQL); err != nil {
				tx.Rollback(context.WithoutCancel(ctx))
				return prog, err
			}
			recordSQL = nil
		}
		tag, err := tx.Exec(bctx, downSQL, from, to, ids)
		if err == nil {
			err = r.checkControl(bctx, tx, control)
		}
		if err == nil && tag.RowsAffected() != int64(len(ids)) {
			err = ErrUnsafe
		}
		if err == nil {
			_, err = tx.Exec(bctx, `INSERT INTO _platform.migration_checkpoints (root_job, op, batch_no, cursor_from, cursor_to, rows_affected)
				VALUES ($1, 'down', $2, $3, $4, $5)`, sp.RootID, no, from, to, tag.RowsAffected())
		}
		if err != nil {
			tx.Rollback(context.WithoutCancel(ctx))
			return prog, err
		}
		if err := r.commit(bctx, s, tx, sp, no); err != nil {
			return prog, err
		}
		before = int64(no)
		prog.Batches++
		prog.Rows += tag.RowsAffected()
		if report != nil {
			report(prog)
		}
	}
	if err := r.record(ctx, s, sp, `UPDATE _platform.migration_runs SET finished_at = now()
		WHERE root_job = $1 AND op = 'down' AND finished_at IS NULL`, sp.RootID); err != nil {
		return prog, err
	}
	return prog, nil
}
